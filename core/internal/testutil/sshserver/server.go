// Package sshserver provides a controlled SSH test server for integration testing.
//
// Copied and adapted from knot/pkg/sshpool/sshpool_test.go
// Source commit: e0b4d51eea6647e192371059381039b99fb301a2
package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"
)

// Server is a controlled SSH test server that listens on loopback.
type Server struct {
	t        *testing.T
	listener net.Listener
	config   *ssh.ServerConfig
	hostKey  ssh.Signer

	mu           sync.Mutex
	connections  map[*ssh.ServerConn]struct{}
	transports   map[net.Conn]struct{}
	goroutines   sync.WaitGroup
	closed       bool
	forwardCount atomic.Int32
	channelCount atomic.Int32
	sessionCount atomic.Int32

	// Recorded session requests, used by tests to assert what the client
	// actually asked the server to do.
	ptyRequests   []PTYRequest
	windowChanges []WindowSize
	envRequests   []EnvRequest
	rejectEnv     bool
	signals       []string
	shell         ShellBehavior
	exec          ExecBehavior
	execCommands  []string

	// closing is closed by Close so a scripted shell blocked on a gate exits
	// instead of keeping Wait blocked forever.
	closing chan struct{}

	// Either a working directory or protocol handlers enable the subsystem.
	sftpRoot     string
	sftpHandlers *sftp.Handlers

	// Control barriers for deterministic testing
	beforeHandshake chan struct{}
	beforeAuth      chan struct{}
	beforePTY       chan struct{}
	beforeExec      chan struct{}
	beforeExit      chan struct{}
	beforeSubsystem chan struct{}
}

// PTYRequest records one accepted pty-req.
type PTYRequest struct {
	Term   string
	Width  uint32
	Height uint32
	Modes  []byte
}

// WindowSize records one window-change request.
type WindowSize struct {
	Width  uint32
	Height uint32
}

// EnvRequest records one accepted env request.
type EnvRequest struct {
	Name  string
	Value string
}

// ShellBehavior programs a shell session. The zero value keeps the default echo
// behaviour; set Scripted to take control of the session.
type ShellBehavior struct {
	// Scripted switches the shell from echo to a programmed session.
	Scripted bool
	// Writes are emitted in order after stdin handling. Each write may be gated,
	// which lets a test attach before output starts or between two phases of
	// output.
	Writes []ScriptedWrite
	// ExitCode is reported through exit-status.
	ExitCode int
	// SendExitStatus false ends the session without an exit status, which is the
	// protocol case a client must report as "exit status missing".
	SendExitStatus bool
	// ExitSignal, when set, is sent as exit-signal instead of exit-status.
	ExitSignal string
	// WritesDone, when non-nil, is closed once every scheduled write has been
	// sent, which lets a test attach only after the scripted output exists.
	WritesDone chan struct{}
	// BeforeExit, when non-nil, is waited on after writing output and before
	// reporting the exit status.
	BeforeExit chan struct{}
	// ReadStdin keeps draining client input until EOF before writing output.
	// Without it the shell writes immediately and exits.
	ReadStdin bool
}

// ScriptedWrite is one planned write to the client.
type ScriptedWrite struct {
	Data      []byte
	Stderr    bool
	ChunkSize int
	// Gate, when non-nil, is waited on before this write is sent.
	Gate chan struct{}
	// FirstChunkSent closes after the first successful write of an exec stream.
	FirstChunkSent chan struct{}
}

// ExecBehavior scripts one command. Done closes after the remote worker exits;
// Started closes once exec has been accepted. IgnoreSignal exercises channel
// close as the cancellation path rather than relying on process signals.
type ExecBehavior struct {
	Scripted       bool
	Writes         []ScriptedWrite
	ExitCode       int
	SendExitStatus bool
	Started        chan struct{}
	Done           chan struct{}
	BeforeExit     chan struct{}
	IgnoreSignal   bool
}

func (s *Server) SetExecBehavior(behavior ExecBehavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = behavior
}

func (s *Server) ExecCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.execCommands...)
}

// Config holds server configuration.
type Config struct {
	// RejectEnv simulates an SSH server without an AcceptEnv allowlist.
	RejectEnv         bool
	User              string
	Password          string
	PublicKeyCallback func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error)
	// SFTPRoot, when set, enables a real SFTP subsystem whose working directory
	// is that path. With neither SFTPRoot nor SFTPHandlers, subsystem requests
	// are refused, letting tests exercise subsystem setup failures.
	SFTPRoot string
	// SFTPHandlers enables a request-based subsystem instead of the filesystem
	// server. Tests can wrap protocol handlers to control individual I/O calls;
	// remote paths then stay independent of the host OS filesystem.
	// Configure handlers before starting clients. Each subsystem copies the
	// interfaces but shares the referenced implementations (and remote files).
	SFTPHandlers *sftp.Handlers
}

// New creates a new test SSH server listening on a random loopback port.
func New(t *testing.T, cfg Config) *Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	return newServer(t, cfg, listener)
}

func newServer(t *testing.T, cfg Config, listener net.Listener) *Server {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create host signer: %v", err)
	}

	serverConfig := &ssh.ServerConfig{}
	serverConfig.AddHostKey(signer)

	srv := &Server{
		t:            t,
		listener:     listener,
		config:       serverConfig,
		hostKey:      signer,
		connections:  make(map[*ssh.ServerConn]struct{}),
		transports:   make(map[net.Conn]struct{}),
		closing:      make(chan struct{}),
		sftpRoot:     cfg.SFTPRoot,
		sftpHandlers: cfg.SFTPHandlers,
		rejectEnv:    cfg.RejectEnv,
	}

	// The auth callbacks consult the server's barrier, so they are installed
	// after the server exists. Waiting inside the callback is what lets a test
	// hold a connection in the authentication stage and then release it.
	if cfg.Password != "" {
		serverConfig.PasswordCallback = func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if !srv.waitBarrier(func(s *Server) chan struct{} { return s.beforeAuth }) {
				return nil, net.ErrClosed
			}
			if meta.User() != cfg.User || string(pass) != cfg.Password {
				return nil, fmt.Errorf("unauthorized")
			}
			return nil, nil
		}
	}

	if cfg.PublicKeyCallback != nil {
		accept := cfg.PublicKeyCallback
		serverConfig.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !srv.waitBarrier(func(s *Server) chan struct{} { return s.beforeAuth }) {
				return nil, net.ErrClosed
			}
			return accept(meta, key)
		}
	}

	srv.goroutines.Add(1)
	go srv.serve()

	t.Cleanup(func() {
		srv.Close()
		srv.Wait()
	})

	return srv
}

// Addr returns the server's listening address.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Host returns the host part of the address.
func (s *Server) Host() string {
	host, _, _ := net.SplitHostPort(s.Addr())
	return host
}

// Port returns the port number.
func (s *Server) Port() int {
	_, portStr, _ := net.SplitHostPort(s.Addr())
	port, _ := strconv.Atoi(portStr)
	return port
}

// HostKey returns the server's host key.
func (s *Server) HostKey() ssh.PublicKey {
	return s.hostKey.PublicKey()
}

// DropConnections closes every established connection without shutting the
// listener down, which simulates a network failure mid-session.
func (s *Server) DropConnections() {
	s.mu.Lock()
	conns := make([]*ssh.ServerConn, 0, len(s.connections))
	for conn := range s.connections {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// PTYRequests returns every pty-req the server accepted, in order.
func (s *Server) PTYRequests() []PTYRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PTYRequest(nil), s.ptyRequests...)
}

// WindowChanges returns every window-change request, in order.
func (s *Server) WindowChanges() []WindowSize {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WindowSize(nil), s.windowChanges...)
}

// EnvRequests returns every accepted env request, in order.
func (s *Server) EnvRequests() []EnvRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]EnvRequest(nil), s.envRequests...)
}

// Signals returns every signal request, in order.
func (s *Server) Signals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signals...)
}

// SetShellBehavior programs what a shell session does. It must be called before
// the session starts. The zero value keeps the default echo behaviour.
func (s *Server) SetShellBehavior(behavior ShellBehavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.shell = behavior
}

// Close stops accepting new connections and closes existing ones.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.closing)
	_ = s.listener.Close()

	conns := make([]*ssh.ServerConn, 0, len(s.connections))
	for conn := range s.connections {
		conns = append(conns, conn)
	}
	transports := make([]net.Conn, 0, len(s.transports))
	for conn := range s.transports {
		transports = append(transports, conn)
	}
	s.mu.Unlock()

	for _, conn := range transports {
		_ = conn.Close()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// Wait waits for all server goroutines to exit.
func (s *Server) Wait() {
	s.goroutines.Wait()
}

// ForwardCount returns the number of direct-tcpip channels accepted.
func (s *Server) ForwardCount() int32 {
	return s.forwardCount.Load()
}

// ChannelCount returns the total number of channels accepted.
func (s *Server) ChannelCount() int32 {
	return s.channelCount.Load()
}

// SessionCount returns the number of session channels accepted.
func (s *Server) SessionCount() int32 {
	return s.sessionCount.Load()
}

func (s *Server) serve() {
	defer s.goroutines.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		s.goroutines.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.goroutines.Done()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.transports[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.transports, conn)
		s.mu.Unlock()
	}()

	if !s.waitBarrier(func(s *Server) chan struct{} { return s.beforeHandshake }) {
		return
	}

	serverConn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		_ = conn.Close()
		return
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = serverConn.Close()
		return
	}
	s.connections[serverConn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.connections, serverConn)
		s.mu.Unlock()
		_ = serverConn.Close()
	}()

	s.goroutines.Add(1)
	go func() {
		defer s.goroutines.Done()
		ssh.DiscardRequests(reqs)
	}()

	for newCh := range chans {
		s.channelCount.Add(1)

		switch newCh.ChannelType() {
		case "session":
			s.sessionCount.Add(1)
			s.goroutines.Add(1)
			go s.handleSession(newCh)
		case "direct-tcpip":
			s.forwardCount.Add(1)
			s.goroutines.Add(1)
			go s.handleDirectTCPIP(newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
}

type directTCPIPReq struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

func (s *Server) handleDirectTCPIP(newCh ssh.NewChannel) {
	defer s.goroutines.Done()

	var req directTCPIPReq
	if err := ssh.Unmarshal(newCh.ExtraData(), &req); err != nil {
		_ = newCh.Reject(ssh.UnknownChannelType, "invalid direct-tcpip payload")
		return
	}

	channel, requests, err := newCh.Accept()
	if err != nil {
		return
	}
	defer channel.Close()

	s.goroutines.Add(1)
	go func() {
		defer s.goroutines.Done()
		ssh.DiscardRequests(requests)
	}()

	targetConn, err := net.Dial("tcp", net.JoinHostPort(req.DestAddr, strconv.Itoa(int(req.DestPort))))
	if err != nil {
		return
	}
	defer targetConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetConn, channel)
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(channel, targetConn)
	}()

	wg.Wait()
}

func (s *Server) handleSession(newCh ssh.NewChannel) {
	defer s.goroutines.Done()

	channel, requests, err := newCh.Accept()
	if err != nil {
		return
	}
	defer channel.Close()

	sess := &sessionHandler{
		server:  s,
		channel: channel,
		closed:  make(chan struct{}),
	}
	defer close(sess.closed)

	for req := range requests {
		switch req.Type {
		case "pty-req":
			if err := sess.handlePTY(req); err != nil {
				_ = req.Reply(false, nil)
			} else {
				_ = req.Reply(true, nil)
			}
		case "shell":
			_ = req.Reply(true, nil)
			s.goroutines.Add(1)
			go sess.runShell()
		case "exec":
			_ = req.Reply(true, nil)
			s.goroutines.Add(1)
			go sess.runExec(req)
		case "window-change":
			sess.handleWindowChange(req)
			_ = req.Reply(true, nil)
		case "env":
			var envReq struct {
				Name  string
				Value string
			}
			if err := ssh.Unmarshal(req.Payload, &envReq); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.envRequests = append(s.envRequests, EnvRequest{Name: envReq.Name, Value: envReq.Value})
			s.mu.Unlock()
			_ = req.Reply(!s.rejectEnv, nil)
		case "signal":
			var signalReq struct {
				Signal string
			}
			if err := ssh.Unmarshal(req.Payload, &signalReq); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.signals = append(s.signals, signalReq.Signal)
			ignore := s.exec.IgnoreSignal
			s.mu.Unlock()
			_ = req.Reply(!ignore, nil)
		case "subsystem":
			if err := sess.handleSubsystem(req); err != nil {
				_ = req.Reply(false, nil)
			} else {
				_ = req.Reply(true, nil)
			}
		default:
			_ = req.Reply(false, nil)
		}
	}
}

type sessionHandler struct {
	server  *Server
	channel ssh.Channel
	closed  chan struct{}

	mu     sync.Mutex
	pty    *ptyRequest
	width  uint32
	height uint32
}

type ptyRequest struct {
	Term      string
	Width     uint32
	Height    uint32
	PixWidth  uint32
	PixHeight uint32
	Modes     string
}

func (h *sessionHandler) handlePTY(req *ssh.Request) error {
	var ptyReq ptyRequest
	if err := ssh.Unmarshal(req.Payload, &ptyReq); err != nil {
		return err
	}

	h.mu.Lock()
	h.pty = &ptyReq
	h.width = ptyReq.Width
	h.height = ptyReq.Height
	h.mu.Unlock()

	h.server.mu.Lock()
	h.server.ptyRequests = append(h.server.ptyRequests, PTYRequest{
		Term:   ptyReq.Term,
		Width:  ptyReq.Width,
		Height: ptyReq.Height,
		Modes:  []byte(ptyReq.Modes),
	})
	h.server.mu.Unlock()

	return nil
}

func (h *sessionHandler) handleWindowChange(req *ssh.Request) {
	var change struct {
		Width     uint32
		Height    uint32
		PixWidth  uint32
		PixHeight uint32
	}
	if err := ssh.Unmarshal(req.Payload, &change); err != nil {
		return
	}

	h.mu.Lock()
	h.width = change.Width
	h.height = change.Height
	h.mu.Unlock()

	h.server.mu.Lock()
	h.server.windowChanges = append(h.server.windowChanges, WindowSize{Width: change.Width, Height: change.Height})
	h.server.mu.Unlock()
}

// CurrentSize returns the last size the client requested.
func (h *sessionHandler) CurrentSize() (uint32, uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.width, h.height
}

type subsystemRequest struct {
	Name string
}

func (h *sessionHandler) handleSubsystem(req *ssh.Request) error {
	var subsys subsystemRequest
	if err := ssh.Unmarshal(req.Payload, &subsys); err != nil {
		return err
	}

	if subsys.Name == "sftp" {
		if h.server.sftpRoot == "" && h.server.sftpHandlers == nil {
			return fmt.Errorf("sftp subsystem is not enabled on this test server")
		}
		// Hold the reply so a client that gives up waiting for the subsystem can
		// be exercised without a real stalled network.
		if !h.server.waitBarrier(func(s *Server) chan struct{} { return s.beforeSubsystem }) {
			return net.ErrClosed
		}
		h.server.goroutines.Add(1)
		go h.runSFTP()
		return nil
	}

	return fmt.Errorf("unsupported subsystem: %s", subsys.Name)
}

func (h *sessionHandler) runShell() {
	defer h.server.goroutines.Done()
	// A real sshd closes the session channel once the shell exits, which is what
	// lets the client's session Wait observe the exit status and end of output.
	defer h.channel.Close()

	h.server.mu.Lock()
	behavior := h.server.shell
	beforePTY := h.server.beforePTY
	h.server.mu.Unlock()

	if beforePTY != nil {
		select {
		case <-beforePTY:
		case <-h.server.closing:
			return
		}
	}

	if !behavior.Scripted {
		// Default: a simple echo shell, so a client can round-trip input.
		_, _ = io.Copy(h.channel, h.channel)
		_, _ = h.channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
		return
	}

	if behavior.ReadStdin {
		_, _ = io.Copy(io.Discard, h.channel)
	}

	for _, write := range behavior.Writes {
		if write.Gate != nil {
			select {
			case <-write.Gate:
			case <-h.server.closing:
				return
			}
		}
		if len(write.Data) == 0 {
			continue
		}
		if write.Stderr {
			writeChunked(h.channel.Stderr(), write.Data, write.ChunkSize)
			continue
		}
		writeChunked(h.channel, write.Data, write.ChunkSize)
	}

	if behavior.WritesDone != nil {
		close(behavior.WritesDone)
	}

	if behavior.BeforeExit != nil {
		select {
		case <-behavior.BeforeExit:
		case <-h.server.closing:
			return
		}
	}

	h.sendExit(behavior)
}

// writeChunked writes data in chunkSize pieces (or one write when chunkSize is
// zero) so a test can force many frames.
func writeChunked(w io.Writer, data []byte, chunkSize int) {
	if chunkSize <= 0 || chunkSize >= len(data) {
		_, _ = w.Write(data)
		return
	}
	for start := 0; start < len(data); start += chunkSize {
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[start:end]); err != nil {
			return
		}
	}
}

// sendExit reports the session outcome according to the behaviour.
func (h *sessionHandler) sendExit(behavior ShellBehavior) {
	if behavior.ExitSignal != "" {
		_, _ = h.channel.SendRequest("exit-signal", false, ssh.Marshal(struct {
			Signal     string
			CoreDumped bool
			Error      string
			Lang       string
		}{Signal: behavior.ExitSignal}))
		return
	}
	if !behavior.SendExitStatus {
		// Ending the channel without exit-status is legal and must be reported by
		// the client as a missing exit status rather than a success.
		return
	}
	_, _ = h.channel.SendRequest("exit-status", false, ssh.Marshal(struct {
		Status uint32
	}{Status: uint32(behavior.ExitCode)}))
}

type execRequest struct {
	Command string
}

func (h *sessionHandler) runExec(req *ssh.Request) {
	defer h.server.goroutines.Done()
	defer h.channel.Close()
	h.server.mu.Lock()
	behavior := h.server.exec
	beforeExec := h.server.beforeExec
	beforeExit := h.server.beforeExit
	h.server.mu.Unlock()
	if behavior.Done != nil {
		defer close(behavior.Done)
	}

	var execReq execRequest
	if err := ssh.Unmarshal(req.Payload, &execReq); err != nil {
		_, _ = h.channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 1}))
		return
	}

	if !h.waitExecGate(beforeExec) {
		return
	}
	h.server.mu.Lock()
	h.server.execCommands = append(h.server.execCommands, execReq.Command)
	h.server.mu.Unlock()
	if behavior.Started != nil {
		close(behavior.Started)
	}
	if !behavior.Scripted {
		_, _ = h.channel.Write([]byte(execReq.Command + "\n"))
		behavior.SendExitStatus = true
	} else {
		for _, write := range behavior.Writes {
			if !h.waitExecGate(write.Gate) {
				return
			}
			writer := io.Writer(h.channel)
			if write.Stderr {
				writer = h.channel.Stderr()
			}
			if !writeExecChunks(writer, write.Data, write.ChunkSize, write.FirstChunkSent) {
				return
			}
		}
	}
	if !h.waitExecGate(behavior.BeforeExit) {
		return
	}
	if !h.waitExecGate(beforeExit) {
		return
	}
	if behavior.SendExitStatus {
		_, _ = h.channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: uint32(behavior.ExitCode)}))
	}
}

func (h *sessionHandler) waitExecGate(gate <-chan struct{}) bool {
	if gate == nil {
		return true
	}
	select {
	case <-gate:
		return true
	case <-h.closed:
		return false
	case <-h.server.closing:
		return false
	}
}

func writeExecChunks(w io.Writer, data []byte, size int, firstChunk chan struct{}) bool {
	if size <= 0 {
		size = len(data)
	}
	for len(data) > 0 {
		n := min(size, len(data))
		written, err := w.Write(data[:n])
		if err != nil || written != n {
			return false
		}
		if firstChunk != nil {
			close(firstChunk)
			firstChunk = nil
		}
		data = data[n:]
	}
	return true
}

func (h *sessionHandler) runSFTP() {
	defer h.server.goroutines.Done()
	defer h.channel.Close()
	if h.server.sftpHandlers != nil {
		server := sftp.NewRequestServer(h.channel, *h.server.sftpHandlers)
		defer server.Close()
		_ = server.Serve()
		return
	}

	// A real SFTP subsystem, so a client that opens one is exercised against the
	// actual protocol instead of a stub that only accepts the request.
	server, err := sftp.NewServer(h.channel, sftp.WithServerWorkingDirectory(h.server.sftpRoot))
	if err != nil {
		return
	}
	defer server.Close()
	// Serve returns when the client closes the channel or the subsystem stops.
	_ = server.Serve()
}

// Done closes when the fixture shuts down. Custom blocking SFTP handlers must
// observe it so Close followed by Wait cannot leave a protocol worker parked.
func (s *Server) Done() <-chan struct{} { return s.closing }

// waitBarrier blocks until a gate opens or Close stops the fixture. False means
// setup must stop. A test may install a gate after startup, so read it under mu.
func (s *Server) waitBarrier(pick func(*Server) chan struct{}) bool {
	s.mu.Lock()
	barrier := pick(s)
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return false
	}
	if barrier == nil {
		return true
	}
	select {
	case <-barrier:
		// A released gate must not resume setup after Close won the race.
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.closed
	case <-s.closing:
		return false
	}
}

// SetHandshakeBarrier sets a barrier before SSH handshake.
func (s *Server) SetHandshakeBarrier(barrier chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeHandshake = barrier
}

// SetAuthBarrier sets a barrier before authentication response.
func (s *Server) SetAuthBarrier(barrier chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeAuth = barrier
}

// SetPTYBarrier sets a barrier before PTY output.
func (s *Server) SetPTYBarrier(barrier chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforePTY = barrier
}

// SetExecBarrier sets a barrier before exec starts.
func (s *Server) SetExecBarrier(barrier chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeExec = barrier
}

// SetExitBarrier sets a barrier before exit-status is sent.
func (s *Server) SetExitBarrier(barrier chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeExit = barrier
}

// SetSubsystemBarrier sets a barrier before an accepted SFTP subsystem reply is
// sent. The request is held until the test releases it, so a client that stops
// waiting for the subsystem can be exercised deterministically.
func (s *Server) SetSubsystemBarrier(barrier chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeSubsystem = barrier
}
