package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var (
	ErrNotFound   = errors.New("session not found")
	ErrValidation = errors.New("session validation failed")
	ErrConflict   = errors.New("session conflict")
)

const (
	maxSessions            = 1024
	maxExecs               = 4096
	challengeTimeout       = 60 * time.Second
	maxAuthRetryCount      = 3
	maxSessionSubscribers  = 16
	maxSessionCWDFollowers = 8
)

type configWriter interface {
	RuntimeConfig() (config.RuntimeConfig, error)
	SetServerPassword(id string, password string) (config.ServerProfileView, error)
	UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error)
}

type Service struct {
	mu       sync.RWMutex
	nextID   int64
	sessions map[string]*resource
	execs    map[string]Exec
	config   configWriter
	pool     *sshpool.Pool
	dial     DialOptions
	testMode bool
	onEvent  func(Event)
}

func NewService() *Service {
	return &Service{
		nextID:   1,
		sessions: map[string]*resource{},
		execs:    map[string]Exec{},
	}
}

type DialOptions struct {
	AgentSocket string
	Timeout     time.Duration
}

func (s *Service) UseConfig(configService configWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = configService
}

func (s *Service) UsePool(pool *sshpool.Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pool = pool
}

func (s *Service) UseDialOptions(opts DialOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dial = opts
}

func (s *Service) UseLocalTestBackend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.testMode = true
}

func (s *Service) OnEvent(callback func(Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEvent = callback
}

type CreateRequest struct {
	ServerRef      string            `json:"server_ref"`
	Alias          string            `json:"alias,omitempty"`
	Term           string            `json:"term,omitempty"`
	Rows           int               `json:"rows,omitempty"`
	Cols           int               `json:"cols,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ForwardAgent   bool              `json:"forward_agent,omitempty"`
	HostKeyPolicy  string            `json:"host_key_policy,omitempty"`
	AllowAuthRetry bool              `json:"allow_auth_retry,omitempty"`
}

type ListOptions struct {
	ServerRef string
	Alias     string
	State     string
}

type Resource struct {
	ID              string            `json:"id"`
	ServerRef       string            `json:"server_ref"`
	Alias           string            `json:"alias,omitempty"`
	State           string            `json:"state"`
	Term            string            `json:"term,omitempty"`
	Rows            int               `json:"rows,omitempty"`
	Cols            int               `json:"cols,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	ForwardAgent    bool              `json:"forward_agent,omitempty"`
	HostKeyPolicy   string            `json:"host_key_policy,omitempty"`
	CurrentDir      string            `json:"current_dir,omitempty"`
	CWDUpdatedAt    *time.Time        `json:"cwd_updated_at,omitempty"`
	Attached        bool              `json:"attached"`
	StartedAt       time.Time         `json:"started_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	ExitedAt        *time.Time        `json:"exited_at,omitempty"`
	ExitCode        *int              `json:"exit_code,omitempty"`
	FrameworkError  string            `json:"framework_error,omitempty"`
	AttachURL       string            `json:"attach_url"`
	EventsURL       string            `json:"events_url"`
	HostKeyPending  bool              `json:"host_key_pending"`
	AuthPending     bool              `json:"auth_pending"`
	DisconnectCause string            `json:"disconnect_cause,omitempty"`
}

type pendingChallenge struct {
	Challenge Challenge
	response  chan ChallengeResponse
}

type resource struct {
	Resource
	serverID         string
	hostKeyChallenge *pendingChallenge
	authChallenge    *pendingChallenge
	authRetryCount   int
	authResponse     *ChallengeResponse
	subscribers      map[chan Event]struct{}
	cwdSubscribers   map[chan CWDNotify]struct{}
	backend          *interactiveBackend
	poolKeys         []string
	cancel           context.CancelFunc
}

type interactiveBackend struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	stderr  io.Reader
	done    chan error
	mu      sync.Mutex
}

type Event struct {
	Type      string     `json:"type"`
	SessionID string     `json:"session_id"`
	State     string     `json:"state,omitempty"`
	Rows      int        `json:"rows,omitempty"`
	Cols      int        `json:"cols,omitempty"`
	Path      string     `json:"path,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	Challenge *Challenge `json:"challenge,omitempty"`
	Time      time.Time  `json:"time"`
}

type CWDNotify struct {
	SessionID string    `json:"session_id"`
	Path      string    `json:"path,omitempty"`
	Closed    bool      `json:"closed,omitempty"`
	Time      time.Time `json:"time"`
}

type ExecRequest struct {
	ServerRef     string `json:"server_ref"`
	Command       string `json:"command"`
	TimeoutMS     int64  `json:"timeout_ms,omitempty"`
	HostKeyPolicy string `json:"host_key_policy,omitempty"`
}

type Exec struct {
	ID             string    `json:"id"`
	ServerRef      string    `json:"server_ref"`
	Command        string    `json:"command"`
	State          string    `json:"state"`
	ExitCode       int       `json:"exit_code"`
	Stdout         string    `json:"stdout"`
	Stderr         string    `json:"stderr"`
	FrameworkError string    `json:"framework_error,omitempty"`
	Truncated      bool      `json:"truncated"`
	StartedAt      time.Time `json:"started_at"`
	CompletedAt    time.Time `json:"completed_at"`
}

type ControlRequest struct {
	Type   string `json:"type"`
	Rows   int    `json:"rows,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Signal string `json:"signal,omitempty"`
}

type Challenge struct {
	SessionID      string    `json:"session_id"`
	Type           string    `json:"type"`
	Pending        bool      `json:"pending"`
	Prompt         string    `json:"prompt,omitempty"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
	Host           string    `json:"host,omitempty"`
	KeyType        string    `json:"key_type,omitempty"`
	Risk           string    `json:"risk,omitempty"`
	ServerAlias    string    `json:"server_alias,omitempty"`
	FailedMethod   string    `json:"failed_method,omitempty"`
	AllowedMethods []string  `json:"allowed_methods,omitempty"`
	RetryCount     int       `json:"retry_count,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

type ChallengeResponse struct {
	Accept     bool   `json:"accept,omitempty"`
	Abort      bool   `json:"abort,omitempty"`
	Password   string `json:"password,omitempty"`
	KeyID      string `json:"key_id,omitempty"`
	Remember   bool   `json:"remember,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

type AttachStream struct {
	Cancel func()
	Stdout <-chan []byte
	Stderr <-chan []byte
	Done   <-chan error
	Input  io.WriteCloser
}

func (s *Service) Create(req CreateRequest) (Resource, error) {
	if strings.TrimSpace(req.ServerRef) == "" {
		return Resource{}, fmt.Errorf("%w: server_ref is required", ErrValidation)
	}
	if req.Term == "" {
		req.Term = "xterm-256color"
	}
	if len(req.Term) > 64 {
		return Resource{}, fmt.Errorf("%w: term is too long", ErrValidation)
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	if req.Cols == 0 {
		req.Cols = 80
	}
	if !validTerminalDimensions(req.Rows, req.Cols) {
		return Resource{}, fmt.Errorf("%w: terminal dimensions are invalid", ErrValidation)
	}
	cfgProvider, pool, dialOpts, testMode := s.dependencies()
	if !testMode && cfgProvider == nil {
		return Resource{}, fmt.Errorf("%w: config service is not available", ErrConflict)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= maxSessions {
		return Resource{}, fmt.Errorf("%w: session limit reached", ErrConflict)
	}
	id := strconv.FormatInt(s.nextID, 10)
	s.nextID++
	ctx, cancel := context.WithCancel(context.Background())
	res := Resource{
		ID:            id,
		ServerRef:     req.ServerRef,
		Alias:         req.Alias,
		State:         "connecting",
		Term:          req.Term,
		Rows:          req.Rows,
		Cols:          req.Cols,
		Env:           cloneEnv(req.Env),
		ForwardAgent:  req.ForwardAgent,
		HostKeyPolicy: req.HostKeyPolicy,
		StartedAt:     now,
		UpdatedAt:     now,
		AttachURL:     "/v1/sessions/" + id + "/attach",
		EventsURL:     "/v1/sessions/" + id + "/events",
	}
	session := &resource{
		Resource:       res,
		subscribers:    map[chan Event]struct{}{},
		cwdSubscribers: map[chan CWDNotify]struct{}{},
		cancel:         cancel,
	}
	s.sessions[id] = session
	s.publishSessionLocked(session, Event{Type: "session.created", SessionID: id, State: res.State, Time: now})
	if testMode {
		backend := newLocalInteractiveBackend()
		session.backend = backend
		session.State = "connected"
		session.UpdatedAt = now
		s.publishSessionLocked(session, Event{Type: "session.connected", SessionID: id, State: session.State, Time: now})
		return session.snapshot(), nil
	}
	go s.connectSession(ctx, id, req, cfgProvider, pool, dialOpts, testMode)
	return session.snapshot(), nil
}

func (s *Service) List() []Resource {
	return s.ListWithOptions(ListOptions{})
}

func (s *Service) ListWithOptions(opts ListOptions) []Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Resource, 0, len(s.sessions))
	for _, session := range s.sessions {
		snapshot := session.snapshot()
		if opts.ServerRef != "" && snapshot.ServerRef != opts.ServerRef && snapshot.Alias != opts.ServerRef {
			continue
		}
		if opts.Alias != "" && snapshot.Alias != opts.Alias {
			continue
		}
		if opts.State != "" && snapshot.State != opts.State {
			continue
		}
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

func (s *Service) ActiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, session := range s.sessions {
		if session.State != "closed" && session.State != "failed" {
			count++
		}
	}
	return count
}

func (s *Service) Get(id string) (Resource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	if !ok {
		return Resource{}, ErrNotFound
	}
	return session.snapshot(), nil
}

func (s *Service) Attach(id string) (Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return Resource{}, ErrNotFound
	}
	if session.State == "closed" || session.State == "failed" {
		return Resource{}, fmt.Errorf("%w: closed session cannot be attached", ErrConflict)
	}
	if session.Attached {
		return Resource{}, fmt.Errorf("%w: session is already attached", ErrConflict)
	}
	if session.backend == nil {
		return Resource{}, fmt.Errorf("%w: SSH PTY backend is not connected", ErrConflict)
	}
	now := time.Now().UTC()
	session.Attached = true
	session.State = "attached"
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.attached", SessionID: id, State: session.State, Time: now})
	return session.snapshot(), nil
}

func (s *Service) AttachStream(id string) (AttachStream, Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return AttachStream{}, Resource{}, ErrNotFound
	}
	if session.State == "closed" || session.State == "failed" {
		return AttachStream{}, Resource{}, fmt.Errorf("%w: closed session cannot be attached", ErrConflict)
	}
	if session.Attached {
		return AttachStream{}, Resource{}, fmt.Errorf("%w: session is already attached", ErrConflict)
	}
	if session.backend == nil {
		return AttachStream{}, Resource{}, fmt.Errorf("%w: SSH PTY backend is not connected", ErrConflict)
	}
	now := time.Now().UTC()
	session.Attached = true
	session.State = "attached"
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.attached", SessionID: id, State: session.State, Time: now})
	ctx, cancel := context.WithCancel(context.Background())
	stdout := make(chan []byte, 16)
	stderr := make(chan []byte, 16)
	go readByteStream(ctx, session.backend.stdout, stdout)
	go readByteStream(ctx, session.backend.stderr, stderr)
	return AttachStream{
		Cancel: cancel,
		Stdout: stdout,
		Stderr: stderr,
		Done:   session.backend.done,
		Input:  session.backend.stdin,
	}, session.snapshot(), nil
}

func (s *Service) Detach(id string) (Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return Resource{}, ErrNotFound
	}
	if session.State == "closed" || session.State == "failed" {
		return session.snapshot(), nil
	}
	now := time.Now().UTC()
	session.Attached = false
	if session.State == "attached" {
		session.State = "detached"
	}
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.detached", SessionID: id, State: session.State, Time: now})
	return session.snapshot(), nil
}

func (s *Service) Control(id string, req ControlRequest) (Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return Resource{}, ErrNotFound
	}
	if (session.State == "closed" || session.State == "failed") && req.Type != "disconnect" {
		return Resource{}, fmt.Errorf("%w: closed session cannot be controlled", ErrConflict)
	}
	now := time.Now().UTC()
	switch req.Type {
	case "resize":
		if !validTerminalDimensions(req.Rows, req.Cols) {
			return Resource{}, fmt.Errorf("%w: terminal dimensions are invalid", ErrValidation)
		}
		session.Rows = req.Rows
		session.Cols = req.Cols
		if session.backend != nil {
			if err := session.backend.WindowChange(req.Rows, req.Cols); err != nil {
				s.publishSessionLocked(session, Event{Type: "session.error", SessionID: id, State: session.State, Error: err.Error(), Time: now})
			}
		}
		s.publishSessionLocked(session, Event{Type: "session.resized", SessionID: id, State: session.State, Rows: req.Rows, Cols: req.Cols, Time: now})
	case "detach":
		session.Attached = false
		session.State = "detached"
		s.publishSessionLocked(session, Event{Type: "session.detached", SessionID: id, State: session.State, Time: now})
	case "close_stdin":
		if session.backend != nil && session.backend.stdin != nil {
			_ = session.backend.stdin.Close()
		}
	case "signal":
		switch req.Signal {
		case "HUP", "INT", "KILL", "TERM", "USR1", "USR2":
		default:
			return Resource{}, fmt.Errorf("%w: unsupported signal", ErrValidation)
		}
		if session.backend != nil {
			if err := session.backend.Signal(req.Signal); err != nil {
				s.publishSessionLocked(session, Event{Type: "session.error", SessionID: id, State: session.State, Error: err.Error(), Time: now})
			}
		}
	case "disconnect":
		s.closeSessionLocked(session, "closed", "client requested disconnect", nil, now)
	default:
		return Resource{}, fmt.Errorf("%w: unsupported control type", ErrValidation)
	}
	session.UpdatedAt = now
	return session.snapshot(), nil
}

func (s *Service) Disconnect(id string) (Resource, error) {
	return s.Control(id, ControlRequest{Type: "disconnect"})
}

func (s *Service) DisconnectByPoolKey(poolKey string) []Resource {
	if poolKey == "" {
		return nil
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	var closed []Resource
	for _, session := range s.sessions {
		if session.State == "closed" || session.State == "failed" || !containsString(session.poolKeys, poolKey) {
			continue
		}
		s.closeSessionLocked(session, "closed", "ssh pool disconnected", nil, now)
		closed = append(closed, session.snapshot())
	}
	return closed
}

func (s *Service) Subscribe(id string) (<-chan Event, func(), Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return nil, nil, Resource{}, ErrNotFound
	}
	ch := make(chan Event, maxSessionSubscribers)
	session.subscribers[ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.sessions[id]; ok {
			if _, ok := current.subscribers[ch]; ok {
				delete(current.subscribers, ch)
				close(ch)
			}
		}
	}
	return ch, cancel, session.snapshot(), nil
}

func (s *Service) SubscribeCWD(id string) (<-chan CWDNotify, func(), Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return nil, nil, Resource{}, ErrNotFound
	}
	if session.State == "closed" || session.State == "failed" {
		return nil, nil, Resource{}, fmt.Errorf("%w: closed session cannot be followed", ErrConflict)
	}
	ch := make(chan CWDNotify, maxSessionCWDFollowers)
	session.cwdSubscribers[ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.sessions[id]; ok {
			if _, ok := current.cwdSubscribers[ch]; ok {
				delete(current.cwdSubscribers, ch)
				close(ch)
			}
		}
	}
	return ch, cancel, session.snapshot(), nil
}

func (s *Service) Exec(req ExecRequest) (Exec, error) {
	if strings.TrimSpace(req.ServerRef) == "" {
		return Exec{}, fmt.Errorf("%w: server_ref is required", ErrValidation)
	}
	if strings.TrimSpace(req.Command) == "" {
		return Exec{}, fmt.Errorf("%w: command is required", ErrValidation)
	}
	now := time.Now().UTC()
	cfgProvider, pool, dialOpts, testMode := s.dependencies()
	if testMode {
		return s.recordTestExec(req, now), nil
	}
	if cfgProvider == nil {
		return Exec{}, fmt.Errorf("%w: config service is not available", ErrConflict)
	}
	runtimeCfg, err := cfgProvider.RuntimeConfig()
	if err != nil {
		return Exec{}, err
	}
	server, err := resolveServer(runtimeCfg, req.ServerRef)
	if err != nil {
		return Exec{}, err
	}
	s.mu.Lock()
	if len(s.execs) >= maxExecs {
		s.pruneExecsLocked()
	}
	id := "exec_" + strconv.FormatInt(s.nextID, 10)
	s.nextID++
	s.mu.Unlock()
	exec := Exec{ID: id, ServerRef: req.ServerRef, Command: req.Command, State: "running", StartedAt: now}
	exec = runExec(exec, req, server, runtimeCfg, pool, dialOpts)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs[id] = exec
	return exec, nil
}

func (s *Service) pruneExecsLocked() {
	if len(s.execs) < maxExecs {
		return
	}
	type item struct {
		id string
		t  time.Time
	}
	items := make([]item, 0, len(s.execs))
	for id, exec := range s.execs {
		items = append(items, item{id: id, t: exec.CompletedAt})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].t.Before(items[j].t)
	})
	for i := 0; i <= len(items)-maxExecs/2; i++ {
		delete(s.execs, items[i].id)
	}
}

func (s *Service) HostKeyChallenge(id string) (Challenge, error) {
	return s.challenge(id, "host_key")
}

func (s *Service) RespondHostKeyChallenge(id string, resp ChallengeResponse) (Challenge, error) {
	return s.respondChallenge(id, "host_key", resp)
}

func (s *Service) AuthChallenge(id string) (Challenge, error) {
	return s.challenge(id, "auth")
}

func (s *Service) RespondAuthChallenge(id string, resp ChallengeResponse) (Challenge, error) {
	return s.respondChallenge(id, "auth", resp)
}

func (s *Service) challenge(id string, typ string) (Challenge, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	if !ok {
		return Challenge{}, ErrNotFound
	}
	var pending *pendingChallenge
	if typ == "host_key" {
		pending = session.hostKeyChallenge
	} else {
		pending = session.authChallenge
	}
	if pending == nil {
		return Challenge{SessionID: id, Type: typ, Pending: false}, nil
	}
	out := pending.Challenge
	out.Pending = true
	return out, nil
}

func (s *Service) respondChallenge(id string, typ string, resp ChallengeResponse) (Challenge, error) {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return Challenge{}, ErrNotFound
	}
	var pending *pendingChallenge
	if typ == "host_key" {
		pending = session.hostKeyChallenge
	} else {
		pending = session.authChallenge
	}
	if pending == nil {
		s.mu.Unlock()
		return Challenge{SessionID: id, Type: typ, Pending: false}, nil
	}
	challenge := pending.Challenge
	if typ == "host_key" {
		session.hostKeyChallenge = nil
		session.HostKeyPending = false
		if session.State == "host_key_pending" {
			session.State = "connecting"
		}
	} else {
		session.authChallenge = nil
		session.AuthPending = false
		session.authResponse = &resp
		if session.State == "auth_pending" {
			session.State = "connecting"
		}
	}
	session.UpdatedAt = time.Now().UTC()
	s.publishSessionLocked(session, Event{Type: "session.challenge.resolved", SessionID: id, State: session.State, Challenge: &challenge, Time: session.UpdatedAt})
	s.mu.Unlock()
	select {
	case pending.response <- resp:
	default:
	}
	challenge.Pending = false
	challenge.UpdatedAt = time.Now().UTC()
	return challenge, nil
}

func (s *Service) dependencies() (configWriter, *sshpool.Pool, DialOptions, bool) {
	s.mu.RLock()
	if s.pool != nil {
		defer s.mu.RUnlock()
		return s.config, s.pool, s.dial, s.testMode
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pool == nil {
		s.pool = sshpool.NewPool()
	}
	return s.config, s.pool, s.dial, s.testMode
}

func (s *Service) connectSession(ctx context.Context, id string, req CreateRequest, cfgProvider configWriter, pool *sshpool.Pool, dialOpts DialOptions, testMode bool) {
	var runtimeCfg config.RuntimeConfig
	var err error
	runtimeCfg, err = cfgProvider.RuntimeConfig()
	if err != nil {
		s.failSession(id, err, config.RuntimeConfig{}, "failed")
		return
	}
	server, err := resolveServer(runtimeCfg, req.ServerRef)
	if err != nil {
		s.failSession(id, err, runtimeCfg, "failed")
		return
	}
	if req.Alias == "" {
		req.Alias = server.Alias
		s.mu.Lock()
		if session, ok := s.sessions[id]; ok {
			session.Alias = server.Alias
			session.serverID = server.ID
		}
		s.mu.Unlock()
	}
	backend, poolKeys, finalCfg, err := s.openInteractive(ctx, id, req, server, runtimeCfg, pool, dialOpts)
	if err != nil {
		s.failSession(id, err, finalCfg, "failed")
		return
	}
	s.attachBackend(id, backend, poolKeys)
	go s.watchInteractive(id, backend, pool, poolKeys, finalCfg)
}

func (s *Service) attachBackend(id string, backend *interactiveBackend, poolKeys []string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		if backend != nil {
			_ = backend.Close()
		}
		return
	}
	if session.State == "closed" || session.State == "failed" {
		if backend != nil {
			_ = backend.Close()
		}
		return
	}
	session.backend = backend
	session.poolKeys = cloneStrings(poolKeys)
	if session.Attached {
		session.State = "attached"
	} else {
		session.State = "connected"
	}
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.connected", SessionID: id, State: session.State, Time: now})
}

func (s *Service) failSession(id string, err error, cfg config.RuntimeConfig, state string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return
	}
	session.State = state
	session.Attached = false
	session.UpdatedAt = now
	session.ExitedAt = &now
	session.FrameworkError = safeError(err, cfg)
	s.publishSessionLocked(session, Event{Type: "session.error", SessionID: id, State: session.State, Error: session.FrameworkError, Time: now})
}

func (s *Service) openInteractive(ctx context.Context, sessionID string, req CreateRequest, server config.ServerProfile, cfg config.RuntimeConfig, pool *sshpool.Pool, dialOpts DialOptions) (*interactiveBackend, []string, config.RuntimeConfig, error) {
	var currentCfg = cfg
	for attempt := 0; attempt <= maxAuthRetryCount; attempt++ {
		select {
		case <-ctx.Done():
			return nil, nil, currentCfg, ctx.Err()
		default:
		}
		confirm := func(prompt sshpool.HostKeyPrompt) bool {
			return s.waitHostKeyResponse(ctx, sessionID, server, prompt)
		}
		client, poolKeys, _, err := pool.GetClient(server, currentCfg, confirm, sshpool.DialOptions{
			AgentSocket:   dialOpts.AgentSocket,
			HostKeyPolicy: req.HostKeyPolicy,
			Timeout:       dialOpts.Timeout,
		})
		if err != nil {
			if errors.Is(err, sshpool.ErrHostKeyReject) {
				return nil, nil, currentCfg, err
			}
			if req.AllowAuthRetry && sshpool.IsAuthError(err) && attempt < maxAuthRetryCount {
				runtimeCfg, retryErr := s.waitAuthResponse(ctx, sessionID, server, currentCfg, err, attempt+1)
				if retryErr != nil {
					return nil, nil, currentCfg, retryErr
				}
				currentCfg = runtimeCfg
				server = runtimeCfg.Servers[server.ID]
				continue
			}
			return nil, nil, currentCfg, err
		}
		pool.IncRef(poolKeys...)
		sshSession, err := client.NewSession()
		if err != nil {
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		if err := sshSession.RequestPty(req.Term, req.Rows, req.Cols, sshTerminalModes()); err != nil {
			_ = sshSession.Close()
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		setSSHSessionEnvironment(sshSession, req.Env)
		if req.ForwardAgent {
			if err := setupAgentForwarding(client, sshSession, dialOpts.AgentSocket); err != nil {
				s.publishError(sessionID, "agent forwarding setup failed: "+err.Error())
			}
		}
		stdin, err := sshSession.StdinPipe()
		if err != nil {
			_ = sshSession.Close()
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		stdout, err := sshSession.StdoutPipe()
		if err != nil {
			_ = sshSession.Close()
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		stderr, err := sshSession.StderrPipe()
		if err != nil {
			_ = sshSession.Close()
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		stdout = newObservedReader(stdout, func(path string) {
			s.updateCurrentDir(sessionID, path)
		})
		stderr = newObservedReader(stderr, func(path string) {
			s.updateCurrentDir(sessionID, path)
		})
		if err := sshSession.Shell(); err != nil {
			_ = sshSession.Close()
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		done := make(chan error, 1)
		backend := &interactiveBackend{session: sshSession, stdin: stdin, stdout: stdout, stderr: stderr, done: done}
		go func() {
			done <- sshSession.Wait()
			close(done)
		}()
		return backend, poolKeys, currentCfg, nil
	}
	return nil, nil, currentCfg, fmt.Errorf("%w: retry limit reached", ErrConflict)
}

func (s *Service) waitHostKeyResponse(ctx context.Context, sessionID string, server config.ServerProfile, prompt sshpool.HostKeyPrompt) bool {
	now := time.Now().UTC()
	challenge := &pendingChallenge{
		Challenge: Challenge{
			SessionID:   sessionID,
			Type:        "host_key",
			Pending:     true,
			Prompt:      prompt.Message,
			Fingerprint: prompt.Fingerprint,
			Host:        prompt.Host,
			KeyType:     prompt.KeyType,
			Risk:        hostKeyRisk(prompt.Changed),
			ServerAlias: server.Alias,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		response: make(chan ChallengeResponse, 1),
	}
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	session.hostKeyChallenge = challenge
	session.HostKeyPending = true
	session.State = "host_key_pending"
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{
		Type:      "session.host_key.challenge",
		SessionID: sessionID,
		State:     session.State,
		Challenge: cloneChallengePtr(&challenge.Challenge),
		Time:      now,
	})
	s.mu.Unlock()
	timer := time.NewTimer(challengeTimeout)
	defer timer.Stop()
	select {
	case resp := <-challenge.response:
		return resp.Accept && !resp.Abort
	case <-timer.C:
		s.failPendingChallenge(sessionID, "host_key", "host key confirmation timed out")
		return false
	case <-ctx.Done():
		s.failPendingChallenge(sessionID, "host_key", "connecting cancelled")
		return false
	}
}

func (s *Service) waitAuthResponse(ctx context.Context, sessionID string, server config.ServerProfile, cfg config.RuntimeConfig, err error, retryCount int) (config.RuntimeConfig, error) {
	failedMethod, allowedMethods, ok := sshpool.AuthFailureDetails(err)
	if !ok || len(allowedMethods) == 0 {
		allowedMethods = []string{config.AuthMethodPassword, config.AuthMethodKey, config.AuthMethodAgent}
	}
	now := time.Now().UTC()
	challenge := &pendingChallenge{
		Challenge: Challenge{
			SessionID:      sessionID,
			Type:           "auth",
			Pending:        true,
			Prompt:         "authentication failed, provide new credentials to retry",
			ServerAlias:    server.Alias,
			FailedMethod:   failedMethod,
			AllowedMethods: cloneStrings(allowedMethods),
			RetryCount:     retryCount,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		response: make(chan ChallengeResponse, 1),
	}
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return cfg, ErrNotFound
	}
	session.authRetryCount = retryCount
	session.authChallenge = challenge
	session.AuthPending = true
	session.State = "auth_pending"
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{
		Type:      "session.auth.challenge",
		SessionID: sessionID,
		State:     session.State,
		Challenge: cloneChallengePtr(&challenge.Challenge),
		Time:      now,
	})
	s.mu.Unlock()
	timer := time.NewTimer(challengeTimeout)
	defer timer.Stop()
	select {
	case resp := <-challenge.response:
		if resp.Abort {
			return cfg, fmt.Errorf("%w: authentication retry aborted", ErrAuthRetryAborted)
		}
		return s.runtimeConfigForAuthResponse(cfg, server, resp)
	case <-timer.C:
		s.failPendingChallenge(sessionID, "auth", "authentication retry timed out")
		return cfg, fmt.Errorf("%w: authentication retry timed out", ErrConflict)
	case <-ctx.Done():
		s.failPendingChallenge(sessionID, "auth", "authentication retry cancelled")
		return cfg, ctx.Err()
	}
}

var ErrAuthRetryAborted = errors.New("authentication retry aborted")

func (s *Service) runtimeConfigForAuthResponse(cfg config.RuntimeConfig, server config.ServerProfile, resp ChallengeResponse) (config.RuntimeConfig, error) {
	next := cloneRuntimeConfig(cfg)
	profile := next.Servers[server.ID]
	if resp.Password != "" {
		profile.AuthMethod = config.AuthMethodPassword
		profile.Password = resp.Password
		if resp.Remember {
			if _, err := s.config.SetServerPassword(server.ID, resp.Password); err != nil {
				return cfg, err
			}
			saved, err := s.config.RuntimeConfig()
			if err != nil {
				return cfg, err
			}
			next = saved
			profile = next.Servers[server.ID]
		}
	}
	if resp.KeyID != "" {
		profile.AuthMethod = config.AuthMethodKey
		profile.KeyID = resp.KeyID
		if resp.Remember {
			savedProfile := profile
			savedProfile.Password = ""
			if _, err := s.config.UpdateServer(server.ID, savedProfile); err != nil {
				return cfg, err
			}
			saved, err := s.config.RuntimeConfig()
			if err != nil {
				return cfg, err
			}
			next = saved
			profile = next.Servers[server.ID]
		}
	}
	if resp.Password == "" && resp.KeyID == "" {
		profile.AuthMethod = config.AuthMethodAgent
	}
	next.Servers[server.ID] = profile
	return next, nil
}

func (s *Service) failPendingChallenge(sessionID string, typ string, message string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return
	}
	if typ == "host_key" {
		session.hostKeyChallenge = nil
		session.HostKeyPending = false
	} else {
		session.authChallenge = nil
		session.AuthPending = false
	}
	session.State = "failed"
	session.FrameworkError = message
	session.ExitedAt = &now
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.error", SessionID: sessionID, State: session.State, Error: message, Time: now})
}

func (s *Service) publishError(sessionID string, message string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		s.publishSessionLocked(session, Event{Type: "session.error", SessionID: sessionID, State: session.State, Error: message, Time: now})
	}
}

func (s *Service) watchInteractive(id string, backend *interactiveBackend, pool *sshpool.Pool, poolKeys []string, cfg config.RuntimeConfig) {
	err := <-backend.done
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		pool.DecRef(poolKeys...)
		return
	}
	if session.State == "closed" || session.State == "failed" {
		pool.DecRef(poolKeys...)
		return
	}
	var exitCode *int
	var frameworkError string
	state := "closed"
	var exitErr *ssh.ExitError
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, context.Canceled):
	case errors.As(err, &exitErr):
		code := exitErr.ExitStatus()
		exitCode = &code
	default:
		frameworkError = safeError(err, cfg)
	}
	session.ExitCode = exitCode
	session.FrameworkError = frameworkError
	s.closeSessionLocked(session, state, "", exitCode, now)
	pool.DecRef(poolKeys...)
}

func (s *Service) updateCurrentDir(id string, dir string) {
	if dir == "" {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return
	}
	if session.CurrentDir == dir {
		return
	}
	session.CurrentDir = dir
	session.CWDUpdatedAt = &now
	session.UpdatedAt = now
	notify := CWDNotify{SessionID: id, Path: dir, Time: now}
	for ch := range session.cwdSubscribers {
		select {
		case ch <- notify:
		default:
		}
	}
	s.publishSessionLocked(session, Event{Type: "session.cwd", SessionID: id, State: session.State, Path: dir, Time: now})
}

func (s *Service) closeSessionLocked(session *resource, state string, cause string, exitCode *int, now time.Time) {
	if session.cancel != nil {
		session.cancel()
	}
	if session.backend != nil {
		_ = session.backend.Close()
	}
	session.State = state
	session.Attached = false
	session.UpdatedAt = now
	session.ExitedAt = &now
	if exitCode != nil {
		session.ExitCode = exitCode
	}
	if cause != "" {
		session.DisconnectCause = cause
	}
	for ch := range session.cwdSubscribers {
		select {
		case ch <- CWDNotify{SessionID: session.ID, Closed: true, Time: now}:
		default:
		}
		close(ch)
		delete(session.cwdSubscribers, ch)
	}
	eventType := "session.closed"
	if session.FrameworkError != "" {
		eventType = "session.failed"
	}
	s.publishSessionLocked(session, Event{
		Type:      eventType,
		SessionID: session.ID,
		State:     session.State,
		ExitCode:  session.ExitCode,
		Error:     session.FrameworkError,
		Time:      now,
	})
}

func (s *Service) publishSessionLocked(session *resource, event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	for ch := range session.subscribers {
		select {
		case ch <- cloneEvent(event):
		default:
		}
	}
	if s.onEvent != nil {
		s.onEvent(cloneEvent(event))
	}
}

func (r *resource) snapshot() Resource {
	out := r.Resource
	out.Env = cloneEnv(out.Env)
	if r.hostKeyChallenge != nil {
		out.HostKeyPending = r.hostKeyChallenge.Challenge.Pending
	}
	if r.authChallenge != nil {
		out.AuthPending = r.authChallenge.Challenge.Pending
	}
	return out
}

func resolveServer(cfg config.RuntimeConfig, ref string) (config.ServerProfile, error) {
	if server, ok := cfg.Servers[ref]; ok {
		return server, nil
	}
	for _, server := range cfg.Servers {
		if server.Alias == ref {
			return server, nil
		}
	}
	return config.ServerProfile{}, ErrNotFound
}

func runExec(exec Exec, req ExecRequest, server config.ServerProfile, cfg config.RuntimeConfig, pool *sshpool.Pool, dialOpts DialOptions) Exec {
	client, poolKeys, _, err := pool.GetClient(server, cfg, nil, sshpool.DialOptions{
		AgentSocket:   dialOpts.AgentSocket,
		HostKeyPolicy: req.HostKeyPolicy,
		Timeout:       dialOpts.Timeout,
	})
	if err != nil {
		exec.State = "failed"
		exec.ExitCode = -1
		exec.FrameworkError = safeError(err, cfg)
		exec.CompletedAt = time.Now().UTC()
		return exec
	}
	pool.IncRef(poolKeys...)
	defer pool.DecRef(poolKeys...)
	session, err := client.NewSession()
	if err != nil {
		exec.State = "failed"
		exec.ExitCode = -1
		exec.FrameworkError = safeError(err, cfg)
		exec.CompletedAt = time.Now().UTC()
		return exec
	}
	defer session.Close()
	stdout := &limitedWriter{limit: 512 * 1024}
	stderr := &limitedWriter{limit: 512 * 1024}
	session.Stdout = stdout
	session.Stderr = stderr
	runCtx := context.Background()
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Run(req.Command)
	}()
	var runErr error
	select {
	case runErr = <-done:
	case <-runCtx.Done():
		_ = session.Signal(ssh.SIGKILL)
		runErr = runCtx.Err()
	}
	exec.CompletedAt = time.Now().UTC()
	exec.Stdout = stdout.String()
	exec.Stderr = stderr.String()
	exec.Truncated = stdout.truncated || stderr.truncated
	if runErr == nil {
		exec.State = "completed"
		exec.ExitCode = 0
		return exec
	}
	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		exec.State = "completed"
		exec.ExitCode = exitErr.ExitStatus()
		return exec
	}
	exec.State = "failed"
	exec.ExitCode = -1
	exec.FrameworkError = safeError(runErr, cfg)
	return exec
}

func (s *Service) recordTestExec(req ExecRequest, now time.Time) Exec {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.execs) >= maxExecs {
		s.pruneExecsLocked()
	}
	id := "exec_" + strconv.FormatInt(s.nextID, 10)
	s.nextID++
	exec := Exec{
		ID:          id,
		ServerRef:   req.ServerRef,
		Command:     req.Command,
		State:       "completed",
		ExitCode:    0,
		StartedAt:   now,
		CompletedAt: now,
	}
	s.execs[id] = exec
	return exec
}

func newLocalInteractiveBackend() *interactiveBackend {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, stdinReader)
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		done <- nil
		close(done)
	}()
	return &interactiveBackend{stdin: stdinWriter, stdout: stdoutReader, stderr: stderrReader, done: done}
}

type limitedWriter struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() < w.limit {
		remaining := w.limit - w.buf.Len()
		if len(p) > remaining {
			_, _ = w.buf.Write(p[:remaining])
			w.truncated = true
			return len(p), nil
		}
		_, _ = w.buf.Write(p)
		return len(p), nil
	}
	w.truncated = true
	return len(p), nil
}

func (w *limitedWriter) String() string {
	return w.buf.String()
}

func readByteStream(ctx context.Context, r io.Reader, out chan<- []byte) {
	defer close(out)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func sshTerminalModes() ssh.TerminalModes {
	return ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 38400,
		ssh.TTY_OP_OSPEED: 38400,
	}
}

func setSSHSessionEnvironment(session *ssh.Session, env map[string]string) {
	for key, value := range env {
		if validSSHEnvName(key) && validSSHEnvValue(value) {
			_ = session.Setenv(key, value)
		}
	}
}

func validTerminalDimensions(rows, cols int) bool {
	return rows > 0 && rows <= 1000 && cols > 0 && cols <= 1000
}

func validSSHEnvName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func validSSHEnvValue(value string) bool {
	if len(value) > 1024 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (b *interactiveBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stdin != nil {
		_ = b.stdin.Close()
	}
	if b.session != nil {
		return b.session.Close()
	}
	return nil
}

func (b *interactiveBackend) WindowChange(rows, cols int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == nil {
		return nil
	}
	return b.session.WindowChange(rows, cols)
}

func (b *interactiveBackend) Signal(signal string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == nil {
		return nil
	}
	return b.session.Signal(ssh.Signal(signal))
}

func safeError(err error, cfg any) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if runtimeCfg, ok := cfg.(config.RuntimeConfig); ok {
		for _, secret := range runtimeSecrets(runtimeCfg) {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	if strings.Contains(strings.ToLower(msg), "private key") {
		return "ssh backend error"
	}
	return msg
}

func runtimeSecrets(cfg config.RuntimeConfig) []string {
	var out []string
	for _, server := range cfg.Servers {
		if server.Password != "" {
			out = append(out, server.Password)
		}
	}
	for _, proxy := range cfg.Proxies {
		if proxy.Password != "" {
			out = append(out, proxy.Password)
		}
	}
	for _, key := range cfg.Keys {
		if key.PrivateKey != "" {
			out = append(out, key.PrivateKey)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i]) > len(out[j])
	})
	return out
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func cloneRuntimeConfig(cfg config.RuntimeConfig) config.RuntimeConfig {
	next := config.RuntimeConfig{
		Settings:      cfg.Settings,
		Servers:       make(map[string]config.ServerProfile, len(cfg.Servers)),
		Proxies:       make(map[string]config.ProxyProfile, len(cfg.Proxies)),
		Keys:          make(map[string]config.KeyMetadata, len(cfg.Keys)),
		SyncProviders: make(map[string]config.SyncProviderConfig, len(cfg.SyncProviders)),
	}
	for id, server := range cfg.Servers {
		server.JumpHostIDs = append([]string(nil), server.JumpHostIDs...)
		server.Tags = append([]string(nil), server.Tags...)
		next.Servers[id] = server
	}
	for id, proxy := range cfg.Proxies {
		next.Proxies[id] = proxy
	}
	for id, key := range cfg.Keys {
		next.Keys[id] = key
	}
	for id, provider := range cfg.SyncProviders {
		next.SyncProviders[id] = provider
	}
	return next
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneChallengePtr(ch *Challenge) *Challenge {
	if ch == nil {
		return nil
	}
	out := *ch
	out.AllowedMethods = cloneStrings(out.AllowedMethods)
	return &out
}

func cloneEvent(event Event) Event {
	out := event
	out.Challenge = cloneChallengePtr(event.Challenge)
	return out
}

func hostKeyRisk(changed bool) string {
	if changed {
		return "HIGH_RISK"
	}
	return "REMOTE_TRUST"
}

func setupAgentForwarding(client *ssh.Client, session *ssh.Session, agentSocket string) error {
	socket := agentSocket
	if socket == "" {
		socket = os.Getenv("SSH_AUTH_SOCK")
	}
	if socket == "" {
		return errors.New("SSH_AUTH_SOCK is not set")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	keyring := agent.NewClient(conn)
	if err := agent.ForwardToAgent(client, keyring); err != nil {
		_ = conn.Close()
		return err
	}
	if err := agent.RequestAgentForwarding(session); err != nil {
		_ = conn.Close()
		return err
	}
	return nil
}

const (
	osc7Prefix    = "\x1b]7;"
	osc7MaxBuffer = 4096
)

type osc7Parser struct {
	buf []byte
}

func (p *osc7Parser) Observe(data []byte) ([]byte, []string, int) {
	if len(data) == 0 {
		return nil, nil, -1
	}
	p.buf = append(p.buf, data...)
	if len(p.buf) > osc7MaxBuffer {
		copy(p.buf, p.buf[len(p.buf)-osc7MaxBuffer:])
		p.buf = p.buf[:osc7MaxBuffer]
	}
	var paths []string
	for {
		start := strings.Index(string(p.buf), osc7Prefix)
		if start < 0 {
			return data, paths, -1
		}
		if start > 0 {
			p.buf = p.buf[start:]
		}
		payloadStart := len(osc7Prefix)
		payloadEnd, terminatorLen, ok := findOSCTerminatorBytes(p.buf[payloadStart:])
		if !ok {
			return data, paths, -1
		}
		payload := string(p.buf[payloadStart : payloadStart+payloadEnd])
		if dir := parseOSC7Payload(payload); dir != "" {
			paths = append(paths, dir)
		}
		p.buf = p.buf[payloadStart+payloadEnd+terminatorLen:]
	}
}

func findOSCTerminatorBytes(s []byte) (idx int, terminatorLen int, ok bool) {
	bel := -1
	st := -1
	for i, b := range s {
		if bel < 0 && b == '\a' {
			bel = i
		}
		if st < 0 && b == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
			st = i
		}
		if bel >= 0 || st >= 0 {
			break
		}
	}
	switch {
	case bel < 0 && st < 0:
		return 0, 0, false
	case bel >= 0 && (st < 0 || bel < st):
		return bel, 1, true
	default:
		return st, 2, true
	}
}

func parseOSC7Payload(payload string) string {
	if !strings.HasPrefix(payload, "file://") {
		return ""
	}
	u, err := url.Parse(payload)
	if err != nil {
		return ""
	}
	if u.Scheme != "file" || u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return ""
	}
	return cleanSlashPath(u.Path)
}

func cleanSlashPath(path string) string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, part)
		}
	}
	return "/" + strings.Join(out, "/")
}

type observedReader struct {
	reader io.Reader
	parser osc7Parser
	onPath func(string)
}

func newObservedReader(reader io.Reader, onPath func(string)) io.Reader {
	if reader == nil || onPath == nil {
		return reader
	}
	return &observedReader{reader: reader, onPath: onPath}
}

func (r *observedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		_, paths, _ := r.parser.Observe(p[:n])
		for _, path := range paths {
			r.onPath(path)
		}
	}
	return n, err
}
