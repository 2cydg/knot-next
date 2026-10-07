package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"

	pkgsftp "github.com/pkg/sftp"
)

var (
	ErrNotFound   = errors.New("sftp resource not found")
	ErrValidation = errors.New("sftp validation failed")
	ErrConflict   = errors.New("sftp conflict")
)

const (
	maxSessions         = 1024
	maxTransfers        = 4096
	challengeTimeout    = 60 * time.Second
	maxAuthRetryCount   = 3
	maxSessionSubs      = 16
	maxTransferSubs     = 16
	progressEmitEvery   = 250 * time.Millisecond
	defaultSessionState = "connecting"
)

type configProvider interface {
	RuntimeConfig() (config.RuntimeConfig, error)
	SetServerPassword(id string, password string) (config.ServerProfileView, error)
	UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error)
}

type sessionProvider interface {
	Get(id string) (session.Resource, error)
	SubscribeCWD(id string) (<-chan session.CWDNotify, func(), session.Resource, error)
}

type Service struct {
	mu           sync.RWMutex
	root         string
	nextID       int64
	sessions     map[string]*resource
	transfers    map[string]*transferState
	subs         map[string]map[chan Event]struct{}
	transferSubs map[string]map[chan TransferEvent]struct{}
	config       configProvider
	session      sessionProvider
	pool         *sshpool.Pool
	testMode     bool
	onEvent      func(Event)
}

type resource struct {
	Session
	client           *pkgsftp.Client
	poolKeys         []string
	followCancel     func()
	hostKeyChallenge *pendingChallenge
	authChallenge    *pendingChallenge
	authRetryCount   int
	cancel           context.CancelFunc
	cache            *remoteDirCache
}

// backendView is captured under Service.mu and remains immutable during I/O.
// Closing the session closes the same client, but cannot change this view's backend.
type backendView struct {
	ID     string
	Root   string
	client *pkgsftp.Client
	cache  *remoteDirCache
}

func (r *resource) backendLocked() *backendView {
	return &backendView{ID: r.ID, Root: r.Root, client: r.client, cache: r.cache}
}

type pendingChallenge struct {
	Challenge Challenge
	response  chan ChallengeResponse
}

type transferState struct {
	Transfer
	cancel context.CancelFunc
}

// transferWork is a private copy used only by a single worker goroutine.
// It does not contain cancel or shared locks.
type transferWork struct {
	Transfer
}

func NewService(root string) *Service {
	return &Service{
		root:         root,
		nextID:       1,
		sessions:     map[string]*resource{},
		transfers:    map[string]*transferState{},
		subs:         map[string]map[chan Event]struct{}{},
		transferSubs: map[string]map[chan TransferEvent]struct{}{},
	}
}

func (s *Service) UseConfig(configService configProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = configService
}

func (s *Service) UseSession(sessionService sessionProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = sessionService
}

func (s *Service) UsePool(pool *sshpool.Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pool = pool
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
	ServerRef       string `json:"server_ref"`
	Alias           string `json:"alias,omitempty"`
	FollowSessionID string `json:"follow_session_id,omitempty"`
	HostKeyPolicy   string `json:"host_key_policy,omitempty"`
	AgentSocket     string `json:"agent_socket,omitempty"`
	AllowAuthRetry  bool   `json:"allow_auth_retry,omitempty"`
}

type Session struct {
	ID                string     `json:"id"`
	ServerRef         string     `json:"server_ref"`
	Alias             string     `json:"alias,omitempty"`
	State             string     `json:"state"`
	Backend           string     `json:"backend"`
	Root              string     `json:"root,omitempty"`
	CurrentDir        string     `json:"current_dir,omitempty"`
	FollowSessionID   string     `json:"follow_session_id,omitempty"`
	HostKeyPolicy     string     `json:"host_key_policy,omitempty"`
	HostKeyPending    bool       `json:"host_key_pending"`
	AuthPending       bool       `json:"auth_pending"`
	DisconnectCause   string     `json:"disconnect_cause,omitempty"`
	EventsURL         string     `json:"events_url"`
	TransferEventsURL string     `json:"transfer_events_url"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
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
	Accept   bool   `json:"accept,omitempty"`
	Abort    bool   `json:"abort,omitempty"`
	Password string `json:"password,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	Remember bool   `json:"remember,omitempty"`
}

type Event struct {
	Type        string     `json:"type"`
	SessionID   string     `json:"session_id"`
	State       string     `json:"state,omitempty"`
	Path        string     `json:"path,omitempty"`
	Error       string     `json:"error,omitempty"`
	TransferID  string     `json:"transfer_id,omitempty"`
	Direction   string     `json:"direction,omitempty"`
	BytesTotal  int64      `json:"bytes_total,omitempty"`
	BytesCopied int64      `json:"bytes_copied,omitempty"`
	FilesTotal  int        `json:"files_total,omitempty"`
	FilesDone   int        `json:"files_done,omitempty"`
	CurrentPath string     `json:"current_path,omitempty"`
	Challenge   *Challenge `json:"challenge,omitempty"`
	Time        time.Time  `json:"time"`
}

type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
}

type ListOptions struct {
	ShowHidden bool
	Sort       string
	Limit      int
	Offset     int
	Cache      bool
}

type TransferRequest struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Overwrite bool   `json:"overwrite,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}

type BatchTransferRequest struct {
	Sources     []string `json:"sources"`
	Target      string   `json:"target"`
	Overwrite   bool     `json:"overwrite,omitempty"`
	Recursive   bool     `json:"recursive,omitempty"`
	IncludeDirs bool     `json:"include_dirs,omitempty"`
}

type Transfer struct {
	ID          string         `json:"id"`
	SessionID   string         `json:"session_id"`
	Direction   string         `json:"direction"`
	Source      string         `json:"source,omitempty"`
	Target      string         `json:"target,omitempty"`
	State       string         `json:"state"`
	BytesTotal  int64          `json:"bytes_total"`
	BytesCopied int64          `json:"bytes_copied"`
	FilesTotal  int            `json:"files_total"`
	FilesDone   int            `json:"files_done"`
	CurrentPath string         `json:"current_path,omitempty"`
	Error       string         `json:"error,omitempty"`
	Items       []TransferItem `json:"items,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at,omitempty"`
}

type TransferItem struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	BytesTotal  int64  `json:"bytes_total,omitempty"`
	BytesCopied int64  `json:"bytes_copied,omitempty"`
	FilesTotal  int    `json:"files_total,omitempty"`
	FilesDone   int    `json:"files_done,omitempty"`
}

type TransferEvent struct {
	Type        string    `json:"type"`
	SessionID   string    `json:"session_id"`
	TransferID  string    `json:"transfer_id,omitempty"`
	Direction   string    `json:"direction,omitempty"`
	State       string    `json:"state,omitempty"`
	BytesTotal  int64     `json:"bytes_total,omitempty"`
	BytesCopied int64     `json:"bytes_copied,omitempty"`
	FilesTotal  int       `json:"files_total,omitempty"`
	FilesDone   int       `json:"files_done,omitempty"`
	CurrentPath string    `json:"current_path,omitempty"`
	Error       string    `json:"error,omitempty"`
	Time        time.Time `json:"time"`
}

type fileJob struct {
	source string
	target string
	size   int64
	mode   os.FileMode
}

type dirJob struct {
	target string
	mode   os.FileMode
}

type transferPlan struct {
	dirs       []dirJob
	files      []fileJob
	bytesTotal int64
	filesTotal int
}

type remoteEntryType int

const (
	remoteEntryMissing remoteEntryType = iota
	remoteEntryFile
	remoteEntryDir
)

type uploadSource struct {
	path         string
	displayPath  string
	stat         os.FileInfo
	copyContents bool
	hadTrailing  bool
}

type remotePathResolver interface {
	Stat(string) (os.FileInfo, error)
	MkdirAll(string) error
}

type localPathResolver struct {
	root string
}

func (r localPathResolver) Stat(name string) (os.FileInfo, error) {
	local, err := r.resolve(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(local)
}

func (r localPathResolver) MkdirAll(name string) error {
	local, err := r.resolve(name)
	if err != nil {
		return err
	}
	return os.MkdirAll(local, 0o755)
}

func (r localPathResolver) resolve(name string) (string, error) {
	clean := cleanRemote(name)
	local := filepath.Join(r.root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if err := ensureInsideRoot(r.root, local); err != nil {
		return "", err
	}
	return local, nil
}

func (s *Service) Create(req CreateRequest) (Session, error) {
	if strings.TrimSpace(req.ServerRef) == "" {
		return Session{}, fmt.Errorf("%w: server_ref is required", ErrValidation)
	}
	cfgProvider, sessionService, pool, testMode := s.dependencies()
	if !testMode && cfgProvider == nil {
		return Session{}, fmt.Errorf("%w: config service is not available", ErrConflict)
	}

	currentDir := "/"
	if req.FollowSessionID != "" {
		if sessionService == nil {
			return Session{}, fmt.Errorf("%w: session service is not available for follow_session_id", ErrConflict)
		}
		followed, err := sessionService.Get(req.FollowSessionID)
		if err != nil {
			return Session{}, err
		}
		if followed.ServerRef != req.ServerRef && followed.Alias != req.ServerRef {
			return Session{}, fmt.Errorf("%w: follow session does not match sftp server", ErrValidation)
		}
		if followed.CurrentDir != "" {
			currentDir = followed.CurrentDir
		}
	}

	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if len(s.sessions) >= maxSessions {
		s.mu.Unlock()
		cancel()
		return Session{}, fmt.Errorf("%w: sftp session limit reached", ErrConflict)
	}
	id := strconv.FormatInt(s.nextID, 10)
	s.nextID++
	res := &resource{
		Session: Session{
			ID:                id,
			ServerRef:         req.ServerRef,
			Alias:             req.Alias,
			State:             defaultSessionState,
			Backend:           "ssh-sftp",
			CurrentDir:        currentDir,
			FollowSessionID:   req.FollowSessionID,
			HostKeyPolicy:     req.HostKeyPolicy,
			EventsURL:         "/v1/sftp/" + id + "/events",
			TransferEventsURL: "/v1/sftp/" + id + "/transfers/events",
			CreatedAt:         now,
			UpdatedAt:         now,
		},
		cancel: cancel,
	}
	if testMode {
		res.Backend = "local-sandbox"
		res.State = "open"
		res.Root = filepath.Join(s.root, "sessions", id)
	}
	s.sessions[id] = res
	s.publishSessionLocked(res, Event{Type: "sftp.session.created", SessionID: id, State: res.State, Time: now})
	s.mu.Unlock()

	if testMode {
		if err := os.MkdirAll(res.Root, 0o700); err != nil {
			s.cleanupCreateFailure(id)
			return Session{}, err
		}
		if err := s.beginFollow(id, req.FollowSessionID); err != nil {
			s.cleanupCreateFailure(id)
			return Session{}, err
		}
		s.mu.Lock()
		if current, ok := s.sessions[id]; ok {
			current.UpdatedAt = now
			s.publishSessionLocked(current, Event{Type: "sftp.session.opened", SessionID: id, State: current.State, Time: now})
			s.mu.Unlock()
			return current.snapshot(), nil
		}
		s.mu.Unlock()
		return Session{}, ErrNotFound
	}

	if err := s.beginFollow(id, req.FollowSessionID); err != nil {
		s.cleanupCreateFailure(id)
		return Session{}, err
	}
	go s.connectSession(ctx, id, req, cfgProvider, pool)
	return res.snapshot(), nil
}

func (s *Service) cleanupCreateFailure(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if res, ok := s.sessions[id]; ok {
		if res.cancel != nil {
			res.cancel()
		}
		if res.followCancel != nil {
			res.followCancel()
		}
		delete(s.sessions, id)
	}
}

func (s *Service) beginFollow(id string, followID string) error {
	if followID == "" {
		return nil
	}
	s.mu.RLock()
	sessionService := s.session
	s.mu.RUnlock()
	if sessionService == nil {
		return fmt.Errorf("%w: session service is not available for follow_session_id", ErrConflict)
	}
	ch, cancel, followed, err := sessionService.SubscribeCWD(followID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	res, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		cancel()
		return ErrNotFound
	}
	if followed.CurrentDir != "" {
		res.CurrentDir = followed.CurrentDir
	}
	res.followCancel = cancel
	s.mu.Unlock()
	go s.followSessionCWD(id, ch)
	return nil
}

func (s *Service) connectSession(ctx context.Context, id string, req CreateRequest, cfgProvider configProvider, pool *sshpool.Pool) {
	runtimeCfg, err := cfgProvider.RuntimeConfig()
	if err != nil {
		s.failSession(id, err, config.RuntimeConfig{})
		return
	}
	server, err := resolveServer(runtimeCfg, req.ServerRef)
	if err != nil {
		s.failSession(id, err, runtimeCfg)
		return
	}
	if req.Alias == "" {
		req.Alias = server.Alias
		s.mu.Lock()
		if res, ok := s.sessions[id]; ok {
			res.Alias = server.Alias
		}
		s.mu.Unlock()
	}
	client, poolKeys, finalCfg, err := s.openRemoteClient(ctx, id, req, server, runtimeCfg, pool)
	if err != nil {
		s.failSession(id, err, finalCfg)
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.sessions[id]
	if !ok {
		_ = client.Close()
		pool.DecRef(poolKeys...)
		return
	}
	if res.State == "closed" || res.State == "disconnected" || res.State == "failed" {
		_ = client.Close()
		pool.DecRef(poolKeys...)
		return
	}
	res.client = client
	res.poolKeys = cloneStrings(poolKeys)
	res.cache = newRemoteDirCache(client, remoteDirCacheTTL)
	res.State = "open"
	res.UpdatedAt = now
	s.publishSessionLocked(res, Event{Type: "sftp.session.opened", SessionID: id, State: res.State, Time: now})
}

func (s *Service) openRemoteClient(ctx context.Context, sessionID string, req CreateRequest, server config.ServerProfile, cfg config.RuntimeConfig, pool *sshpool.Pool) (*pkgsftp.Client, []string, config.RuntimeConfig, error) {
	currentCfg := cfg
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
			AgentSocket:   req.AgentSocket,
			HostKeyPolicy: req.HostKeyPolicy,
		})
		if err != nil {
			if errors.Is(err, sshpool.ErrHostKeyReject) {
				return nil, nil, currentCfg, err
			}
			if req.AllowAuthRetry && sshpool.IsAuthError(err) && attempt < maxAuthRetryCount {
				nextCfg, retryErr := s.waitAuthResponse(ctx, sessionID, server, currentCfg, err, attempt+1)
				if retryErr != nil {
					return nil, nil, currentCfg, retryErr
				}
				currentCfg = nextCfg
				server = currentCfg.Servers[server.ID]
				continue
			}
			return nil, nil, currentCfg, err
		}
		pool.IncRef(poolKeys...)
		sftpClient, err := pkgsftp.NewClient(client)
		if err != nil {
			pool.DecRef(poolKeys...)
			return nil, nil, currentCfg, err
		}
		return sftpClient, poolKeys, currentCfg, nil
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
	res, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	res.hostKeyChallenge = challenge
	res.HostKeyPending = true
	res.State = "connecting"
	res.UpdatedAt = now
	s.publishSessionLocked(res, Event{
		Type:      "sftp.host_key.challenge",
		SessionID: sessionID,
		State:     res.State,
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
	res, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return cfg, ErrNotFound
	}
	res.authRetryCount = retryCount
	res.authChallenge = challenge
	res.AuthPending = true
	res.State = "connecting"
	res.UpdatedAt = now
	s.publishSessionLocked(res, Event{
		Type:      "sftp.auth.challenge",
		SessionID: sessionID,
		State:     res.State,
		Challenge: cloneChallengePtr(&challenge.Challenge),
		Time:      now,
	})
	s.mu.Unlock()

	timer := time.NewTimer(challengeTimeout)
	defer timer.Stop()
	select {
	case resp := <-challenge.response:
		if resp.Abort {
			return cfg, fmt.Errorf("%w: authentication retry aborted", ErrConflict)
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
	res, ok := s.sessions[id]
	if !ok {
		return Challenge{}, ErrNotFound
	}
	var pending *pendingChallenge
	if typ == "host_key" {
		pending = res.hostKeyChallenge
	} else {
		pending = res.authChallenge
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
	res, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return Challenge{}, ErrNotFound
	}
	var pending *pendingChallenge
	if typ == "host_key" {
		pending = res.hostKeyChallenge
	} else {
		pending = res.authChallenge
	}
	if pending == nil {
		s.mu.Unlock()
		return Challenge{SessionID: id, Type: typ, Pending: false}, nil
	}
	challenge := pending.Challenge
	if typ == "host_key" {
		res.hostKeyChallenge = nil
		res.HostKeyPending = false
	} else {
		res.authChallenge = nil
		res.AuthPending = false
	}
	res.UpdatedAt = time.Now().UTC()
	s.publishSessionLocked(res, Event{Type: "sftp.challenge.resolved", SessionID: id, State: res.State, Challenge: &challenge, Time: res.UpdatedAt})
	s.mu.Unlock()
	select {
	case pending.response <- resp:
	default:
	}
	challenge.Pending = false
	challenge.UpdatedAt = time.Now().UTC()
	return challenge, nil
}

func (s *Service) failPendingChallenge(sessionID string, typ string, message string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.sessions[sessionID]
	if !ok {
		return
	}
	if typ == "host_key" {
		res.hostKeyChallenge = nil
		res.HostKeyPending = false
	} else {
		res.authChallenge = nil
		res.AuthPending = false
	}
	res.State = "failed"
	res.DisconnectCause = message
	res.ClosedAt = &now
	res.UpdatedAt = now
	s.publishSessionLocked(res, Event{Type: "sftp.error", SessionID: sessionID, State: res.State, Error: message, Time: now})
}

func (s *Service) failSession(id string, err error, cfg config.RuntimeConfig) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.sessions[id]
	if !ok {
		return
	}
	if res.followCancel != nil {
		res.followCancel()
		res.followCancel = nil
	}
	res.State = "failed"
	res.DisconnectCause = safeError(err, cfg)
	res.ClosedAt = &now
	res.UpdatedAt = now
	s.publishSessionLocked(res, Event{Type: "sftp.error", SessionID: id, State: res.State, Error: res.DisconnectCause, Time: now})
}

func (s *Service) Get(id string) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res, ok := s.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return res.snapshot(), nil
}

func (s *Service) ListSessions() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Session, 0, len(s.sessions))
	for _, res := range s.sessions {
		out = append(out, res.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *Service) Close(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	if res.State == "closed" {
		return res.snapshot(), nil
	}
	now := time.Now().UTC()
	s.closeSessionLocked(res, "closed", "client requested close", now)
	return res.snapshot(), nil
}

func (s *Service) CloseByPoolKey(poolKey string) []Session {
	if poolKey == "" {
		return nil
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	closed := make([]Session, 0)
	for _, res := range s.sessions {
		if (res.State == "closed" || res.State == "failed" || res.State == "disconnected") || !containsString(res.poolKeys, poolKey) {
			continue
		}
		s.closeSessionLocked(res, "disconnected", "ssh pool disconnected", now)
		closed = append(closed, res.snapshot())
	}
	return closed
}

func (s *Service) closeSessionLocked(res *resource, state string, cause string, now time.Time) {
	if res.cancel != nil {
		res.cancel()
	}
	if res.followCancel != nil {
		res.followCancel()
		res.followCancel = nil
	}
	// Signal transfer cancellation before closing the transport: pending I/O may
	// return immediately with a connection error once the client is closed.
	s.cancelSessionTransfersLocked(res.ID)
	if res.client != nil {
		_ = res.client.Close()
		res.client = nil
	}
	if len(res.poolKeys) > 0 && s.pool != nil {
		s.pool.DecRef(res.poolKeys...)
		res.poolKeys = nil
	}
	res.State = state
	res.DisconnectCause = cause
	res.UpdatedAt = now
	res.ClosedAt = &now
	eventType := "sftp.session.closed"
	if state == "disconnected" {
		eventType = "sftp.session.disconnected"
	}
	s.publishSessionLocked(res, Event{Type: eventType, SessionID: res.ID, State: state, Error: cause, Time: now})
	for ch := range s.subs[res.ID] {
		close(ch)
	}
	delete(s.subs, res.ID)
	for ch := range s.transferSubs[res.ID] {
		close(ch)
	}
	delete(s.transferSubs, res.ID)
}

func (s *Service) cancelSessionTransfersLocked(sessionID string) {
	for _, transfer := range s.transfers {
		if transfer.SessionID != sessionID {
			continue
		}
		if transfer.State == "queued" || transfer.State == "running" {
			transfer.cancel()
		}
	}
}

func (s *Service) Subscribe(id string) (<-chan Event, func(), Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.sessions[id]
	if !ok {
		return nil, nil, Session{}, ErrNotFound
	}
	ch := make(chan Event, maxSessionSubs)
	if s.subs[id] == nil {
		s.subs[id] = map[chan Event]struct{}{}
	}
	s.subs[id][ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if subs := s.subs[id]; subs != nil {
			if _, ok := subs[ch]; ok {
				delete(subs, ch)
				close(ch)
			}
		}
	}
	return ch, cancel, res.snapshot(), nil
}

func (s *Service) SubscribeTransfers(sessionID string) (<-chan TransferEvent, func(), error) {
	ch, cancel, _, err := s.SubscribeTransfersWithSnapshot(sessionID)
	return ch, cancel, err
}

func (s *Service) SubscribeTransfersWithSnapshot(sessionID string) (<-chan TransferEvent, func(), []Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil, nil, ErrNotFound
	}
	if res.State == "closed" || res.State == "failed" || res.State == "disconnected" {
		return nil, nil, nil, fmt.Errorf("%w: sftp session is not open", ErrConflict)
	}

	// Register subscriber
	ch := make(chan TransferEvent, maxTransferSubs)
	if s.transferSubs[sessionID] == nil {
		s.transferSubs[sessionID] = map[chan TransferEvent]struct{}{}
	}
	s.transferSubs[sessionID][ch] = struct{}{}

	// Build snapshot of current transfers for this session
	snapshot := make([]Transfer, 0)
	for _, transfer := range s.transfers {
		if transfer.SessionID == sessionID {
			snapshot = append(snapshot, transfer.snapshot())
		}
	}
	// Sort by start time, same as ListTransfers
	sort.Slice(snapshot, func(i, j int) bool {
		return snapshot[i].StartedAt.Before(snapshot[j].StartedAt)
	})

	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if subs := s.transferSubs[sessionID]; subs != nil {
			if _, ok := subs[ch]; ok {
				delete(subs, ch)
				close(ch)
			}
		}
	}
	return ch, cancel, snapshot, nil
}

func (s *Service) SessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, res := range s.sessions {
		if res.State == "connecting" || res.State == "open" {
			count++
		}
	}
	return count
}

func (s *Service) RunningTransferCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, transfer := range s.transfers {
		if transfer.State == "running" {
			count++
		}
	}
	return count
}

func (s *Service) List(id string, remotePath string, opts ListOptions) ([]Entry, error) {
	res, clean, err := s.requireOpenSession(id, remotePath)
	if err != nil {
		return nil, err
	}
	var entries []os.FileInfo
	if res.client != nil {
		if opts.Cache && res.cache != nil {
			entries, err = res.cache.ReadDir(clean)
		} else {
			entries, err = res.client.ReadDir(clean)
		}
		if err != nil {
			return nil, err
		}
	} else {
		local, _, err := s.resolve(id, clean)
		if err != nil {
			return nil, err
		}
		dirEntries, err := os.ReadDir(local)
		if err != nil {
			return nil, err
		}
		entries = make([]os.FileInfo, 0, len(dirEntries))
		for _, entry := range dirEntries {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			entries = append(entries, info)
		}
	}

	out := make([]Entry, 0, len(entries))
	for _, info := range entries {
		if !opts.ShowHidden && strings.HasPrefix(info.Name(), ".") {
			continue
		}
		out = append(out, entryFromInfo(path.Join(clean, info.Name()), info))
	}
	sortEntries(out, opts.Sort)
	return paginateEntries(out, opts.Offset, opts.Limit), nil
}

func (s *Service) Stat(id string, remotePath string) (Entry, error) {
	res, clean, err := s.requireOpenSession(id, remotePath)
	if err != nil {
		return Entry{}, err
	}
	if res.client != nil {
		info, err := res.client.Stat(clean)
		if err != nil {
			return Entry{}, err
		}
		return entryFromInfo(clean, info), nil
	}
	local, _, err := s.resolve(id, clean)
	if err != nil {
		return Entry{}, err
	}
	info, err := os.Stat(local)
	if err != nil {
		return Entry{}, err
	}
	return entryFromInfo(clean, info), nil
}

func (s *Service) Mkdir(id string, remotePath string, recursive bool) (Entry, error) {
	res, clean, err := s.requireOpenSession(id, remotePath)
	if err != nil {
		return Entry{}, err
	}
	if res.client != nil {
		if recursive {
			err = res.client.MkdirAll(clean)
		} else {
			err = res.client.Mkdir(clean)
		}
		if err != nil {
			return Entry{}, err
		}
		res.cache.Invalidate(clean, path.Dir(clean))
		return s.Stat(id, clean)
	}
	local, _, err := s.resolve(id, clean)
	if err != nil {
		return Entry{}, err
	}
	if recursive {
		err = os.MkdirAll(local, 0o700)
	} else {
		err = os.Mkdir(local, 0o700)
	}
	if err != nil {
		return Entry{}, err
	}
	return s.Stat(id, clean)
}

func (s *Service) RemoveFile(id string, remotePath string) error {
	res, clean, err := s.requireOpenSession(id, remotePath)
	if err != nil {
		return err
	}
	if res.client != nil {
		info, err := res.client.Stat(clean)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%w: path is a directory", ErrValidation)
		}
		if err := res.client.Remove(clean); err != nil {
			return err
		}
		res.cache.Invalidate(clean, path.Dir(clean))
		return nil
	}
	local, _, err := s.resolve(id, clean)
	if err != nil {
		return err
	}
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%w: path is a directory", ErrValidation)
	}
	return os.Remove(local)
}

func (s *Service) RemoveDir(id string, remotePath string) error {
	res, clean, err := s.requireOpenSession(id, remotePath)
	if err != nil {
		return err
	}
	if res.client != nil {
		info, err := res.client.Stat(clean)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: path is not a directory", ErrValidation)
		}
		if err := res.client.RemoveDirectory(clean); err != nil {
			return err
		}
		res.cache.Invalidate(clean, path.Dir(clean))
		return nil
	}
	local, _, err := s.resolve(id, clean)
	if err != nil {
		return err
	}
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: path is not a directory", ErrValidation)
	}
	return os.Remove(local)
}

func (s *Service) Rename(id string, oldPath string, newPath string) (Entry, error) {
	res, oldClean, err := s.requireOpenSession(id, oldPath)
	if err != nil {
		return Entry{}, err
	}
	newClean := cleanRemote(newPath)
	if res.client != nil {
		if err := res.client.Rename(oldClean, newClean); err != nil {
			return Entry{}, err
		}
		res.cache.Invalidate(oldClean, newClean, path.Dir(oldClean), path.Dir(newClean))
		return s.Stat(id, newClean)
	}
	oldLocal, _, err := s.resolve(id, oldClean)
	if err != nil {
		return Entry{}, err
	}
	newLocal, _, err := s.resolve(id, newClean)
	if err != nil {
		return Entry{}, err
	}
	if err := os.Rename(oldLocal, newLocal); err != nil {
		return Entry{}, err
	}
	return s.Stat(id, newClean)
}

func (s *Service) Upload(id string, req TransferRequest) (Transfer, error) {
	if strings.TrimSpace(req.Source) == "" || strings.TrimSpace(req.Target) == "" {
		return Transfer{}, fmt.Errorf("%w: source and target are required", ErrValidation)
	}
	return s.startTransfer(id, "upload", req.Source, req.Target, nil, func(ctx context.Context, res *backendView, work *transferWork) (string, error) {
		return s.runUpload(ctx, res, work, req)
	})
}

func (s *Service) runUpload(ctx context.Context, res *backendView, work *transferWork, req TransferRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if res.client != nil {
		return s.runUploadRemote(ctx, res, work, req)
	}
	return s.runUploadLocal(ctx, res, work, req)
}

func (s *Service) Download(id string, req TransferRequest) (Transfer, error) {
	if strings.TrimSpace(req.Source) == "" || strings.TrimSpace(req.Target) == "" {
		return Transfer{}, fmt.Errorf("%w: source and target are required", ErrValidation)
	}
	return s.startTransfer(id, "download", req.Source, req.Target, nil, func(ctx context.Context, res *backendView, work *transferWork) (string, error) {
		return s.runDownload(ctx, res, work, req)
	})
}

func (s *Service) runDownload(ctx context.Context, res *backendView, work *transferWork, req TransferRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if res.client != nil {
		return s.runDownloadRemote(ctx, res, work, req)
	}
	return s.runDownloadLocal(ctx, res, work, req)
}

func (s *Service) BatchUpload(id string, req BatchTransferRequest) (Transfer, error) {
	if len(req.Sources) == 0 || strings.TrimSpace(req.Target) == "" {
		return Transfer{}, fmt.Errorf("%w: sources and target are required", ErrValidation)
	}
	items := make([]TransferItem, 0, len(req.Sources))
	for _, source := range req.Sources {
		items = append(items, TransferItem{Source: source, Target: req.Target, State: "queued"})
	}
	return s.startTransfer(id, "upload", "", req.Target, items, func(ctx context.Context, res *backendView, work *transferWork) (string, error) {
		return s.runBatchUpload(ctx, res, work, req)
	})
}

func (s *Service) BatchDownload(id string, req BatchTransferRequest) (Transfer, error) {
	if len(req.Sources) == 0 || strings.TrimSpace(req.Target) == "" {
		return Transfer{}, fmt.Errorf("%w: sources and target are required", ErrValidation)
	}
	items := make([]TransferItem, 0, len(req.Sources))
	for _, source := range req.Sources {
		items = append(items, TransferItem{Source: source, Target: req.Target, State: "queued"})
	}
	return s.startTransfer(id, "download", "", req.Target, items, func(ctx context.Context, res *backendView, work *transferWork) (string, error) {
		return s.runBatchDownload(ctx, res, work, req)
	})
}

func (s *Service) startTransfer(sessionID string, direction string, source string, target string, items []TransferItem, worker func(context.Context, *backendView, *transferWork) (string, error)) (Transfer, error) {
	s.mu.Lock()
	res, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return Transfer{}, ErrNotFound
	}
	if res.State != "open" {
		s.mu.Unlock()
		return Transfer{}, fmt.Errorf("%w: sftp session is not open", ErrConflict)
	}
	if len(s.transfers) >= maxTransfers {
		s.pruneTransfersLocked()
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	transfer := &transferState{
		Transfer: Transfer{
			ID:        "transfer_" + strconv.FormatInt(s.nextID, 10),
			SessionID: sessionID,
			Direction: direction,
			Source:    source,
			Target:    target,
			State:     "queued",
			Items:     cloneTransferItems(items),
			StartedAt: now,
		},
		cancel: cancel,
	}
	s.nextID++
	s.transfers[transfer.ID] = transfer
	s.publishTransferLocked(transfer, "sftp.transfer.queued", now)

	// Take snapshot before releasing lock and starting worker
	snapshot := transfer.snapshot()
	s.mu.Unlock()

	go s.executeTransfer(ctx, transfer.ID, worker)
	return snapshot, nil
}

func (s *Service) executeTransfer(ctx context.Context, transferID string, worker func(context.Context, *backendView, *transferWork) (string, error)) {
	res, work, ok := s.markTransferRunning(ctx, transferID)
	if !ok {
		return
	}
	finalState, err := worker(ctx, res, work)
	switch {
	case errors.Is(err, context.Canceled) || (err != nil && ctx.Err() != nil):
		s.finishTransferFromWorker(transferID, work, "canceled", "")
	case err != nil:
		if finalState == "" {
			finalState = "failed"
		}
		s.finishTransferFromWorker(transferID, work, finalState, err.Error())
	default:
		if finalState == "" {
			finalState = "completed"
		}
		s.finishTransferFromWorker(transferID, work, finalState, "")
	}
}

func (s *Service) markTransferRunning(ctx context.Context, transferID string) (*backendView, *transferWork, bool) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	transfer, ok := s.transfers[transferID]
	if !ok {
		return nil, nil, false
	}
	// Reject already terminal transfers
	if isTerminalState(transfer.State) {
		return nil, nil, false
	}
	res, ok := s.sessions[transfer.SessionID]
	if ctx.Err() != nil || !ok || res.State != "open" {
		for i := range transfer.Items {
			if !isTerminalState(transfer.Items[i].State) {
				transfer.Items[i].State = "canceled"
			}
		}
		s.finishTransferLocked(transfer, "canceled", "", now)
		return nil, nil, false
	}
	transfer.State = "running"
	transfer.StartedAt = now
	s.publishTransferLocked(transfer, "sftp.transfer.started", now)

	// Clone Items as well as the scalar fields before handing ownership to the worker.
	work := &transferWork{Transfer: transfer.snapshot()}
	return res.backendLocked(), work, true
}

func (s *Service) finishTransfer(id string, state string, message string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	transfer, ok := s.transfers[id]
	if !ok {
		return
	}
	// Terminal state protection: once in a terminal state, do not overwrite
	if isTerminalState(transfer.State) {
		return
	}
	s.finishTransferLocked(transfer, state, message, now)
}

// Caller holds Service.mu; final progress must already be merged.
func (s *Service) finishTransferLocked(transfer *transferState, state, message string, now time.Time) {
	transfer.State = state
	transfer.Error = message
	transfer.CompletedAt = now
	s.publishTransferLocked(transfer, "sftp.transfer."+state, now)
}

func isTerminalState(state string) bool {
	switch state {
	case "completed", "failed", "partial_failed", "canceled":
		return true
	default:
		return false
	}
}

// applyWorkerProgressLocked merges progress fields from worker's private copy
// into the shared transfer state. Caller must hold s.mu.
// Does not overwrite State, Error, or CompletedAt.
func (s *Service) applyWorkerProgressLocked(dst *transferState, src Transfer) {
	dst.BytesTotal = src.BytesTotal
	dst.BytesCopied = src.BytesCopied
	dst.FilesTotal = src.FilesTotal
	dst.FilesDone = src.FilesDone
	dst.CurrentPath = src.CurrentPath
	dst.Items = cloneTransferItems(src.Items)
}

// publishWorkerProgress publishes progress from worker's private copy to shared state.
// Called by the same worker that owns the transferWork.
func (s *Service) publishWorkerProgress(work *transferWork) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.transfers[work.ID]
	if !ok || isTerminalState(current.State) {
		return
	}
	s.applyWorkerProgressLocked(current, work.Transfer)
	s.publishTransferLocked(current, "sftp.transfer.progress", now)
}

// finishTransferFromWorker submits final progress and terminal state in one atomic operation.
// This ensures GET, snapshot, and events see consistent final values.
func (s *Service) finishTransferFromWorker(id string, work *transferWork, state string, message string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	transfer, ok := s.transfers[id]
	if !ok {
		return
	}
	// Terminal state protection: once in a terminal state, do not overwrite
	if isTerminalState(transfer.State) {
		return
	}
	// Merge final progress
	s.applyWorkerProgressLocked(transfer, work.Transfer)
	// Set terminal state
	s.finishTransferLocked(transfer, state, message, now)
}

func (s *Service) ListTransfers(sessionID string) ([]Transfer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	out := make([]Transfer, 0)
	for _, transfer := range s.transfers {
		if transfer.SessionID == sessionID {
			out = append(out, transfer.snapshot())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func (s *Service) Transfer(sessionID string, transferID string) (Transfer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	transfer, ok := s.transfers[transferID]
	if !ok || transfer.SessionID != sessionID {
		return Transfer{}, ErrNotFound
	}
	return transfer.snapshot(), nil
}

func (s *Service) CancelTransfer(sessionID string, transferID string) (Transfer, error) {
	s.mu.RLock()
	transfer, ok := s.transfers[transferID]
	if !ok || transfer.SessionID != sessionID {
		s.mu.RUnlock()
		return Transfer{}, ErrNotFound
	}
	snapshot := transfer.snapshot()
	cancel := transfer.cancel
	s.mu.RUnlock()
	if !isTerminalState(snapshot.State) {
		cancel()
	}
	return snapshot, nil
}

func (s *Service) GlobMatches(id string, pattern string, includeDirs bool, useCache bool) ([]Entry, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("%w: pattern is required", ErrValidation)
	}
	res, _, err := s.requireOpenSession(id, "/")
	if err != nil {
		return nil, err
	}
	return s.globMatches(res, pattern, includeDirs, useCache)
}

func (s *Service) globMatches(res *backendView, pattern string, includeDirs bool, useCache bool) ([]Entry, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("%w: pattern is required", ErrValidation)
	}
	dir := path.Dir(pattern)
	base := path.Base(pattern)
	var infos []os.FileInfo
	var err error
	if res.client != nil {
		if useCache && res.cache != nil {
			infos, err = res.cache.ReadDir(dir)
		} else {
			infos, err = res.client.ReadDir(dir)
		}
		if err != nil {
			return nil, err
		}
	} else {
		localDir, err := (localPathResolver{root: res.Root}).resolve(dir)
		if err != nil {
			return nil, err
		}
		dirEntries, err := os.ReadDir(localDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range dirEntries {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			infos = append(infos, info)
		}
	}
	matches := make([]Entry, 0)
	for _, info := range infos {
		if info.IsDir() && !includeDirs {
			continue
		}
		match, err := path.Match(base, info.Name())
		if err != nil {
			return nil, fmt.Errorf("%w: invalid glob pattern", ErrValidation)
		}
		if match {
			matches = append(matches, entryFromInfo(path.Join(dir, info.Name()), info))
		}
	}
	if len(matches) == 0 {
		return nil, os.ErrNotExist
	}
	sortEntries(matches, "name")
	return matches, nil
}

func (s *Service) runUploadLocal(ctx context.Context, res *backendView, work *transferWork, req TransferRequest) (string, error) {
	plan, err := planUploadLocal(req.Source, req.Target, req.Recursive, localPathResolver{root: res.Root})
	if err != nil {
		return "", err
	}
	s.seedPlanProgress(work, plan)
	return s.executeLocalUploadPlan(ctx, res, work, plan, req.Overwrite)
}

func (s *Service) runUploadRemote(ctx context.Context, res *backendView, work *transferWork, req TransferRequest) (string, error) {
	plan, err := planUploadLocal(req.Source, req.Target, req.Recursive, res.client)
	if err != nil {
		return "", err
	}
	s.seedPlanProgress(work, plan)
	return s.executeRemoteUploadPlan(ctx, res, work, plan, req.Overwrite)
}

func (s *Service) runDownloadLocal(ctx context.Context, res *backendView, work *transferWork, req TransferRequest) (string, error) {
	plan, err := s.planDownloadLocal(res, req.Source, req.Target, req.Recursive)
	if err != nil {
		return "", err
	}
	s.seedPlanProgress(work, plan)
	return s.executeLocalDownloadPlan(ctx, work, plan, req.Overwrite)
}

func (s *Service) runDownloadRemote(ctx context.Context, res *backendView, work *transferWork, req TransferRequest) (string, error) {
	plan, err := s.planDownloadRemote(res, req.Source, req.Target, req.Recursive)
	if err != nil {
		return "", err
	}
	s.seedPlanProgress(work, plan)
	return s.executeRemoteDownloadPlan(ctx, res, work, plan, req.Overwrite)
}

func (s *Service) runBatchUpload(ctx context.Context, res *backendView, work *transferWork, req BatchTransferRequest) (string, error) {
	return s.runBatch(ctx, work, work.Direction, len(req.Sources), func(index int) error {
		source := req.Sources[index]
		itemReq := TransferRequest{Source: source, Target: req.Target, Overwrite: req.Overwrite, Recursive: req.Recursive}
		if res.client != nil {
			return s.runBatchUploadItemRemote(ctx, res, work, index, itemReq)
		}
		return s.runBatchUploadItemLocal(ctx, res, work, index, itemReq)
	})
}

func (s *Service) runBatchDownload(ctx context.Context, res *backendView, work *transferWork, req BatchTransferRequest) (string, error) {
	expanded := make([][]Entry, len(req.Sources))
	for i, pattern := range req.Sources {
		if err := ctx.Err(); err != nil {
			for j := range work.Items {
				s.markTransferItemCanceled(work, j)
			}
			return "", err
		}
		matches, err := s.globMatches(res, pattern, req.IncludeDirs, true)
		if err != nil {
			if ctx.Err() != nil {
				for j := range work.Items {
					s.markTransferItemCanceled(work, j)
				}
				return "", ctx.Err()
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "failed", err
			}
			continue
		}
		expanded[i] = matches
	}
	return s.runBatch(ctx, work, work.Direction, len(req.Sources), func(index int) error {
		matches := expanded[index]
		if len(matches) == 0 {
			return os.ErrNotExist
		}
		var itemErr error
		for _, match := range matches {
			target := req.Target
			if target != "" && !localPathHasTrailingSeparator(target) {
				target += string(os.PathSeparator)
			}
			itemReq := TransferRequest{Source: match.Path, Target: target, Overwrite: req.Overwrite, Recursive: req.Recursive}
			if res.client != nil {
				itemErr = s.runBatchDownloadItemRemote(ctx, res, work, index, itemReq)
			} else {
				itemErr = s.runBatchDownloadItemLocal(ctx, res, work, index, itemReq)
			}
			if itemErr != nil {
				return itemErr
			}
		}
		return nil
	})
}

func (s *Service) runBatch(ctx context.Context, work *transferWork, direction string, total int, runItem func(int) error) (string, error) {
	failures := 0
	for i := 0; i < total; i++ {
		// Check context before each item
		select {
		case <-ctx.Done():
			// Mark all remaining items as canceled
			for j := i; j < total; j++ {
				s.markTransferItemCanceled(work, j)
			}
			return "", ctx.Err()
		default:
		}
		if err := runItem(i); err != nil {
			// Distinguish cancellation from regular failure
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				s.markTransferItemCanceled(work, i)
				// Mark remaining items as canceled
				for j := i + 1; j < total; j++ {
					s.markTransferItemCanceled(work, j)
				}
				return "", context.Canceled
			}
			failures++
			s.markTransferItemError(work, i, err)
			continue
		}
		s.markTransferItemCompleted(work, i)
	}
	switch {
	case failures == 0:
		return "completed", nil
	case failures == total:
		return "failed", fmt.Errorf("%w: all batch items failed", ErrConflict)
	default:
		return "partial_failed", fmt.Errorf("%w: one or more batch items failed", ErrConflict)
	}
}

func (s *Service) runBatchUploadItemLocal(ctx context.Context, res *backendView, work *transferWork, index int, req TransferRequest) error {
	plan, err := planUploadLocal(req.Source, req.Target, req.Recursive, localPathResolver{root: res.Root})
	if err != nil {
		return err
	}
	s.seedBatchItemPlan(work, index, plan)
	_, err = s.executeLocalUploadPlan(ctx, res, work, plan, req.Overwrite)
	return err
}

func (s *Service) runBatchUploadItemRemote(ctx context.Context, res *backendView, work *transferWork, index int, req TransferRequest) error {
	plan, err := planUploadLocal(req.Source, req.Target, req.Recursive, res.client)
	if err != nil {
		return err
	}
	s.seedBatchItemPlan(work, index, plan)
	_, err = s.executeRemoteUploadPlan(ctx, res, work, plan, req.Overwrite)
	return err
}

func (s *Service) runBatchDownloadItemLocal(ctx context.Context, res *backendView, work *transferWork, index int, req TransferRequest) error {
	plan, err := s.planDownloadLocal(res, req.Source, req.Target, req.Recursive)
	if err != nil {
		return err
	}
	s.seedBatchItemPlan(work, index, plan)
	_, err = s.executeLocalDownloadPlan(ctx, work, plan, req.Overwrite)
	return err
}

func (s *Service) runBatchDownloadItemRemote(ctx context.Context, res *backendView, work *transferWork, index int, req TransferRequest) error {
	plan, err := s.planDownloadRemote(res, req.Source, req.Target, req.Recursive)
	if err != nil {
		return err
	}
	s.seedBatchItemPlan(work, index, plan)
	_, err = s.executeRemoteDownloadPlan(ctx, res, work, plan, req.Overwrite)
	return err
}

func (s *Service) seedPlanProgress(work *transferWork, plan transferPlan) {
	work.BytesTotal += plan.bytesTotal
	work.FilesTotal += plan.filesTotal
	s.publishWorkerProgress(work)
}

func (s *Service) seedBatchItemPlan(work *transferWork, index int, plan transferPlan) {
	work.BytesTotal += plan.bytesTotal
	work.FilesTotal += plan.filesTotal
	if index >= 0 && index < len(work.Items) {
		work.Items[index].BytesTotal += plan.bytesTotal
		work.Items[index].FilesTotal += plan.filesTotal
		work.Items[index].State = "running"
	}
	s.publishWorkerProgress(work)
}

func (s *Service) markTransferItemError(work *transferWork, index int, err error) {
	if index >= 0 && index < len(work.Items) {
		work.Items[index].State = "failed"
		work.Items[index].Error = err.Error()
	}
	s.publishWorkerProgress(work)
}

func (s *Service) markTransferItemCompleted(work *transferWork, index int) {
	if index >= 0 && index < len(work.Items) {
		work.Items[index].State = "completed"
	}
	s.publishWorkerProgress(work)
}

func (s *Service) markTransferItemCanceled(work *transferWork, index int) {
	if index >= 0 && index < len(work.Items) {
		work.Items[index].State = "canceled"
	}
	s.publishWorkerProgress(work)
}

func (s *Service) executeLocalUploadPlan(ctx context.Context, res *backendView, work *transferWork, plan transferPlan, overwrite bool) (string, error) {
	for _, dir := range plan.dirs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		localTarget, err := localPathResolver{root: res.Root}.resolve(dir.target)
		if err != nil {
			return "", err
		}
		mode := dir.mode.Perm()
		if mode == 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(localTarget, mode); err != nil {
			return "", err
		}
	}
	for _, file := range plan.files {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		localTarget, err := localPathResolver{root: res.Root}.resolve(file.target)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(localTarget), 0o755); err != nil {
			return "", err
		}
		if !overwrite {
			if _, err := os.Stat(localTarget); err == nil {
				return "", fmt.Errorf("%w: target already exists", ErrConflict)
			}
		}
		src, err := os.Open(file.source)
		if err != nil {
			return "", err
		}
		flag := os.O_WRONLY | os.O_CREATE
		if overwrite {
			flag |= os.O_TRUNC
		} else {
			flag |= os.O_EXCL
		}
		dst, err := os.OpenFile(localTarget, flag, file.mode.Perm())
		if err != nil {
			_ = src.Close()
			if errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("%w: target already exists", ErrConflict)
			}
			return "", err
		}
		if err := s.copyWithProgress(ctx, work, file.target, src, dst, file.size, 0); err != nil {
			_ = src.Close()
			_ = dst.Close()
			return "", err
		}
		_ = src.Close()
		_ = dst.Close()
		work.FilesDone++
		s.publishWorkerProgress(work)
	}
	return "completed", nil
}

func (s *Service) executeRemoteUploadPlan(ctx context.Context, res *backendView, work *transferWork, plan transferPlan, overwrite bool) (string, error) {
	for _, dir := range plan.dirs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := res.client.MkdirAll(dir.target); err != nil {
			return "", err
		}
	}
	for _, file := range plan.files {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		src, err := os.Open(file.source)
		if err != nil {
			return "", err
		}
		if !overwrite {
			if _, err := res.client.Stat(file.target); err == nil {
				_ = src.Close()
				return "", fmt.Errorf("%w: target already exists", ErrConflict)
			}
		}
		if err := res.client.MkdirAll(path.Dir(file.target)); err != nil {
			_ = src.Close()
			return "", err
		}
		flag := os.O_WRONLY | os.O_CREATE
		if overwrite {
			flag |= os.O_TRUNC
		} else {
			flag |= os.O_EXCL
		}
		dst, err := res.client.OpenFile(file.target, flag)
		if err != nil {
			_ = src.Close()
			if errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("%w: target already exists", ErrConflict)
			}
			return "", err
		}
		if err := s.copyWithProgress(ctx, work, file.target, src, dst, file.size, 0); err != nil {
			_ = src.Close()
			_ = dst.Close()
			return "", err
		}
		_ = src.Close()
		_ = dst.Close()
		work.FilesDone++
		s.publishWorkerProgress(work)
	}
	if res.cache != nil {
		res.cache.Invalidate(path.Dir(work.Target), work.Target)
	}
	return "completed", nil
}

func (s *Service) executeLocalDownloadPlan(ctx context.Context, work *transferWork, plan transferPlan, overwrite bool) (string, error) {
	for _, dir := range plan.dirs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		mode := dir.mode.Perm()
		if mode == 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(dir.target, mode); err != nil {
			return "", err
		}
	}
	for _, file := range plan.files {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if err := os.MkdirAll(filepath.Dir(file.target), 0o755); err != nil {
			return "", err
		}
		src, err := os.Open(file.source)
		if err != nil {
			return "", err
		}
		flag := os.O_WRONLY | os.O_CREATE
		if overwrite {
			flag |= os.O_TRUNC
		} else {
			flag |= os.O_EXCL
		}
		dst, err := os.OpenFile(file.target, flag, file.mode.Perm())
		if err != nil {
			_ = src.Close()
			if errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("%w: target already exists", ErrConflict)
			}
			return "", err
		}
		if err := s.copyWithProgress(ctx, work, file.source, src, dst, file.size, file.mode); err != nil {
			_ = src.Close()
			_ = dst.Close()
			return "", err
		}
		_ = src.Close()
		_ = dst.Close()
		work.FilesDone++
		s.publishWorkerProgress(work)
	}
	return "completed", nil
}

func (s *Service) executeRemoteDownloadPlan(ctx context.Context, res *backendView, work *transferWork, plan transferPlan, overwrite bool) (string, error) {
	for _, dir := range plan.dirs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		mode := dir.mode.Perm()
		if mode == 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(dir.target, mode); err != nil {
			return "", err
		}
	}
	for _, file := range plan.files {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		src, err := res.client.Open(file.source)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(file.target), 0o755); err != nil {
			_ = src.Close()
			return "", err
		}
		flag := os.O_WRONLY | os.O_CREATE
		if overwrite {
			flag |= os.O_TRUNC
		} else {
			flag |= os.O_EXCL
		}
		dst, err := os.OpenFile(file.target, flag, file.mode.Perm())
		if err != nil {
			_ = src.Close()
			if errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("%w: target already exists", ErrConflict)
			}
			return "", err
		}
		if err := s.copyWithProgress(ctx, work, file.source, src, dst, file.size, file.mode); err != nil {
			_ = src.Close()
			_ = dst.Close()
			return "", err
		}
		_ = src.Close()
		_ = dst.Close()
		work.FilesDone++
		s.publishWorkerProgress(work)
	}
	return "completed", nil
}

func (s *Service) copyWithProgress(ctx context.Context, work *transferWork, currentPath string, src io.Reader, dst io.Writer, total int64, mode os.FileMode) error {
	buf := make([]byte, 32*1024)
	last := time.Time{}
	work.CurrentPath = currentPath
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				work.BytesCopied += int64(nw)
				if mode != 0 {
					_ = os.Chmod(currentPath, mode.Perm())
				}
				now := time.Now().UTC()
				if last.IsZero() || now.Sub(last) >= progressEmitEvery || work.BytesCopied >= total {
					last = now
					s.publishWorkerProgress(work)
				}
			}
			if ew != nil {
				return ew
			}
			if nw != nr {
				return io.ErrShortWrite
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				return nil
			}
			return er
		}
	}
}

func planUploadLocal(localPath string, remotePath string, recursive bool, resolver remotePathResolver) (transferPlan, error) {
	src, err := prepareUploadSource(localPath)
	if err != nil {
		return transferPlan{}, err
	}
	if src.stat.IsDir() {
		if !recursive {
			return transferPlan{}, fmt.Errorf("%w: directory transfer requires recursive=true", ErrValidation)
		}
		target, err := resolveUploadDirTarget(resolver, src, remotePath)
		if err != nil {
			return transferPlan{}, err
		}
		return buildUploadDirPlan(src.path, target)
	}
	if src.hadTrailing {
		return transferPlan{}, fmt.Errorf("%w: local source is a file; remove the trailing path separator", ErrValidation)
	}
	target, err := resolveUploadFileTarget(resolver, src.path, remotePath)
	if err != nil {
		return transferPlan{}, err
	}
	return transferPlan{
		files: []fileJob{{
			source: src.path,
			target: target,
			size:   src.stat.Size(),
			mode:   src.stat.Mode(),
		}},
		bytesTotal: src.stat.Size(),
		filesTotal: 1,
	}, nil
}

func buildUploadDirPlan(localDir string, remoteDir string) (transferPlan, error) {
	plan := transferPlan{}
	err := filepath.Walk(localDir, func(lp string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localDir, lp)
		if err != nil {
			return err
		}
		rp := path.Join(remoteDir, filepath.ToSlash(rel))
		if info.IsDir() {
			plan.dirs = append(plan.dirs, dirJob{target: rp, mode: info.Mode()})
			return nil
		}
		plan.files = append(plan.files, fileJob{source: lp, target: rp, size: info.Size(), mode: info.Mode()})
		plan.bytesTotal += info.Size()
		plan.filesTotal++
		return nil
	})
	return plan, err
}

func (s *Service) planDownloadLocal(res *backendView, remotePath string, localTarget string, recursive bool) (transferPlan, error) {
	clean := cleanRemote(remotePath)
	localSource, err := (localPathResolver{root: res.Root}).resolve(clean)
	if err != nil {
		return transferPlan{}, err
	}
	info, err := os.Stat(localSource)
	if err != nil {
		return transferPlan{}, err
	}
	localTarget, err = expandLocalHome(localTarget)
	if err != nil {
		return transferPlan{}, err
	}
	if info.IsDir() {
		if !recursive {
			return transferPlan{}, fmt.Errorf("%w: directory transfer requires recursive=true", ErrValidation)
		}
		rootTarget, copyContents, err := resolveLocalDownloadRoot(clean, localTarget, true)
		if err != nil {
			return transferPlan{}, err
		}
		return buildLocalDownloadDirPlan(localSource, clean, rootTarget, copyContents)
	}
	finalTarget, err := resolveDownloadFileTarget(clean, localTarget)
	if err != nil {
		return transferPlan{}, err
	}
	return transferPlan{
		files: []fileJob{{
			source: localSource,
			target: finalTarget,
			size:   info.Size(),
			mode:   info.Mode(),
		}},
		bytesTotal: info.Size(),
		filesTotal: 1,
	}, nil
}

func (s *Service) planDownloadRemote(res *backendView, remotePath string, localTarget string, recursive bool) (transferPlan, error) {
	localTarget, err := expandLocalHome(localTarget)
	if err != nil {
		return transferPlan{}, err
	}
	statPath := cleanRemote(remotePath)
	if strings.HasSuffix(remotePath, "/.") {
		statPath = strings.TrimSuffix(statPath, "/.")
	}
	info, err := res.client.Stat(statPath)
	if err != nil {
		return transferPlan{}, err
	}
	if info.IsDir() {
		if !recursive {
			return transferPlan{}, fmt.Errorf("%w: directory transfer requires recursive=true", ErrValidation)
		}
		rootTarget, copyContents, err := resolveLocalDownloadRoot(remotePath, localTarget, true)
		if err != nil {
			return transferPlan{}, err
		}
		return buildRemoteDownloadDirPlan(res.client, statPath, rootTarget, copyContents)
	}
	finalTarget, err := resolveDownloadFileTarget(statPath, localTarget)
	if err != nil {
		return transferPlan{}, err
	}
	return transferPlan{
		files: []fileJob{{
			source: statPath,
			target: finalTarget,
			size:   info.Size(),
			mode:   info.Mode(),
		}},
		bytesTotal: info.Size(),
		filesTotal: 1,
	}, nil
}

func buildLocalDownloadDirPlan(localRoot string, remoteRoot string, targetRoot string, copyContents bool) (transferPlan, error) {
	plan := transferPlan{}
	err := filepath.Walk(localRoot, func(lp string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localRoot, lp)
		if err != nil {
			return err
		}
		target := targetRoot
		if rel != "." {
			target = filepath.Join(targetRoot, filepath.FromSlash(filepath.ToSlash(rel)))
		}
		if copyContents && rel == "." {
			return nil
		}
		if info.IsDir() {
			plan.dirs = append(plan.dirs, dirJob{target: target, mode: info.Mode()})
			return nil
		}
		plan.files = append(plan.files, fileJob{source: lp, target: target, size: info.Size(), mode: info.Mode()})
		plan.bytesTotal += info.Size()
		plan.filesTotal++
		return nil
	})
	return plan, err
}

func buildRemoteDownloadDirPlan(client *pkgsftp.Client, remoteRoot string, targetRoot string, copyContents bool) (transferPlan, error) {
	plan := transferPlan{}
	walker := client.Walk(remoteRoot)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return transferPlan{}, err
		}
		rp := walker.Path()
		rel, err := filepath.Rel(remoteRoot, rp)
		if err != nil {
			return transferPlan{}, err
		}
		target := targetRoot
		if rel != "." {
			target = filepath.Join(targetRoot, filepath.FromSlash(rel))
		}
		if copyContents && rel == "." {
			continue
		}
		stat := walker.Stat()
		if stat.IsDir() {
			plan.dirs = append(plan.dirs, dirJob{target: target, mode: stat.Mode()})
			continue
		}
		plan.files = append(plan.files, fileJob{source: rp, target: target, size: stat.Size(), mode: stat.Mode()})
		plan.bytesTotal += stat.Size()
		plan.filesTotal++
	}
	return plan, nil
}

func resolveLocalDownloadRoot(remotePath string, localTarget string, recursive bool) (string, bool, error) {
	if !recursive {
		return "", false, fmt.Errorf("%w: directory transfer requires recursive=true", ErrValidation)
	}
	copyContents := strings.HasSuffix(remotePath, "/.")
	remoteRoot := strings.TrimSuffix(cleanRemote(remotePath), "/.")
	if copyContents {
		if localTarget == "" {
			localTarget = "."
		}
		return localTarget, true, nil
	}
	target, err := resolveDownloadDirTarget(remoteRoot, localTarget)
	return target, false, err
}

func (s *Service) requireOpenSession(id string, remotePath string) (*backendView, string, error) {
	s.mu.RLock()
	res, ok := s.sessions[id]
	if !ok {
		s.mu.RUnlock()
		return nil, "", ErrNotFound
	}
	if res.State != "open" {
		s.mu.RUnlock()
		return nil, "", fmt.Errorf("%w: sftp session is not open", ErrConflict)
	}
	clean := cleanRemote(remotePath)
	view := res.backendLocked()
	s.mu.RUnlock()
	return view, clean, nil
}

func (s *Service) resolve(id string, remotePath string) (string, string, error) {
	s.mu.RLock()
	res, ok := s.sessions[id]
	if !ok {
		s.mu.RUnlock()
		return "", "", ErrNotFound
	}
	if res.State != "open" {
		s.mu.RUnlock()
		return "", "", fmt.Errorf("%w: sftp session is not open", ErrConflict)
	}
	root := res.Root
	s.mu.RUnlock()
	clean := cleanRemote(remotePath)
	local := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if err := ensureInsideRoot(root, local); err != nil {
		return "", "", err
	}
	return local, clean, nil
}

func (s *Service) dependencies() (configProvider, sessionProvider, *sshpool.Pool, bool) {
	s.mu.RLock()
	if s.pool != nil {
		defer s.mu.RUnlock()
		return s.config, s.session, s.pool, s.testMode
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pool == nil {
		s.pool = sshpool.NewPool()
	}
	return s.config, s.session, s.pool, s.testMode
}

func (s *Service) followSessionCWD(id string, ch <-chan session.CWDNotify) {
	for notify := range ch {
		now := notify.Time
		if now.IsZero() {
			now = time.Now().UTC()
		}
		s.mu.Lock()
		res, ok := s.sessions[id]
		if !ok {
			s.mu.Unlock()
			return
		}
		if notify.Closed {
			res.followCancel = nil
			s.mu.Unlock()
			return
		}
		if notify.Path == "" || res.State == "closed" || res.State == "failed" || res.State == "disconnected" {
			s.mu.Unlock()
			continue
		}
		if res.CurrentDir == notify.Path {
			s.mu.Unlock()
			continue
		}
		res.CurrentDir = notify.Path
		res.UpdatedAt = now
		s.publishSessionLocked(res, Event{Type: "sftp.cwd.follow", SessionID: id, State: res.State, Path: notify.Path, Time: now})
		s.mu.Unlock()
	}
}

func (s *Service) publishSessionLocked(res *resource, event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	for ch := range s.subs[res.ID] {
		select {
		case ch <- cloneEvent(event):
		default:
		}
	}
	if s.onEvent != nil {
		s.onEvent(cloneEvent(event))
	}
}

func (s *Service) publishTransferLocked(transfer *transferState, typ string, now time.Time) {
	event := TransferEvent{
		Type:        typ,
		SessionID:   transfer.SessionID,
		TransferID:  transfer.ID,
		Direction:   transfer.Direction,
		State:       transfer.State,
		BytesTotal:  transfer.BytesTotal,
		BytesCopied: transfer.BytesCopied,
		FilesTotal:  transfer.FilesTotal,
		FilesDone:   transfer.FilesDone,
		CurrentPath: transfer.CurrentPath,
		Error:       transfer.Error,
		Time:        now,
	}
	for ch := range s.transferSubs[transfer.SessionID] {
		select {
		case ch <- event:
		default:
		}
	}
	if s.onEvent != nil {
		s.onEvent(Event{
			Type:        typ,
			SessionID:   transfer.SessionID,
			State:       transfer.State,
			TransferID:  transfer.ID,
			Direction:   transfer.Direction,
			BytesTotal:  transfer.BytesTotal,
			BytesCopied: transfer.BytesCopied,
			FilesTotal:  transfer.FilesTotal,
			FilesDone:   transfer.FilesDone,
			CurrentPath: transfer.CurrentPath,
			Error:       transfer.Error,
			Time:        now,
		})
	}
}

func (r *resource) snapshot() Session {
	out := r.Session
	if r.hostKeyChallenge != nil {
		out.HostKeyPending = r.hostKeyChallenge.Challenge.Pending
	}
	if r.authChallenge != nil {
		out.AuthPending = r.authChallenge.Challenge.Pending
	}
	return out
}

func (t *transferState) snapshot() Transfer {
	out := t.Transfer
	out.Items = cloneTransferItems(out.Items)
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

func prepareUploadSource(localPath string) (uploadSource, error) {
	displayPath := localPath
	localPath, err := expandLocalHome(localPath)
	if err != nil {
		return uploadSource{}, fmt.Errorf("failed to resolve local path: %w", err)
	}
	copyContents := localPathHasDotSuffix(localPath)
	statPath := localPath
	if copyContents {
		statPath = trimLocalDotSuffix(localPath)
	} else {
		statPath = trimTrailingLocalSeparators(localPath)
	}
	stat, err := os.Stat(statPath)
	if err != nil {
		return uploadSource{}, fmt.Errorf("failed to stat local path %q: %w", displayPath, err)
	}
	return uploadSource{
		path:         statPath,
		displayPath:  displayPath,
		stat:         stat,
		copyContents: copyContents,
		hadTrailing:  localPathHasTrailingSeparator(localPath),
	}, nil
}

func resolveUploadFileTarget(client remotePathResolver, localPath, remotePath string) (string, error) {
	originalTarget := cleanRemoteTarget(remotePath)
	targetIsDir := remoteTargetHasTrailingSlash(originalTarget)
	remotePath = cleanRemotePathForStat(originalTarget)
	targetType, err := remoteEntryKind(client, remotePath)
	if err != nil {
		return "", err
	}

	if targetIsDir {
		switch targetType {
		case remoteEntryFile:
			return "", fmt.Errorf("%w: remote target %q ends with '/' but exists as a file", ErrConflict, originalTarget)
		case remoteEntryMissing:
			if err := client.MkdirAll(remotePath); err != nil {
				return "", fmt.Errorf("failed to create remote directory %q: %w", remotePath, err)
			}
		}
		return path.Join(remotePath, localBase(localPath)), nil
	}
	if targetType == remoteEntryDir {
		return path.Join(remotePath, localBase(localPath)), nil
	}
	return remotePath, nil
}

func resolveUploadDirTarget(client remotePathResolver, src uploadSource, remotePath string) (string, error) {
	originalTarget := cleanRemoteTarget(remotePath)
	targetIsDir := remoteTargetHasTrailingSlash(originalTarget)
	remotePath = cleanRemotePathForStat(originalTarget)
	targetType, err := remoteEntryKind(client, remotePath)
	if err != nil {
		return "", err
	}
	if targetType == remoteEntryFile {
		if targetIsDir {
			return "", fmt.Errorf("%w: remote target %q ends with '/' but exists as a file", ErrConflict, originalTarget)
		}
		return "", fmt.Errorf("%w: remote target %q exists as a file", ErrConflict, remotePath)
	}

	target := remotePath
	switch {
	case src.copyContents:
		target = remotePath
	case targetIsDir:
		target = path.Join(remotePath, localBase(src.path))
	case targetType == remoteEntryDir:
		target = path.Join(remotePath, localBase(src.path))
	default:
		target = remotePath
	}
	if err := client.MkdirAll(target); err != nil {
		return "", fmt.Errorf("failed to create remote directory %q: %w", target, err)
	}
	return target, nil
}

func remoteEntryKind(client interface {
	Stat(string) (os.FileInfo, error)
}, remotePath string) (remoteEntryType, error) {
	stat, err := client.Stat(remotePath)
	if err == nil {
		if stat.IsDir() {
			return remoteEntryDir, nil
		}
		return remoteEntryFile, nil
	}
	if os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "not exist") || strings.Contains(strings.ToLower(err.Error()), "no such file") {
		return remoteEntryMissing, nil
	}
	return remoteEntryMissing, fmt.Errorf("failed to stat remote path %q: %w", remotePath, err)
}

func cleanRemoteTarget(remotePath string) string {
	if remotePath == "" {
		return "."
	}
	return remotePath
}

func remoteTargetHasTrailingSlash(remotePath string) bool {
	return strings.HasSuffix(remotePath, "/") && remotePath != "/"
}

func cleanRemotePathForStat(remotePath string) string {
	if remotePath == "" {
		return "."
	}
	if remotePath == "/" {
		return "/"
	}
	return strings.TrimRight(remotePath, "/")
}

func resolveDownloadFileTarget(remotePath, localPath string) (string, error) {
	if localPath == "" {
		localPath = path.Base(remotePath)
	}
	if localPathHasTrailingSeparator(localPath) {
		localPath = trimTrailingLocalSeparators(localPath)
		if localPath == "" {
			localPath = "."
		}
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			return "", fmt.Errorf("failed to create local directory %q: %w", localPath, err)
		}
		return filepath.Join(localPath, path.Base(remotePath)), nil
	}
	if stat, err := os.Stat(localPath); err == nil && stat.IsDir() {
		return filepath.Join(localPath, path.Base(remotePath)), nil
	}
	return localPath, nil
}

func resolveDownloadDirTarget(remoteDir, localDir string) (string, error) {
	if localDir == "" {
		localDir = "."
	}
	if localPathHasTrailingSeparator(localDir) {
		localDir = trimTrailingLocalSeparators(localDir)
		if localDir == "" {
			localDir = "."
		}
		return filepath.Join(localDir, path.Base(remoteDir)), nil
	}
	if stat, err := os.Stat(localDir); err == nil && stat.IsDir() {
		return filepath.Join(localDir, path.Base(remoteDir)), nil
	}
	return localDir, nil
}

func cleanRemote(remotePath string) string {
	if remotePath == "" {
		return "/"
	}
	clean := path.Clean("/" + remotePath)
	if clean == "." {
		return "/"
	}
	return clean
}

func ensureInsideRoot(root string, local string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realLocal, err := filepath.EvalSymlinks(local)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		realParent, parentErr := evalExistingParent(filepath.Dir(local), realRoot)
		if parentErr != nil {
			return parentErr
		}
		realLocal = filepath.Join(realParent, filepath.Base(local))
	}
	rel, err := filepath.Rel(realRoot, realLocal)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: remote path escapes session root", ErrValidation)
	}
	return nil
}

func evalExistingParent(dir string, root string) (string, error) {
	for {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			rel, relErr := filepath.Rel(root, real)
			if relErr != nil {
				return "", relErr
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
				return "", fmt.Errorf("%w: remote path escapes session root", ErrValidation)
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(dir)
		if next == dir {
			return "", err
		}
		dir = next
	}
}

func entryFromInfo(remotePath string, info os.FileInfo) Entry {
	typ := "file"
	if info.IsDir() {
		typ = "directory"
	}
	return Entry{
		Name:    path.Base(remotePath),
		Path:    remotePath,
		Type:    typ,
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime().UTC(),
	}
}

func sortEntries(entries []Entry, sortBy string) {
	desc := strings.HasPrefix(sortBy, "-")
	sortBy = strings.TrimPrefix(sortBy, "-")
	if sortBy == "" {
		sortBy = "name"
	}
	sort.Slice(entries, func(i, j int) bool {
		less := false
		switch sortBy {
		case "size":
			less = entries[i].Size < entries[j].Size
		case "mod_time":
			less = entries[i].ModTime.Before(entries[j].ModTime)
		case "type":
			if entries[i].Type == entries[j].Type {
				less = entries[i].Name < entries[j].Name
			} else {
				less = entries[i].Type < entries[j].Type
			}
		default:
			if entries[i].Type == entries[j].Type {
				less = entries[i].Name < entries[j].Name
			} else {
				less = entries[i].Type == "directory"
			}
		}
		if desc {
			return !less
		}
		return less
	})
}

func paginateEntries(entries []Entry, offset int, limit int) []Entry {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(entries) {
		return []Entry{}
	}
	end := len(entries)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return append([]Entry(nil), entries[offset:end]...)
}

func (s *Service) pruneTransfersLocked() {
	if len(s.transfers) < maxTransfers {
		return
	}
	type item struct {
		id string
		t  time.Time
	}
	items := make([]item, 0, len(s.transfers))
	for id, transfer := range s.transfers {
		if transfer.State == "running" || transfer.State == "queued" {
			continue
		}
		items = append(items, item{id: id, t: transfer.CompletedAt})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].t.Before(items[j].t)
	})
	limit := len(items) - maxTransfers/2
	for i := 0; i <= limit && i < len(items); i++ {
		delete(s.transfers, items[i].id)
	}
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
	out.Challenge = cloneChallengePtr(out.Challenge)
	return out
}

func cloneTransferItems(items []TransferItem) []TransferItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]TransferItem, len(items))
	copy(out, items)
	return out
}

func hostKeyRisk(changed bool) string {
	if changed {
		return "host_key_changed"
	}
	return "unknown_host"
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
