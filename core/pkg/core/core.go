package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

const (
	APIVersion = "v1"
)

var (
	DefaultVersion = "0.1.0-dev"
	BuildCommit    = ""
	BuildTime      = ""
	BuildDirty     = ""
)

type Service struct {
	version      string
	startedAt    time.Time
	statusCache  statusCache
	capabilityMu sync.RWMutex
	config       *config.Service
	secret       *secret.Service
	session      *session.Service
	sftp         *sftp.Service
	sshPool      *sshpool.Pool
	events       *EventBus
	shutdown     func()
}

type statusCache struct {
	mu        sync.Mutex
	expiresAt time.Time
	alloc     uint64
}

func New(version string, startedAt time.Time) *Service {
	if version == "" {
		version = DefaultVersion
	}
	return &Service{version: version, startedAt: startedAt.UTC(), events: NewEventBus()}
}

func (s *Service) UseConfig(configService *config.Service) {
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.config = configService
}

func (s *Service) UseSecret(secretService *secret.Service) {
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.secret = secretService
}

func (s *Service) UseSession(sessionService *session.Service) {
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.session = sessionService
	if sessionService != nil {
		sessionService.OnEvent(s.handleSessionEvent)
	}
}

func (s *Service) UseSFTP(sftpService *sftp.Service) {
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.sftp = sftpService
	if sftpService != nil {
		sftpService.OnEvent(s.handleSFTPEvent)
	}
}

func (s *Service) UseSSHPool(pool *sshpool.Pool) {
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.sshPool = pool
	if pool != nil {
		pool.DisconnectCallback = s.handleSSHPoolDisconnect
	}
}

func (s *Service) UseShutdown(fn func()) {
	s.capabilityMu.Lock()
	defer s.capabilityMu.Unlock()
	s.shutdown = fn
}

func (s *Service) Config() *config.Service {
	s.capabilityMu.RLock()
	defer s.capabilityMu.RUnlock()
	return s.config
}

func (s *Service) Secret() *secret.Service {
	s.capabilityMu.RLock()
	defer s.capabilityMu.RUnlock()
	return s.secret
}

func (s *Service) Session() *session.Service {
	s.capabilityMu.RLock()
	defer s.capabilityMu.RUnlock()
	return s.session
}

func (s *Service) SFTP() *sftp.Service {
	s.capabilityMu.RLock()
	defer s.capabilityMu.RUnlock()
	return s.sftp
}

func (s *Service) SSHPool() *sshpool.Pool {
	s.capabilityMu.RLock()
	defer s.capabilityMu.RUnlock()
	return s.sshPool
}

type VersionInfo struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	GoVersion  string `json:"go_version"`
	Commit     string `json:"commit"`
	BuildTime  string `json:"build_time"`
	Dirty      string `json:"dirty"`
	Compiler   string `json:"compiler"`
}

func (s *Service) VersionInfo() VersionInfo {
	return VersionInfo{
		Version:    s.version,
		APIVersion: APIVersion,
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		GoVersion:  runtime.Version(),
		Commit:     BuildCommit,
		BuildTime:  BuildTime,
		Dirty:      BuildDirty,
		Compiler:   runtime.Compiler,
	}
}

func (s *Service) SubscribeEvents() (<-chan Event, func(), error) {
	return s.events.Subscribe()
}

func (s *Service) PublishEvent(event Event) {
	s.events.Publish(event)
}

func (s *Service) ClearConnections() int {
	s.capabilityMu.RLock()
	pool := s.sshPool
	s.capabilityMu.RUnlock()
	if pool == nil {
		return 0
	}
	closed := pool.Clear()
	s.PublishEvent(Event{
		Type:     "core.connections_cleared",
		Resource: "connections",
		Level:    "info",
		Data:     map[string]any{"closed": closed},
	})
	return closed
}

// RequestShutdown asks the process to begin its shutdown flow. The HTTP handler
// uses it because the runner owns the single teardown flow: every trigger, the
// API included, enters through the same entry point instead of releasing
// resources on its own path.
func (s *Service) RequestShutdown() {
	s.capabilityMu.RLock()
	shutdown := s.shutdown
	s.capabilityMu.RUnlock()
	if shutdown != nil {
		shutdown()
	}
}

// Shutdown releases the resources this service owns. The lifecycle runner calls
// it during teardown, so the API, SIGINT/SIGTERM, and serve-error paths all
// release sessions, SFTP resources, and pooled connections the same way.
func (s *Service) Shutdown(ctx context.Context) error {
	s.capabilityMu.RLock()
	sessionService := s.session
	sftpService := s.sftp
	pool := s.sshPool
	s.capabilityMu.RUnlock()

	s.PublishEvent(Event{Type: "core.shutdown_started", Resource: "core", Level: "info"})

	// The three resource classes are released concurrently. Running them in
	// sequence would let one stalled connection hold the others back and make the
	// budget cover the sum of the steps rather than the whole teardown; each class
	// also stops as soon as the budget is gone instead of walking the rest of its
	// list, so an expired budget cannot be multiplied by the number of resources.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		releases []error
	)
	// These classes share one budget. A deadline error can also be returned by
	// an already drained class; it is not evidence that this class still owns work.
	release := func(name string, fn func(ctx context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				mu.Lock()
				releases = append(releases, fmt.Errorf("%s release skipped: %w", name, err))
				mu.Unlock()
				return
			}
			err := fn(ctx)
			if err != nil {
				mu.Lock()
				releases = append(releases, fmt.Errorf("%s release: %w", name, err))
				mu.Unlock()
			}
			if err := ctx.Err(); err != nil {
				mu.Lock()
				releases = append(releases, fmt.Errorf("%s release did not finish: %w", name, err))
				mu.Unlock()
			}
		}()
	}

	if sessionService != nil {
		release("session", sessionService.Shutdown)
	}
	if sftpService != nil {
		release("sftp", sftpService.Shutdown)
	}
	if pool != nil {
		release("ssh pool", pool.Shutdown)
	}
	// The wait is bounded by the same budget. A release step that never returns —
	// a subsystem close parked on a remote that stopped answering — must not keep
	// Shutdown from returning; the step is left to finish on its own and its
	// expiry is reported below.
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		mu.Lock()
		releases = append(releases, fmt.Errorf("core resource release did not finish: %w", ctx.Err()))
		mu.Unlock()
	}

	// A release step may still be running — the wait above is bounded, not the
	// step — so the report is snapshotted under the lock rather than read while
	// a straggler could still be appending to it.
	mu.Lock()
	report := append([]error(nil), releases...)
	mu.Unlock()

	s.events.Close()

	// A cancelled budget means teardown ran out of time part-way or skipped a
	// class entirely; the caller must be able to observe that rather than read it
	// as a complete release.
	if err := errors.Join(report...); err != nil {
		return fmt.Errorf("core resource release: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("core resource release: %w", err)
	}
	return nil
}

func (s *Service) handleSessionEvent(event session.Event) {
	data := map[string]any{
		"state": event.State,
		"rows":  event.Rows,
		"cols":  event.Cols,
	}
	if event.Path != "" {
		data["path"] = event.Path
	}
	if event.ExitCode != nil {
		data["exit_code"] = *event.ExitCode
	}
	if event.Error != "" {
		data["error"] = event.Error
	}
	if event.Challenge != nil {
		data["challenge"] = event.Challenge
	}
	s.PublishEvent(Event{
		Type:       event.Type,
		Resource:   "session",
		ResourceID: event.SessionID,
		Level:      "info",
		Time:       event.Time,
		Data:       data,
	})
}

func (s *Service) handleSFTPEvent(event sftp.Event) {
	resource := "sftp"
	resourceID := event.SessionID
	if strings.HasPrefix(event.Type, "sftp.transfer.") {
		resource = "sftp_transfer"
		resourceID = event.TransferID
	}
	data := map[string]any{
		"session_id":   event.SessionID,
		"state":        event.State,
		"transfer_id":  event.TransferID,
		"direction":    event.Direction,
		"bytes_total":  event.BytesTotal,
		"bytes_copied": event.BytesCopied,
		"files_total":  event.FilesTotal,
		"files_done":   event.FilesDone,
		"current_path": event.CurrentPath,
		"error":        event.Error,
	}
	if event.Path != "" {
		data["path"] = event.Path
	}
	if event.Challenge != nil {
		data["challenge"] = event.Challenge
	}
	s.PublishEvent(Event{
		Type:       event.Type,
		Resource:   resource,
		ResourceID: resourceID,
		Level:      eventLevel(event.State, event.Error),
		Time:       event.Time,
		Data:       data,
	})
}

func (s *Service) handleSSHPoolDisconnect(poolKey string) {
	s.capabilityMu.RLock()
	sessionService := s.session
	sftpService := s.sftp
	s.capabilityMu.RUnlock()

	if sessionService != nil {
		sessionService.DisconnectByPoolKey(poolKey)
	}
	if sftpService != nil {
		sftpService.CloseByPoolKey(poolKey)
	}
	s.PublishEvent(Event{
		Type:       "ssh_pool.disconnected",
		Resource:   "ssh_pool",
		ResourceID: poolKey,
		Level:      "warning",
		Data:       map[string]any{"pool_key": poolKey},
	})
}

func eventLevel(state string, err string) string {
	if err != "" || state == "failed" || strings.Contains(state, "error") {
		return "error"
	}
	return "info"
}

type Health struct {
	Status        string        `json:"status"`
	Authenticated bool          `json:"authenticated"`
	StartedAt     time.Time     `json:"started_at"`
	Checks        []HealthCheck `json:"checks"`
}

type HealthCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type HealthInput struct {
	TokenAvailable  bool
	RuntimePath     string
	ListenAddresses []string
}

func (s *Service) Health(authenticated bool, input HealthInput) Health {
	s.capabilityMu.RLock()
	configService := s.config
	secretService := s.secret
	pool := s.sshPool
	s.capabilityMu.RUnlock()

	checks := []HealthCheck{
		checkToken(input.TokenAvailable),
		checkRuntimeFile(input.RuntimePath),
		checkListener(input.ListenAddresses),
	}
	checks = append(checks, checkConfig(configService))
	checks = append(checks, checkCrypto(secretService))
	checks = append(checks, checkSSHPool(pool))

	status := "ok"
	for _, check := range checks {
		if check.Status != "ok" {
			status = "degraded"
			break
		}
	}
	return Health{
		Status:        status,
		Authenticated: authenticated,
		StartedAt:     s.startedAt,
		Checks:        checks,
	}
}

func checkToken(available bool) HealthCheck {
	if !available {
		return HealthCheck{Name: "token", Status: "failed", Detail: "token verifier is not configured"}
	}
	return HealthCheck{Name: "token", Status: "ok"}
}

func checkRuntimeFile(path string) HealthCheck {
	if path == "" {
		return HealthCheck{Name: "runtime_file", Status: "failed", Detail: "runtime path is not configured"}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return HealthCheck{Name: "runtime_file", Status: "failed", Detail: err.Error()}
	}
	if err := file.Close(); err != nil {
		return HealthCheck{Name: "runtime_file", Status: "failed", Detail: err.Error()}
	}
	return HealthCheck{Name: "runtime_file", Status: "ok", Detail: path}
}

func checkListener(addresses []string) HealthCheck {
	if len(addresses) == 0 {
		return HealthCheck{Name: "listener", Status: "failed", Detail: "no listen addresses are registered"}
	}
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return HealthCheck{Name: "listener", Status: "failed", Detail: fmt.Sprintf("%s: %v", address, err)}
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return HealthCheck{Name: "listener", Status: "failed", Detail: fmt.Sprintf("%s is not loopback", address)}
		}
	}
	return HealthCheck{Name: "listener", Status: "ok"}
}

func checkConfig(configService *config.Service) HealthCheck {
	if configService == nil {
		return HealthCheck{Name: "config", Status: "failed", Detail: "config service is not configured"}
	}
	metadata, err := configService.Metadata()
	if err != nil {
		return HealthCheck{Name: "config", Status: "failed", Detail: err.Error()}
	}
	return HealthCheck{Name: "config", Status: "ok", Detail: metadata.ConfigPath}
}

func checkCrypto(secretService *secret.Service) HealthCheck {
	if secretService == nil {
		return HealthCheck{Name: "crypto", Status: "failed", Detail: "secret service is not configured"}
	}
	capability := secretService.CryptoCapability()
	if !capability.Available {
		return HealthCheck{Name: "crypto", Status: "failed", Detail: capability.Provider}
	}
	return HealthCheck{Name: "crypto", Status: "ok", Detail: capability.Provider}
}

func checkSSHPool(pool *sshpool.Pool) HealthCheck {
	if pool == nil {
		return HealthCheck{Name: "ssh_pool", Status: "failed", Detail: "ssh pool is not configured"}
	}
	if pool.IsClosed() {
		return HealthCheck{Name: "ssh_pool", Status: "failed", Detail: "ssh pool is closed"}
	}
	return HealthCheck{Name: "ssh_pool", Status: "ok"}
}

type Capability struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Risk   string `json:"risk"`
}

func (s *Service) Capabilities() []Capability {
	s.capabilityMu.RLock()
	configAvailable := s.config != nil
	secretAvailable := s.secret != nil
	sessionAvailable := s.session != nil
	sftpAvailable := s.sftp != nil
	s.capabilityMu.RUnlock()
	return []Capability{
		{Name: "core", Status: "available", Risk: "READ_ONLY"},
		{Name: "http_api", Status: "available", Risk: "READ_ONLY"},
		{Name: "token_auth", Status: "available", Risk: "READ_ONLY"},
		{Name: "runtime_discovery", Status: "available", Risk: "READ_ONLY"},
		{Name: "config", Status: capabilityStatus(configAvailable), Risk: "LOCAL_MUTATION"},
		{Name: "secret", Status: capabilityStatus(secretAvailable), Risk: "LOCAL_MUTATION"},
		{Name: "session", Status: capabilityStatus(sessionAvailable), Risk: "LONG_RUNNING"},
		{Name: "sftp", Status: capabilityStatus(sftpAvailable), Risk: "REMOTE_MUTATION"},
		{Name: "forward", Status: "planned", Risk: "LONG_RUNNING"},
		{Name: "broadcast", Status: "planned", Risk: "LONG_RUNNING"},
		{Name: "sync", Status: "planned", Risk: "LONG_RUNNING"},
		{Name: "archive", Status: "planned", Risk: "LOCAL_MUTATION"},
		{Name: "update", Status: "planned", Risk: "DESTRUCTIVE"},
		{Name: "task", Status: "planned", Risk: "LONG_RUNNING"},
	}
}

func capabilityStatus(available bool) string {
	if available {
		return "available"
	}
	return "planned"
}

type Status struct {
	UptimeSeconds       int64      `json:"uptime_seconds"`
	ActiveSessions      int        `json:"active_sessions"`
	ActiveSFTPSessions  int        `json:"active_sftp_sessions"`
	RunningTransfers    int        `json:"running_transfers"`
	ActiveForwards      int        `json:"active_forwards"`
	RunningTasks        int        `json:"running_tasks"`
	SSHPool             PoolStatus `json:"ssh_pool"`
	AllocatedMemoryByte uint64     `json:"allocated_memory_bytes"`
}

type PoolStatus struct {
	Count   int                 `json:"count"`
	Entries []sshpool.EntryStat `json:"entries"`
}

func (s *Service) Status() Status {
	alloc := s.cachedAllocatedMemory()
	s.capabilityMu.RLock()
	sessionService := s.session
	sftpService := s.sftp
	pool := s.sshPool
	s.capabilityMu.RUnlock()
	activeSessions := 0
	if sessionService != nil {
		activeSessions = sessionService.ActiveCount()
	}
	activeSFTPSessions := 0
	runningTransfers := 0
	if sftpService != nil {
		activeSFTPSessions = sftpService.SessionCount()
		runningTransfers = sftpService.RunningTransferCount()
	}
	poolStatus := PoolStatus{}
	if pool != nil {
		poolStatus.Count, poolStatus.Entries = pool.Snapshot()
	}
	return Status{
		UptimeSeconds:       int64(time.Since(s.startedAt).Seconds()),
		ActiveSessions:      activeSessions,
		ActiveSFTPSessions:  activeSFTPSessions,
		RunningTransfers:    runningTransfers,
		ActiveForwards:      0,
		RunningTasks:        0,
		SSHPool:             poolStatus,
		AllocatedMemoryByte: alloc,
	}
}

func (s *Service) cachedAllocatedMemory() uint64 {
	now := time.Now()
	s.statusCache.mu.Lock()
	defer s.statusCache.mu.Unlock()
	if now.Before(s.statusCache.expiresAt) {
		return s.statusCache.alloc
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s.statusCache.alloc = mem.Alloc
	s.statusCache.expiresAt = now.Add(time.Second)
	return s.statusCache.alloc
}
