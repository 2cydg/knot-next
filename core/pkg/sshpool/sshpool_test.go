package sshpool

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-core/pkg/config"

	"golang.org/x/crypto/ssh"
)

type testSSHServer struct {
	config       *ssh.ServerConfig
	acceptCount  atomic.Int32
	globalReqs   atomic.Int32
	conns        sync.Map
	closeOnReq   bool
	connectedCh  chan struct{}
	disconnectCh chan struct{}
}

func startTestSSHServer(t *testing.T, closeOnReq bool) *testSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("create host signer: %v", err)
	}
	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if meta.User() != "tester" || string(pass) != "secret" {
				return nil, errors.New("unauthorized")
			}
			return nil, nil
		},
	}
	serverConfig.AddHostKey(signer)
	srv := &testSSHServer{
		config:       serverConfig,
		closeOnReq:   closeOnReq,
		connectedCh:  make(chan struct{}),
		disconnectCh: make(chan struct{}),
	}
	return srv
}

func (s *testSSHServer) addr() string {
	return "127.0.0.1:22"
}

func (s *testSSHServer) close() {
	s.conns.Range(func(key any, _ any) bool {
		_ = key.(net.Conn).Close()
		return true
	})
}

func (s *testSSHServer) closeClients() {
	s.conns.Range(func(key any, _ any) bool {
		_ = key.(net.Conn).Close()
		return true
	})
}

func (s *testSSHServer) handleConn(conn net.Conn) {
	defer s.conns.Delete(conn)
	serverConn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		_ = conn.Close()
		return
	}
	s.signal(s.connectedCh)
	defer func() {
		_ = serverConn.Close()
		s.signal(s.disconnectCh)
	}()
	go func() {
		for req := range reqs {
			s.globalReqs.Add(1)
			if s.closeOnReq {
				_ = serverConn.Close()
				return
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()
	for newCh := range chans {
		_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
	}
}

func (s *testSSHServer) newClient(t *testing.T, server config.ServerProfile) (*ssh.Client, error) {
	t.Helper()
	clientConn, serverConn := memoryPipe()
	s.conns.Store(serverConn, struct{}{})
	s.acceptCount.Add(1)
	go s.handleConn(serverConn)
	clientConfig := &ssh.ClientConfig{
		User:            server.User,
		Auth:            []ssh.AuthMethod{ssh.Password(server.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	}
	conn, chans, reqs, err := ssh.NewClientConn(clientConn, s.addr(), clientConfig)
	if err != nil {
		_ = clientConn.Close()
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

type memoryConn struct {
	in     chan []byte
	peer   *memoryConn
	buf    []byte
	done   chan struct{}
	once   *sync.Once
	closed atomic.Bool
}

func memoryPipe() (net.Conn, net.Conn) {
	once := &sync.Once{}
	done := make(chan struct{})
	a := &memoryConn{in: make(chan []byte, 1024), done: done, once: once}
	b := &memoryConn{in: make(chan []byte, 1024), done: done, once: once}
	a.peer = b
	b.peer = a
	return a, b
}

func (c *memoryConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		select {
		case data := <-c.in:
			c.buf = data
		case <-c.done:
			return 0, io.EOF
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *memoryConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	data := append([]byte(nil), p...)
	select {
	case c.peer.in <- data:
		return len(p), nil
	case <-c.done:
		return 0, io.ErrClosedPipe
	}
}

func (c *memoryConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.once.Do(func() {
		close(c.done)
	})
	return nil
}

func (c *memoryConn) LocalAddr() net.Addr {
	return memoryAddr("local")
}

func (c *memoryConn) RemoteAddr() net.Addr {
	return memoryAddr("remote")
}

func (c *memoryConn) SetDeadline(time.Time) error {
	return nil
}

func (c *memoryConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *memoryConn) SetWriteDeadline(time.Time) error {
	return nil
}

type memoryAddr string

func (a memoryAddr) Network() string {
	return "memory"
}

func (a memoryAddr) String() string {
	return string(a)
}

func (s *testSSHServer) signal(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func testServerProfile(t *testing.T, srv *testSSHServer) config.ServerProfile {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return config.ServerProfile{
		ID:             "target",
		Alias:          "Target",
		Host:           host,
		Port:           port,
		User:           "tester",
		Password:       "secret",
		AuthMethod:     config.AuthMethodPassword,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
	}
}

func testRuntimeConfig(server config.ServerProfile) config.RuntimeConfig {
	return config.RuntimeConfig{
		Settings: config.Settings{KeepaliveInterval: "-1s"},
		Servers:  map[string]config.ServerProfile{server.ID: server},
		Proxies:  map[string]config.ProxyProfile{},
		Keys:     map[string]config.KeyMetadata{},
	}
}

func TestGetClientDeduplicatesConcurrentDial(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()
	installTestDial(t, srv)

	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	keysCh := make(chan []string, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, keys, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew})
			if err != nil {
				errs <- err
				return
			}
			keysCh <- keys
		}()
	}
	wg.Wait()
	close(errs)
	close(keysCh)
	for err := range errs {
		t.Fatalf("GetClient failed: %v", err)
	}
	if got := srv.acceptCount.Load(); got != 1 {
		t.Fatalf("expected one SSH dial, got %d", got)
	}
	for keys := range keysCh {
		if len(keys) != 1 {
			t.Fatalf("expected one pool key, got %v", keys)
		}
	}
}

func TestKeepaliveIntervalControlsRequests(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	cfg.Settings.KeepaliveInterval = "10ms"
	pool := NewPool()
	defer pool.CloseAll()
	installTestDial(t, srv)

	if _, _, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew}); err != nil {
		t.Fatalf("GetClient failed: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		return srv.globalReqs.Load() > 0
	})
}

func TestKeepaliveDisabledStillDropsOnDisconnect(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	cfg.Settings.KeepaliveInterval = "0"
	pool := NewPool()
	defer pool.CloseAll()
	installTestDial(t, srv)
	disconnects := make(chan string, 1)
	pool.DisconnectCallback = func(key string) { disconnects <- key }

	client, keys, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew})
	if err != nil {
		t.Fatalf("GetClient failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected one pool key, got %v", keys)
	}
	srv.closeClients()

	select {
	case got := <-disconnects:
		if got != keys[0] {
			t.Fatalf("expected disconnect for %q, got %q", keys[0], got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for disconnect callback")
	}
	waitFor(t, time.Second, func() bool {
		return !pool.IsAlive(keys[0], client) && pool.Count() == 0
	})
}

func TestClearClosesConnectionsAndKeepsPoolUsable(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()
	installTestDial(t, srv)
	disconnects := make(chan string, 2)
	pool.DisconnectCallback = func(key string) { disconnects <- key }

	if _, _, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew}); err != nil {
		t.Fatalf("first GetClient failed: %v", err)
	}
	if got := pool.Clear(); got != 1 {
		t.Fatalf("expected Clear to close one connection, got %d", got)
	}
	select {
	case <-disconnects:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for clear disconnect callback")
	}
	if pool.Count() != 0 {
		t.Fatalf("expected empty pool after Clear, got %d", pool.Count())
	}
	if _, _, created, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew}); err != nil {
		t.Fatalf("second GetClient failed: %v", err)
	} else if !created {
		t.Fatal("expected new connection after Clear")
	}
	if got := srv.acceptCount.Load(); got != 2 {
		t.Fatalf("expected two SSH dials after reconnect, got %d", got)
	}
}

func TestCloseAllClosesPoolAndReportsCount(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	installTestDial(t, srv)

	if _, _, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew}); err != nil {
		t.Fatalf("GetClient failed: %v", err)
	}
	if got := pool.CloseAll(); got != 1 {
		t.Fatalf("expected CloseAll to close one connection, got %d", got)
	}
	if got := pool.CloseAll(); got != 0 {
		t.Fatalf("expected repeated CloseAll to report zero, got %d", got)
	}
	if _, _, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew}); err == nil {
		t.Fatal("expected closed pool to reject GetClient")
	}
}

func TestCleanupIdleSkipsReferencedEntries(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()
	installTestDial(t, srv)
	pool.SetIdleTimeout(time.Nanosecond)

	_, keys, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew})
	if err != nil {
		t.Fatalf("GetClient failed: %v", err)
	}
	pool.IncRef(keys...)
	time.Sleep(time.Millisecond)
	pool.cleanupIdle()
	if got := pool.Count(); got != 1 {
		t.Fatalf("expected referenced entry to remain, got count %d", got)
	}
	pool.DecRef(keys...)
	time.Sleep(time.Millisecond)
	pool.cleanupIdle()
	if got := pool.Count(); got != 0 {
		t.Fatalf("expected idle unreferenced entry to be cleaned, got count %d", got)
	}
}

func TestStatsIncludeEntryMetadata(t *testing.T) {
	srv := startTestSSHServer(t, false)
	defer srv.close()
	server := testServerProfile(t, srv)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()
	installTestDial(t, srv)

	_, keys, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew})
	if err != nil {
		t.Fatalf("GetClient failed: %v", err)
	}
	stats := pool.Stats()
	if len(stats) != 1 {
		t.Fatalf("expected one stat, got %d", len(stats))
	}
	stat := stats[0]
	if stat.Key != keys[0] || stat.ServerID != server.ID || stat.Alias != server.Alias || stat.Host != server.Host {
		t.Fatalf("unexpected stat metadata: %+v", stat)
	}
	if len(stat.ChainKeys) != 1 || stat.ChainKeys[0] != keys[0] {
		t.Fatalf("unexpected chain keys: %+v", stat.ChainKeys)
	}
}

func TestDialErrorDoesNotPoisonSingleflightCache(t *testing.T) {
	server := config.ServerProfile{
		ID:             "target",
		Alias:          "Target",
		Host:           "127.0.0.1",
		Port:           1,
		User:           "tester",
		AuthMethod:     config.AuthMethodPassword,
		Password:       "secret",
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
	}
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()
	srv := startTestSSHServer(t, false)
	defer srv.close()
	var reject atomic.Bool
	dialMu.Lock()
	orig := dialClient
	dialClient = func(server config.ServerProfile, cfg config.RuntimeConfig, jump *ssh.Client, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, error) {
		if reject.Load() {
			return nil, errors.New("dial failed")
		}
		return srv.newClient(t, server)
	}
	dialMu.Unlock()
	t.Cleanup(func() {
		dialMu.Lock()
		dialClient = orig
		dialMu.Unlock()
	})

	reject.Store(true)
	if _, _, _, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew, Timeout: time.Millisecond}); err == nil {
		t.Fatal("expected first dial to fail")
	}
	reject.Store(false)
	cfg = testRuntimeConfig(server)
	if _, _, created, err := pool.GetClient(server, cfg, nil, DialOptions{HostKeyPolicy: HostKeyPolicyAcceptNew}); err != nil {
		t.Fatalf("second dial should succeed: %v", err)
	} else if !created {
		t.Fatal("expected successful retry to create connection")
	}
}

func TestAppendKnownHostConcurrentWrites(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}

	knownHostsPath := filepath.Join(t.TempDir(), "nested", "known_hosts")
	const hosts = 16
	var wg sync.WaitGroup
	errCh := make(chan error, hosts)
	for i := range hosts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- appendKnownHost(knownHostsPath, fmt.Sprintf("host-%d.example.invalid", i), signer.PublicKey())
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("appendKnownHost failed: %v", err)
		}
	}
	if _, err := os.Stat(knownHostsPath); err != nil {
		t.Fatalf("expected known_hosts file: %v", err)
	}
}

var dialMu sync.Mutex

func installTestDial(t *testing.T, srv *testSSHServer) {
	t.Helper()
	dialMu.Lock()
	orig := dialClient
	dialClient = func(server config.ServerProfile, cfg config.RuntimeConfig, jump *ssh.Client, confirm func(HostKeyPrompt) bool, opts DialOptions) (*ssh.Client, error) {
		if jump != nil {
			return nil, errors.New("jump clients are not supported in this test")
		}
		return srv.newClient(t, server)
	}
	dialMu.Unlock()
	t.Cleanup(func() {
		dialMu.Lock()
		dialClient = orig
		dialMu.Unlock()
	})
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
