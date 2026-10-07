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

	"knot-core/internal/resourcepolicy"
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
	challengeTimeout       = 60 * time.Second
	maxAuthRetryCount      = 3
	maxSessionSubscribers  = 16
	maxSessionCWDFollowers = 8
)

// attachDrainTimeout bounds how long a terminal transition waits for output
// delivery to finish before forcing the pump closed. It exists so a wedged
// reader cannot stall the transition, not as a tail-flush delay.
var attachDrainTimeout = 5 * time.Second

type configWriter interface {
	RuntimeConfig() (config.RuntimeConfig, error)
	SetServerPassword(id string, password string) (config.ServerProfileView, error)
	UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error)
}

type Service struct {
	execPolicy         resourcepolicy.Policy
	policy             resourcepolicy.Policy
	workers            resourcepolicy.Group
	callbacks          resourcepolicy.Callbacks
	stopped            bool
	releaseErrors      []error
	maintenanceStarted bool
	mu                 sync.RWMutex
	nextID             int64
	sessions           map[string]*resource
	execs              map[string]Exec
	execCtx            context.Context
	execCancel         context.CancelCauseFunc
	execActive         map[string]*execOperation
	config             configWriter
	pool               *sshpool.Pool
	dial               DialOptions
	testMode           bool
	onEvent            func(Event)
}

func NewService() *Service {
	execCtx, execCancel := context.WithCancelCause(context.Background())
	return &Service{
		execPolicy: resourcepolicy.Execs(),
		policy:     resourcepolicy.Sessions(),
		nextID:     1,
		sessions:   map[string]*resource{},
		execs:      map[string]Exec{},
		execCtx:    execCtx,
		execCancel: execCancel,
		execActive: map[string]*execOperation{},
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
	workers int
	Resource
	serverID         string
	hostKeyChallenge *pendingChallenge
	authChallenge    *pendingChallenge
	authRetryCount   int
	authResponse     *ChallengeResponse

	// pendingCredentials holds credentials to save after successful authentication.
	// This ensures we only save credentials that actually worked.
	pendingCredentials *ChallengeResponse

	// outcomeCommitted guards one-time recording of the terminal exit outcome,
	// so the session resource and its attach observers always agree.
	outcomeCommitted bool
	// attachGen identifies the current attachment owner so a stale detachment
	// cannot release a newer attachment.
	attachGen int64
	// claim is the attachment that owns this session's interactive streams. It
	// is the current owner while Attached is set, and the retiring owner until
	// the next attachment has waited for its workers.
	claim *attachmentClaim

	subscribers    map[chan Event]struct{}
	cwdSubscribers map[chan CWDNotify]struct{}
	backend        *interactiveBackend
	poolKeys       []string
	cancel         context.CancelFunc
}

type interactiveBackend struct {
	lease      *sshpool.ClientLease
	session    *ssh.Session
	stdin      io.WriteCloser
	pump       *outputPump
	exitResult *exitResult
	mu         sync.Mutex
	closed     bool
	closeDone  chan struct{}
	// inputSlot is held by the actual stdin write, including after its attachment
	// stops waiting. Reattachment cannot create another writer on this backend.
	inputSlot chan struct{}
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
	Warning   *Warning   `json:"warning,omitempty"`
	Time      time.Time  `json:"time"`
}

// Warning is a sanitized, client-visible notice about a non-fatal problem, such
// as a credential that could not be persisted after a successful connection.
// Its message never carries credential material.
type Warning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Warning kinds. They are part of the event contract, so a client can react to
// a specific condition instead of matching on message text.
const (
	WarningCredentialSaveFailed = "credential_save_failed"
)

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
	FrameworkCode  string    `json:"framework_code,omitempty"`
	CleanupError   string    `json:"cleanup_error,omitempty"`
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

// AttachStream is one attachment to an interactive session's PTY. Exactly one
// attachment exists at a time; callers must invoke Cancel and Release when done.
//
// Output is delivered per stream: ordering within stdout or stderr is preserved,
// but no global ordering between the two streams is promised. `Exit` delivers the
// session's final outcome exactly once, after which no further output arrives.
// If `Overflow` is closed, this attachment fell behind and output was truncated;
// the client must treat the stream as incomplete and re-attach.
type AttachStream struct {
	Cancel  func()
	Release func()
	Stdout  <-chan []byte
	Stderr  <-chan []byte
	// Input accepts stdin bytes for this attachment and queues them for the
	// attachment's own worker. Closing it ends input for this attachment only;
	// the session's stdin stays usable for the next one.
	Input io.WriteCloser
	Exit  <-chan ExitOutcome
	// Revoked is closed when this attachment loses ownership of the session
	// without the session ending: an explicit detach, or a newer attachment. A
	// session that ends is reported through Exit instead, so a completed session
	// is never mistaken for a revocation.
	Revoked <-chan struct{}
	// InputError reports the first failure delivering input to the remote, after
	// which this attachment's input worker has stopped.
	InputError <-chan error
	Overflow   <-chan struct{}
	// BacklogTruncated reports that output produced before this attachment was
	// dropped because it exceeded the retained backlog. Bytes delivered to an
	// earlier attachment are never replayed.
	BacklogTruncated bool
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
	now := s.policy.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneSessionsLocked()
	if s.pendingLocked() >= s.policy.Active {
		return Resource{}, fmt.Errorf("%w: session cleanup limit reached", ErrConflict)
	}
	if s.stopped || s.activeLocked() >= s.policy.Active {
		return Resource{}, fmt.Errorf("%w: session limit reached", ErrConflict)
	}
	id := strconv.FormatInt(s.nextID, 10)
	s.nextID++
	ctx, cancel := context.WithCancel(s.execCtx)
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
	ctx = resourcepolicy.WithWork(ctx, func() func() { s.mu.Lock(); defer s.mu.Unlock(); return s.beginWorkLocked(session) })
	s.sessions[id] = session
	s.publishSessionLocked(session, Event{Type: "session.created", SessionID: id, State: res.State, Time: now})
	if testMode {
		backend := newSessionBackend()
		session.backend = backend
		session.State = "connected"
		session.UpdatedAt = now
		s.publishSessionLocked(session, Event{Type: "session.connected", SessionID: id, State: session.State, Time: now})
		return session.snapshot(), nil
	}
	s.runLocked(session, func() { s.connectSession(ctx, id, req, cfgProvider, pool, dialOpts, testMode) })
	return session.snapshot(), nil
}

func (s *Service) List() []Resource {
	return s.ListWithOptions(ListOptions{})
}

func (s *Service) ListWithOptions(opts ListOptions) []Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneSessionsLocked()
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
	return s.activeLocked()
}

func (s *Service) Get(id string) (Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneSessionsLocked()
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
	now := s.policy.Now().UTC()
	session.Attached = true
	session.State = "attached"
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.attached", SessionID: id, State: session.State, Time: now})
	return session.snapshot(), nil
}

func (s *Service) AttachStream(id string) (AttachStream, Resource, error) {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return AttachStream{}, Resource{}, ErrNotFound
	}
	if session.State == "closed" || session.State == "failed" {
		s.mu.Unlock()
		return AttachStream{}, Resource{}, fmt.Errorf("%w: closed session cannot be attached", ErrConflict)
	}
	if session.Attached {
		s.mu.Unlock()
		return AttachStream{}, Resource{}, fmt.Errorf("%w: session is already attached", ErrConflict)
	}
	backend := session.backend
	if backend == nil {
		s.mu.Unlock()
		return AttachStream{}, Resource{}, fmt.Errorf("%w: SSH PTY backend is not connected", ErrConflict)
	}
	// The previous claim loses ownership before the new one takes the pump. Its
	// subscription is dropped here, and its workers are awaited below, so no
	// output owner or input worker of the old attachment overlaps the new one.
	retiring := session.claim
	if retiring != nil {
		retiring.revoke()
	}
	now := s.policy.Now().UTC()
	session.Attached = true
	session.attachGen++
	claim := newAttachmentClaim(session.attachGen, backend)
	session.claim = claim
	s.runLocked(session, func() { <-claim.done })
	session.State = "attached"
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.attached", SessionID: id, State: session.State, Time: now})
	snapshot := session.snapshot()
	s.mu.Unlock()

	// Waiting for the old workers happens outside the service lock: a worker that
	// is stuck writing to a remote that stopped reading must not block other
	// sessions.
	if retiring != nil {
		retiring.wait(attachmentGrace)
	}
	claim.start()

	var releaseMu sync.Mutex
	released := false
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true
		s.detachAttachment(id, claim.gen)
	}
	cancel := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		claim.revoke()
	}

	return AttachStream{
		Cancel:           cancel,
		Release:          release,
		Stdout:           claim.stdout,
		Stderr:           claim.stderr,
		Input:            claim.input,
		Exit:             claim.exit,
		Overflow:         claim.sub.Overflow(),
		Revoked:          claim.revoked,
		InputError:       claim.inputErr,
		BacklogTruncated: claim.backlogTruncated,
	}, snapshot, nil
}

// detachAttachment releases attachment ownership only if gen still owns it, so a
// finishing attachment can never detach a newer one.
func (s *Service) detachAttachment(id string, gen int64) {
	now := s.policy.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok || !session.Attached || session.attachGen != gen {
		return
	}
	s.releaseAttachLocked(session, now)
}

// releaseAttachLocked hands the session back to the next client. The claim stays
// on the session as the retiring owner: it is revoked here, which drops its
// output subscription immediately, and the next attachment waits for its
// workers before starting.
func (s *Service) releaseAttachLocked(session *resource, now time.Time) {
	session.Attached = false
	if session.State == "attached" {
		session.State = "detached"
	}
	session.UpdatedAt = now
	s.publishSessionLocked(session, Event{Type: "session.detached", SessionID: session.ID, State: session.State, Time: now})
	if session.claim != nil {
		session.claim.revoke()
	}
}

// Detach releases the current attachment, whatever its generation. Attachments
// started via AttachStream should prefer AttachStream.Release so they cannot
// release a newer attachment.
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
	s.releaseAttachLocked(session, s.policy.Now().UTC())
	return session.snapshot(), nil
}

// controlIOTimeout bounds one control operation's backend I/O. A control
// request must never wait forever on a remote that stopped answering: past the
// bound the caller is given a definite error instead of being pinned. It is a
// variable so tests can shorten it.
var controlIOTimeout = 10 * time.Second

// runControlIO runs a backend control operation without the service lock held
// and never blocks the caller for longer than controlIOTimeout. The operation
// itself is released by the session teardown a disconnect performs: closing the
// SSH session unblocks a write that is stuck on it.
func runControlIO(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	timer := time.NewTimer(controlIOTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("timed out after %s", controlIOTimeout)
	}
}

// failControl reports a backend control failure on the session's event stream
// and returns it as a definite error. A control operation that failed is never
// reported as success.
func (s *Service) failControl(id string, target *resource, op string, cause error) error {
	err := fmt.Errorf("%w: %s failed: %v", ErrConflict, op, cause)
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.sessions[id]; ok && current == target && !isTerminalState(current.State) {
		s.publishSessionLocked(current, Event{Type: "session.error", SessionID: id, State: current.State, Error: err.Error(), Time: s.policy.Now().UTC()})
	}
	return err
}

// Control applies one control operation to a session.
//
// The service lock only guards validation and result commit. Operations that
// reach the remote (resize, signal, close_stdin) run with the lock released, so
// a stalled SSH write cannot block every other session, and a concurrent
// disconnect can still acquire the lock and tear the backend down to release
// them. The result is committed only after re-checking, under the lock, that the
// session is still the same live resource.
func (s *Service) Control(id string, req ControlRequest) (Resource, error) {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return Resource{}, ErrNotFound
	}
	if isTerminalState(session.State) && req.Type != "disconnect" {
		s.mu.Unlock()
		return Resource{}, fmt.Errorf("%w: closed session cannot be controlled", ErrConflict)
	}
	now := s.policy.Now().UTC()

	switch req.Type {
	case "detach":
		s.releaseAttachLocked(session, now)
		snapshot := session.snapshot()
		s.mu.Unlock()
		return snapshot, nil
	case "disconnect":
		// Close the backend outside the service lock: tearing down an SSH session
		// must never block every other session behind s.mu.
		backend := s.closeSessionLocked(session, "closed", ExitOutcome{DisconnectCause: causeClientDisconnected}, now)
		snapshot := session.snapshot()
		s.mu.Unlock()
		if backend != nil {
			if err := backend.Close(); err != nil {
				return snapshot, fmt.Errorf("disconnect session: %w", err)
			}
		}
		return snapshot, nil
	}

	// Validate with the lock held so invalid parameters never reach the remote,
	// then capture the backend the operation applies to.
	backend := session.backend
	var op func() error
	switch req.Type {
	case "resize":
		if !validTerminalDimensions(req.Rows, req.Cols) {
			s.mu.Unlock()
			return Resource{}, fmt.Errorf("%w: terminal dimensions are invalid", ErrValidation)
		}
		if backend != nil {
			op = func() error { return backend.WindowChange(req.Rows, req.Cols) }
		}
	case "close_stdin":
		// Idempotent: repeating close_stdin must not close twice or affect other
		// sessions.
		if backend != nil {
			op = func() error { return backend.CloseStdin() }
		}
	case "signal":
		switch req.Signal {
		case "HUP", "INT", "KILL", "TERM", "USR1", "USR2":
		default:
			s.mu.Unlock()
			return Resource{}, fmt.Errorf("%w: unsupported signal", ErrValidation)
		}
		if backend != nil {
			op = func() error { return backend.Signal(req.Signal) }
		}
	default:
		s.mu.Unlock()
		return Resource{}, fmt.Errorf("%w: unsupported control type", ErrValidation)
	}
	s.mu.Unlock()

	if op != nil {
		if err := runControlIO(op); err != nil {
			return Resource{}, s.failControl(id, session, req.Type, err)
		}
	}

	// Commit under the lock, and only if the session is still this resource and
	// still live. An operation that raced a disconnect did reach the backend, but
	// its result no longer describes a live resource: record nothing new and
	// publish nothing, so a closed session is never reported as resized.
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[id]
	if !ok {
		return Resource{}, fmt.Errorf("%w: session no longer exists", ErrConflict)
	}
	if current != session || isTerminalState(current.State) {
		return current.snapshot(), nil
	}
	if req.Type == "resize" {
		current.Rows = req.Rows
		current.Cols = req.Cols
	}
	current.UpdatedAt = s.policy.Now().UTC()
	if req.Type == "resize" {
		s.publishSessionLocked(current, Event{Type: "session.resized", SessionID: id, State: current.State, Rows: req.Rows, Cols: req.Cols, Time: current.UpdatedAt})
	}
	return current.snapshot(), nil
}

func (s *Service) Disconnect(id string) (Resource, error) {
	return s.Control(id, ControlRequest{Type: "disconnect"})
}

func (s *Service) DisconnectByPoolKey(poolKey string) []Resource {
	if poolKey == "" {
		return nil
	}
	now := s.policy.Now().UTC()
	s.mu.Lock()
	var closed []Resource
	var backends []*interactiveBackend
	for _, session := range s.sessions {
		if session.State == "closed" || session.State == "failed" || !containsString(session.poolKeys, poolKey) {
			continue
		}
		backend := s.closeSessionLocked(session, "closed", ExitOutcome{DisconnectCause: causePoolDisconnected}, now)
		if backend != nil {
			backends = append(backends, backend)
		}
		closed = append(closed, session.snapshot())
	}
	s.mu.Unlock()
	for _, backend := range backends {
		_ = backend.Close()
	}
	return closed
}

func (s *Service) Subscribe(id string) (<-chan Event, func(), Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneSessionsLocked()
	session, ok := s.sessions[id]
	if !ok {
		return nil, nil, Resource{}, ErrNotFound
	}
	if len(session.subscribers) >= maxSessionSubscribers {
		return nil, nil, Resource{}, fmt.Errorf("%w: subscriber limit reached", ErrConflict)
	}
	ch := make(chan Event, maxSessionSubscribers)
	if isTerminalState(session.State) {
		close(ch)
		return ch, func() {}, session.snapshot(), nil
	}
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
	s.pruneSessionsLocked()
	session, ok := s.sessions[id]
	if !ok {
		return nil, nil, Resource{}, ErrNotFound
	}
	if session.State == "closed" || session.State == "failed" {
		return nil, nil, Resource{}, fmt.Errorf("%w: closed session cannot be followed", ErrConflict)
	}
	if len(session.cwdSubscribers) >= maxSessionCWDFollowers {
		return nil, nil, Resource{}, fmt.Errorf("%w: CWD follower limit reached", ErrConflict)
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

func (s *Service) pruneExecsLocked() {
	var items []resourcepolicy.Item
	for id, exec := range s.execs {
		if _, active := s.execActive[id]; !active {
			items = append(items, resourcepolicy.Item{ID: id, End: exec.CompletedAt})
		}
	}
	for _, id := range s.execPolicy.Expired(items) {
		delete(s.execs, id)
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
	// Once terminal, a challenge is no longer submittable: answering one would
	// let a late prompt move the session back out of its recorded outcome.
	if isTerminalState(session.State) {
		s.mu.Unlock()
		return Challenge{}, fmt.Errorf("%w: session %s no longer accepts challenge responses", ErrConflict, id)
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
	session.UpdatedAt = s.policy.Now().UTC()
	s.publishSessionLocked(session, Event{Type: "session.challenge.resolved", SessionID: id, State: session.State, Challenge: &challenge, Time: session.UpdatedAt})
	s.mu.Unlock()
	select {
	case pending.response <- resp:
	default:
	}
	challenge.Pending = false
	challenge.UpdatedAt = s.policy.Now().UTC()
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

// isTerminalState reports whether a session state is final. A terminal state is
// never left again: the outcome, its time and its cause are recorded once, so a
// late background failure cannot rewrite a completed cancellation.
func isTerminalState(state string) bool {
	return state == "closed" || state == "failed"
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
	// The resolved server is always recorded: it is the save target for a
	// remembered credential, and a client-supplied Alias is display-only.
	if req.Alias == "" {
		req.Alias = server.Alias
	}
	s.mu.Lock()
	if session, ok := s.sessions[id]; ok {
		session.serverID = server.ID
		if req.Alias != "" {
			session.Alias = req.Alias
		}
	}
	s.mu.Unlock()
	backend, poolKeys, finalCfg, err := s.openInteractive(ctx, id, req, server, runtimeCfg, pool, dialOpts)
	if err != nil {
		s.failSession(id, err, finalCfg, "failed")
		return
	}
	s.attachBackend(id, backend, poolKeys)
	s.mu.Lock()
	res := s.sessions[id]
	s.runLocked(res, func() { s.watchInteractive(id, backend, finalCfg) })
	s.mu.Unlock()
}

func (s *Service) attachBackend(id string, backend *interactiveBackend, poolKeys []string) {
	now := s.policy.Now().UTC()
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok || isTerminalState(session.State) {
		s.mu.Unlock()
		if backend != nil {
			_ = backend.Close()
		}
		return
	}
	defer s.mu.Unlock()
	session.backend = backend
	session.poolKeys = cloneStrings(poolKeys)
	if session.Attached {
		session.State = "attached"
	} else {
		session.State = "connected"
	}
	session.UpdatedAt = now

	// Authentication succeeded - now save credentials if Remember was set. The
	// candidate is bound to the attempt that just succeeded, so a stale one from
	// an earlier rejected attempt can never be written here.
	if session.pendingCredentials != nil && session.pendingCredentials.Remember {
		candidate := *session.pendingCredentials
		session.pendingCredentials = nil
		serverID := session.serverID
		s.runLocked(session, func() { s.saveCredentials(id, serverID, candidate) })
	} else {
		session.pendingCredentials = nil
	}

	s.publishSessionLocked(session, Event{Type: "session.connected", SessionID: id, State: session.State, Time: now})
}

func (s *Service) failSession(id string, err error, cfg config.RuntimeConfig, state string) {
	now := s.policy.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return
	}
	// A session that already reached a terminal state keeps it. The connect
	// attempt that produced this error was cancelled, so its failure must not
	// rewrite the recorded outcome, time or cause.
	if isTerminalState(session.State) {
		return
	}
	if session.cancel != nil {
		session.cancel()
	}
	session.State = state
	session.Attached = false
	session.UpdatedAt = now
	session.ExitedAt = &now
	session.FrameworkError = safeError(err, cfg)
	// No credential candidate survives a failed connect attempt.
	session.pendingCredentials = nil
	session.hostKeyChallenge = nil
	session.authChallenge = nil
	session.HostKeyPending = false
	session.AuthPending = false
	s.publishSessionLocked(session, Event{Type: "session.error", SessionID: id, State: session.State, Error: session.FrameworkError, Time: now})
	s.closeSubscribersLocked(session)
}

// openSSHSession opens a channel on a pooled client under ctx. x/crypto/ssh has
// no cancellation for this call, so a cancelled attempt abandons the pending open
// and closes whatever channel it eventually returns; the shared client itself is
// never closed here, because another session may be using it.
func openSSHSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		session *ssh.Session
		err     error
	}
	done := make(chan result, 1)
	resourcepolicy.Go(ctx, func() {
		session, err := client.NewSession()
		done <- result{session: session, err: err}
	})
	select {
	case res := <-done:
		return res.session, res.err
	case <-ctx.Done():
		resourcepolicy.Go(ctx, func() {
			if res := <-done; res.session != nil {
				_ = res.session.Close()
			}
		})
		return nil, ctx.Err()
	}
}

// sessionSetup runs one blocking setup step on a session this attempt owns. A
// cancelled attempt closes that session, which unblocks the step; the step's own
// result is drained so the goroutine cannot leak.
func sessionSetup(ctx context.Context, session *ssh.Session, step func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	resourcepolicy.Go(ctx, func() { done <- step() })
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Close()
		resourcepolicy.Go(ctx, func() { <-done })
		return ctx.Err()
	}
}

func (s *Service) openInteractive(ctx context.Context, sessionID string, req CreateRequest, server config.ServerProfile, cfg config.RuntimeConfig, pool *sshpool.Pool, dialOpts DialOptions) (*interactiveBackend, []string, config.RuntimeConfig, error) {
	var currentCfg = cfg
	for attempt := 0; attempt <= maxAuthRetryCount; attempt++ {
		select {
		case <-ctx.Done():
			return nil, nil, currentCfg, ctx.Err()
		default:
		}
		confirm := func(attemptCtx context.Context, prompt sshpool.HostKeyPrompt) bool {
			return s.waitHostKeyResponse(attemptCtx, sessionID, server, prompt)
		}
		lease, err := pool.AcquireClientContextWithPrompt(ctx, server, currentCfg, confirm, sshpool.DialOptions{
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
		client, poolKeys := lease.Client, lease.Keys
		ownedCtx, work := resourcepolicy.Scope(ctx)
		release := func() {
			resourcepolicy.Go(ctx, func() { _ = work.Wait(context.Background()); lease.Release() })
		}
		sshSession, err := openSSHSession(ownedCtx, client)
		if err != nil {
			release()
			return nil, nil, currentCfg, err
		}
		if err := sessionSetup(ownedCtx, sshSession, func() error {
			return sshSession.RequestPty(req.Term, req.Rows, req.Cols, sshTerminalModes())
		}); err != nil {
			_ = sshSession.Close()
			release()
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
			release()
			return nil, nil, currentCfg, err
		}
		stdout, err := sshSession.StdoutPipe()
		if err != nil {
			_ = sshSession.Close()
			release()
			return nil, nil, currentCfg, err
		}
		stderr, err := sshSession.StderrPipe()
		if err != nil {
			_ = sshSession.Close()
			release()
			return nil, nil, currentCfg, err
		}
		stdout = newObservedReader(stdout, func(path string) {
			s.updateCurrentDir(sessionID, path)
		})
		stderr = newObservedReader(stderr, func(path string) {
			s.updateCurrentDir(sessionID, path)
		})
		if err := sessionSetup(ownedCtx, sshSession, sshSession.Shell); err != nil {
			_ = sshSession.Close()
			release()
			return nil, nil, currentCfg, err
		}

		// Both streams feed one pump, so a re-attach subscribes to the same single
		// reader instead of racing a second one, and remote stderr is drained
		// (an unread stderr pipe would stop replenishing the SSH window).
		pump := newOutputPump()
		pump.AddReader(streamStdout, stdout)
		pump.AddReader(streamStderr, stderr)
		backend := &interactiveBackend{
			lease:      lease,
			session:    sshSession,
			stdin:      stdin,
			pump:       pump,
			exitResult: newExitResult(),
		}
		pump.Start()

		return backend, poolKeys, currentCfg, nil
	}
	return nil, nil, currentCfg, fmt.Errorf("%w: retry limit reached", ErrConflict)
}

func (s *Service) waitHostKeyResponse(ctx context.Context, sessionID string, server config.ServerProfile, prompt sshpool.HostKeyPrompt) bool {
	now := s.policy.Now().UTC()
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
	// A cancelled session must not re-enter a pending state through a late host
	// key prompt from the attempt that is still unwinding.
	if isTerminalState(session.State) {
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
	now := s.policy.Now().UTC()
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
	// The session was cancelled or already failed while this attempt was in
	// flight; do not raise another prompt or move it back to a pending state.
	if isTerminalState(session.State) {
		s.mu.Unlock()
		return cfg, fmt.Errorf("%w: session is no longer connectable", ErrConflict)
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

		// The candidate is bound to the attempt it answers: every response
		// replaces the previous one, including with nil when this attempt is not
		// to be remembered. A rejected attempt's remembered password therefore
		// never survives into a later successful connection.
		s.mu.Lock()
		if session, ok := s.sessions[sessionID]; ok {
			if resp.Remember {
				candidate := resp
				session.pendingCredentials = &candidate
			} else {
				session.pendingCredentials = nil
			}
		}
		s.mu.Unlock()

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

// runtimeConfigForAuthResponse constructs temporary runtime config for authentication attempt.
// It does NOT save credentials - that happens only after successful authentication.
// The Remember flag is stored separately and processed after connection succeeds.
func (s *Service) runtimeConfigForAuthResponse(cfg config.RuntimeConfig, server config.ServerProfile, resp ChallengeResponse) (config.RuntimeConfig, error) {
	next := cloneRuntimeConfig(cfg)
	profile := next.Servers[server.ID]

	if resp.Password != "" {
		profile.AuthMethod = config.AuthMethodPassword
		profile.Password = resp.Password
		// Note: Do NOT save here - wait for authentication success
	}

	if resp.KeyID != "" {
		profile.AuthMethod = config.AuthMethodKey
		profile.KeyID = resp.KeyID
		// Note: Do NOT save here - wait for authentication success
	}

	if resp.Password == "" && resp.KeyID == "" {
		profile.AuthMethod = config.AuthMethodAgent
	}

	next.Servers[server.ID] = profile
	return next, nil
}

func (s *Service) failPendingChallenge(sessionID string, typ string, message string) {
	now := s.policy.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return
	}
	// A challenge that was cancelled by a terminal transition reports its
	// failure here; the terminal state and its cause stand.
	if isTerminalState(session.State) {
		return
	}
	if typ == "host_key" {
		session.hostKeyChallenge = nil
		session.HostKeyPending = false
	} else {
		session.authChallenge = nil
		session.AuthPending = false
	}
	if session.cancel != nil {
		session.cancel()
	}
	session.State = "failed"
	session.FrameworkError = message
	session.ExitedAt = &now
	session.UpdatedAt = now
	session.pendingCredentials = nil
	s.publishSessionLocked(session, Event{Type: "session.error", SessionID: sessionID, State: session.State, Error: message, Time: now})
	s.closeSubscribersLocked(session)
}

func (s *Service) publishError(sessionID string, message string) {
	now := s.policy.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		s.publishSessionLocked(session, Event{Type: "session.error", SessionID: sessionID, State: session.State, Error: message, Time: now})
	}
}

// watchInteractive is the single owner of a session's terminal transition. It
// waits for the remote to exit, drains output that was already received, records
// the outcome once, and releases the pool references.
func (s *Service) watchInteractive(id string, backend *interactiveBackend, cfg config.RuntimeConfig) {
	// ssh.Session.Wait returns only after the stdin/stdout/stderr copies have all
	// finished, so by the time it returns the pump has read every byte the remote
	// sent. A bounded wait (instead of a fixed sleep) covers a wedged reader.
	err := backend.session.Wait()
	if !backend.pump.WaitDone(attachDrainTimeout) {
		backend.pump.Abort()
	}

	outcome := computeExitResult(err)

	now := s.policy.Now().UTC()
	s.mu.Lock()
	session, ok := s.sessions[id]
	switch {
	case !ok:
		// The session record is gone; still publish the outcome so no attach
		// observer waits forever.
		backend.exitResult.Set(outcome)
	case isTerminalState(session.State):
		// Already finalized, for example by a client disconnect or pool teardown.
		// The terminal state and its cause stand; commitOutcomeLocked is a no-op
		// for an outcome already recorded, and guarantees publication otherwise.
		s.commitOutcomeLocked(session, outcome)
	default:
		state := "closed"
		if outcome.FrameworkError != "" {
			state = "failed"
		}
		s.closeSessionLocked(session, state, outcome, now)
	}
	s.mu.Unlock()

	_ = backend.Close()
	// Close bounds the caller, not the real cleanup. Keep ownership until the
	// backend settles, even after ErrTeardownTimeout; Shutdown reports its budget
	// expiry instead of pretending this work ended or returning the lease early.
	<-backend.completion()
	if backend.lease != nil {
		backend.lease.Release()
	}
}

// exitStatuser matches anything exposing a remote exit status, including wrapped
// errors and test doubles, without pinning callers to *ssh.ExitError.
type exitStatuser interface {
	ExitStatus() int
}

// computeExitResult maps the error from ssh.Session.Wait into an explicit
// outcome. A missing exit status, a dropped connection and a deliberate local
// close are all kept distinct from a normal remote exit, and none of them is
// reported as success.
func computeExitResult(err error) ExitOutcome {
	if err == nil || errors.Is(err, io.EOF) {
		zero := 0
		return ExitOutcome{Code: &zero}
	}

	var missing *ssh.ExitMissingError
	if errors.As(err, &missing) {
		return ExitOutcome{
			FrameworkError:  "remote session ended without exit status",
			DisconnectCause: causeExitStatusMissing,
		}
	}

	// A remote exit-signal is reported by the ssh package as 128+signum together
	// with the signal name, which is what distinguishes it from a plain non-zero
	// exit status.
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitStatus()
		outcome := ExitOutcome{Code: &code}
		if exitErr.Signal() != "" {
			outcome.DisconnectCause = causeRemoteSignal
		}
		return outcome
	}

	var status exitStatuser
	if errors.As(err, &status) {
		code := status.ExitStatus()
		if code < 0 {
			return ExitOutcome{Code: &code, DisconnectCause: causeRemoteSignal}
		}
		return ExitOutcome{Code: &code}
	}

	if errors.Is(err, context.Canceled) {
		return ExitOutcome{DisconnectCause: causeClientDisconnected}
	}

	return ExitOutcome{FrameworkError: err.Error(), DisconnectCause: causeNetworkError}
}

func (s *Service) updateCurrentDir(id string, dir string) {
	if dir == "" {
		return
	}
	now := s.policy.Now().UTC()
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

// closeSessionLocked marks the session terminal and records its outcome exactly
// once. The outcome is committed to the session resource and to the backend's
// exit result together, so GET and the attach exit message always agree.
//
// The backend is returned for the caller to close after releasing s.mu: it is
// never closed here, because tearing down an SSH session can block on the
// network and must not hold the service lock.
//
// Calling it again for an already terminal session is a no-op; the terminal
// state, exit outcome and ExitedAt of the first close are preserved.
func (s *Service) closeSessionLocked(session *resource, state string, outcome ExitOutcome, now time.Time) *interactiveBackend {
	if isTerminalState(session.State) {
		return nil
	}
	backend := session.backend
	if backend != nil {
		done := backend.completion()
		s.runLocked(session, func() { <-done })
	}
	s.commitOutcomeLocked(session, outcome)
	if session.cancel != nil {
		session.cancel()
	}
	session.State = state
	session.Attached = false
	// The attachment is dropped without being revoked: its exit waiter must still
	// deliver the terminal outcome to whoever is watching, and its input worker
	// ends when the pump does.
	session.claim = nil
	session.UpdatedAt = now
	session.ExitedAt = &now
	// Clear every pending marker, waiter and credential candidate so a late
	// challenge, retry or background failure cannot resurrect the session or
	// leave a secret waiting to be written.
	session.hostKeyChallenge = nil
	session.authChallenge = nil
	session.HostKeyPending = false
	session.AuthPending = false
	session.authResponse = nil
	session.pendingCredentials = nil
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
	for ch := range session.subscribers {
		close(ch)
		delete(session.subscribers, ch)
	}
	return backend
}

// commitOutcomeLocked records the terminal outcome on the session record and
// publishes it to exit observers. The first outcome wins, so a synthesized
// result can never replace a real exit status.
func (s *Service) commitOutcomeLocked(session *resource, outcome ExitOutcome) {
	if session.outcomeCommitted {
		return
	}
	session.outcomeCommitted = true
	if outcome.Code != nil {
		session.ExitCode = outcome.Code
	}
	if outcome.FrameworkError != "" {
		session.FrameworkError = outcome.FrameworkError
	}
	if outcome.DisconnectCause != "" {
		session.DisconnectCause = outcome.DisconnectCause
	}
	if session.backend != nil {
		session.backend.exitResult.Set(outcome)
	}
}

func (s *Service) publishSessionLocked(session *resource, event Event) {
	if event.Time.IsZero() {
		event.Time = s.policy.Now().UTC()
	}
	for ch := range session.subscribers {
		select {
		case ch <- cloneEvent(event):
		default:
		}
	}
	if s.onEvent != nil {
		callback := s.onEvent
		copy := cloneEvent(event)
		s.callbacks.Send(func() { callback(copy) })
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

// newSessionBackend builds the backend of a session created in test mode. It is a
// variable so tests can supply a backend whose I/O they control.
var newSessionBackend = newLocalInteractiveBackend

// newLocalInteractiveBackend builds a backend that behaves like a session whose
// remote immediately reaches EOF on stdin: it echoes nothing, and reports a clean
// exit 0 once stdin is closed. It exists so HTTP/WS contract tests can exercise
// attach without a real SSH server.
func newLocalInteractiveBackend() *interactiveBackend {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	pump := newOutputPump()
	pump.AddReader(streamStdout, stdoutReader)
	exitResult := newExitResult()
	pump.Start()

	go func() {
		_, _ = io.Copy(io.Discard, stdinReader)
		_ = stdoutWriter.Close()
		<-pump.Done()

		zero := 0
		exitResult.Set(ExitOutcome{Code: &zero})
	}()

	return &interactiveBackend{
		stdin:      stdinWriter,
		pump:       pump,
		exitResult: exitResult,
	}
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

// teardownGrace bounds how long a teardown step waits for one close to finish
// before leaving it to complete in the background. It is a variable so tests can
// shorten it.
var teardownGrace = 2 * time.Second

// ErrTeardownTimeout reports that a teardown step did not finish within its
// grace. The step keeps running in the background, but the release it performs
// is incomplete, and a caller that needs to know the difference between "done"
// and "still going" must not read that as success.
var ErrTeardownTimeout = errors.New("teardown step did not finish within its grace")

// boundedClose runs one teardown step and waits at most grace for it.
//
// A close stuck in a network write also holds the SSH channel's write lock, so
// waiting for it in-line pins the caller behind a remote that stopped reading.
// Past the grace the step is left to finish on its own and teardown proceeds:
// the backend is already marked closed and its output is aborted, so nothing
// that follows can treat it as live, while a disconnect or a shutdown stays
// responsive. The expiry is reported, not swallowed, so a shutdown can tell a
// complete release from one that ran out of time.
func boundedClose(grace time.Duration, release func() error) error {
	done := make(chan error, 1)
	go func() { done <- release() }()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("%w after %s", ErrTeardownTimeout, grace)
	}
}

func (b *interactiveBackend) completion() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closeDone == nil {
		b.closeDone = make(chan struct{})
	}
	return b.closeDone
}

func (b *interactiveBackend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.closeDone == nil {
		b.closeDone = make(chan struct{})
	}
	done := b.closeDone
	stdin := b.stdin
	b.stdin = nil
	pump := b.pump
	session := b.session
	slot := b.inputSlot
	b.mu.Unlock()

	// Abort first so attach relays see end-of-stream, then close the stdin writer
	// and the SSH session to release any reader still blocked in Read.
	if pump != nil {
		pump.Abort()
	}
	// The two closes run concurrently. Closing the stdin writer can block in a
	// network write when the remote stopped reading, and the SSH session close is
	// exactly what releases it: running them in sequence would let the blocked
	// one hold the other back, so neither could finish.
	var steps []func() error
	if stdin != nil {
		steps = append(steps, stdin.Close)
	}
	if session != nil {
		steps = append(steps, session.Close)
	}
	return boundedClose(teardownGrace, func() error {
		defer close(done)
		err := closeConcurrently(steps...)
		if pump != nil {
			pump.wg.Wait()
		}
		// Successful release also waits for the backend's actual writer. A close
		// that fails to unblock stdin cannot be mistaken for completed teardown.
		if slot != nil {
			slot <- struct{}{}
			<-slot
		}
		return err
	})
}

// closeConcurrently runs independent close steps together and joins their
// results. The steps of one teardown release each other: closing the SSH session
// is what unblocks a stdin write stuck in the network, so running them in
// sequence would let the blocked step hold back the very close that frees it.
// Nil steps are skipped, so a backend that owns only some of the handles still
// closes what it has.
func closeConcurrently(steps ...func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(steps))
	for i, step := range steps {
		if step == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = step()
			if errors.Is(errs[i], io.EOF) || errors.Is(errs[i], net.ErrClosed) {
				errs[i] = nil
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// CloseStdin closes the remote stdin once. Repeated close_stdin controls are
// no-ops rather than double closes.
//
// The handle is detached under the lock but closed outside it: a stdin close
// that blocks on the network must not hold the backend lock, or a concurrent
// Close could not release it.
func (b *interactiveBackend) CloseStdin() error {
	b.mu.Lock()
	stdin := b.stdin
	b.stdin = nil
	b.mu.Unlock()
	if stdin != nil {
		return stdin.Close()
	}
	return nil
}

// stdinWriter returns the session's current stdin handle, or nil once it has
// been closed. Callers must not cache it: an attachment's worker reads it per
// write so a close_stdin ends its input instead of writing to a dead handle.
func (b *interactiveBackend) stdinWriter() io.WriteCloser {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stdin
}

// WindowChange and Signal hand the session pointer to the SSH library outside
// the backend lock for the same reason: the lock must stay available to Close so
// teardown can release an operation stuck in a network write.
func (b *interactiveBackend) WindowChange(rows, cols int) error {
	b.mu.Lock()
	session := b.session
	b.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.WindowChange(rows, cols)
}

func (b *interactiveBackend) Signal(signal string) error {
	b.mu.Lock()
	session := b.session
	b.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Signal(ssh.Signal(signal))
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
	if event.Warning != nil {
		warning := *event.Warning
		out.Warning = &warning
	}
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
