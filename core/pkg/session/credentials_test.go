package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
)

// spyConfigWriter tracks calls to credential-saving methods. Credential saving
// runs on a background goroutine, so the recorded values are mutex-guarded and
// each save signals a channel that tests wait on instead of sleeping.
type spyConfigWriter struct {
	savePasswordCalls atomic.Int32
	updateServerCalls atomic.Int32

	mu             sync.Mutex
	savedPasswords []string
	saved          chan struct{}
}

func newSpyConfigWriter() *spyConfigWriter {
	return &spyConfigWriter{saved: make(chan struct{}, 8)}
}

// waitForSave blocks until at least one credential save happened.
func (s *spyConfigWriter) waitForSave(t *testing.T) {
	t.Helper()
	select {
	case <-s.saved:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for credential save")
	}
}

func (s *spyConfigWriter) passwords() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.savedPasswords...)
}

func (s *spyConfigWriter) RuntimeConfig() (config.RuntimeConfig, error) {
	return config.RuntimeConfig{
		Servers: map[string]config.ServerProfile{
			"test-server": {
				ID:         "test-server",
				Alias:      "test",
				Host:       "127.0.0.1",
				Port:       22,
				User:       "testuser",
				AuthMethod: config.AuthMethodPassword,
			},
		},
	}, nil
}

func (s *spyConfigWriter) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	s.savePasswordCalls.Add(1)
	s.mu.Lock()
	s.savedPasswords = append(s.savedPasswords, password)
	s.mu.Unlock()
	select {
	case s.saved <- struct{}{}:
	default:
	}
	return config.ServerProfileView{}, nil
}

func (s *spyConfigWriter) UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error) {
	s.updateServerCalls.Add(1)
	return config.ServerProfileView{}, nil
}

func TestCredentialSaveOnlyAfterSuccess(t *testing.T) {
	spy := newSpyConfigWriter()
	service := NewService()
	service.UseConfig(spy)
	service.UseLocalTestBackend() // Use test mode to avoid real SSH

	// Create session with Remember=true
	req := CreateRequest{
		ServerRef: "test-server",
		Term:      "xterm",
		Rows:      24,
		Cols:      80,
	}

	res, err := service.Create(req)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// In test mode the session is connected synchronously and no challenge ever
	// happened, so there is nothing to remember and nothing may be saved.
	if spy.savePasswordCalls.Load() != 0 {
		t.Errorf("SetServerPassword should not be called in test mode without challenge, got %d calls",
			spy.savePasswordCalls.Load())
	}

	// Verify session is connected
	session, err := service.Get(res.ID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}

	if session.State != "connected" {
		t.Errorf("expected state 'connected', got %q", session.State)
	}
}

func TestCredentialNotSavedOnAuthFailure(t *testing.T) {
	spy := newSpyConfigWriter()
	service := NewService()
	service.UseConfig(spy)

	// Test that credentials are NOT saved when using runtimeConfigForAuthResponse
	// without subsequent connection success
	cfg, err := spy.RuntimeConfig()
	if err != nil {
		t.Fatalf("failed to get runtime config: %v", err)
	}

	server := cfg.Servers["test-server"]
	resp := ChallengeResponse{
		Password: "wrong-password",
		Remember: true,
	}

	// This should only construct config, not save
	_, err = service.runtimeConfigForAuthResponse(cfg, server, resp)
	if err != nil {
		t.Fatalf("runtimeConfigForAuthResponse failed: %v", err)
	}

	// runtimeConfigForAuthResponse only builds a candidate config; it must not
	// persist anything. It is a pure function, so the assertion needs no wait.
	if spy.savePasswordCalls.Load() != 0 {
		t.Errorf("SetServerPassword should not be called before auth success, got %d calls",
			spy.savePasswordCalls.Load())
	}
}

func TestPendingCredentialsSavedOnSuccess(t *testing.T) {
	spy := newSpyConfigWriter()
	service := NewService()
	service.UseConfig(spy)
	pool := sshpool.NewPool()
	service.UsePool(pool)

	// Create a session
	now := time.Now().UTC()
	session := &resource{
		Resource: Resource{
			ID:        "test-1",
			ServerRef: "test-server",
			State:     "connecting",
			StartedAt: now,
			UpdatedAt: now,
		},
		serverID:       "test-server",
		subscribers:    map[chan Event]struct{}{},
		cwdSubscribers: map[chan CWDNotify]struct{}{},
		cancel:         func() {},
	}

	// Set pending credentials with Remember=true
	session.pendingCredentials = &ChallengeResponse{
		Password: "test-password",
		Remember: true,
	}

	service.mu.Lock()
	service.sessions["test-1"] = session
	service.mu.Unlock()

	// Simulate successful authentication by calling attachBackend
	service.attachBackend("test-1", nil, nil)

	// The save is asynchronous, so wait for its signal rather than sleeping.
	spy.waitForSave(t)

	if calls := spy.savePasswordCalls.Load(); calls != 1 {
		t.Errorf("expected SetServerPassword to be called once after success, got %d calls", calls)
	}

	if saved := spy.passwords(); len(saved) != 1 || saved[0] != "test-password" {
		t.Errorf("expected password 'test-password' to be saved, got %v", saved)
	}

	// Verify pending credentials are cleared
	service.mu.RLock()
	if session.pendingCredentials != nil {
		t.Error("pending credentials should be cleared after save")
	}
	service.mu.RUnlock()
}

func TestCredentialNotSavedIfRememberFalse(t *testing.T) {
	spy := newSpyConfigWriter()
	service := NewService()
	service.UseConfig(spy)

	now := time.Now().UTC()
	session := &resource{
		Resource: Resource{
			ID:        "test-2",
			ServerRef: "test-server",
			State:     "connecting",
			StartedAt: now,
			UpdatedAt: now,
		},
		serverID:       "test-server",
		subscribers:    map[chan Event]struct{}{},
		cwdSubscribers: map[chan CWDNotify]struct{}{},
		cancel:         func() {},
	}

	// Set pending credentials with Remember=false
	session.pendingCredentials = &ChallengeResponse{
		Password: "test-password",
		Remember: false,
	}

	service.mu.Lock()
	service.sessions["test-2"] = session
	service.mu.Unlock()

	// Call attachBackend
	service.attachBackend("test-2", nil, nil)

	// Remember=false takes no asynchronous path at all, and R06 requires the
	// candidate to be dropped rather than retained: a stale candidate that
	// outlives its attempt is exactly what gets saved by a later success.
	service.mu.RLock()
	pending := session.pendingCredentials
	service.mu.RUnlock()

	if pending != nil {
		t.Errorf("pending credentials must be dropped when Remember is false, got %+v", pending)
	}
	if calls := spy.savePasswordCalls.Load(); calls != 0 {
		t.Errorf("SetServerPassword should not be called when Remember=false, got %d calls", calls)
	}
}

func (s *spyConfigWriter) RememberServerAuth(id string, choice config.AuthChoice) error {
	if choice.Method == config.AuthMethodPassword {
		s.savePasswordCalls.Add(1)
		s.mu.Lock()
		s.savedPasswords = append(s.savedPasswords, choice.Password)
		s.mu.Unlock()
		select {
		case s.saved <- struct{}{}:
		default:
		}
	} else {
		s.updateServerCalls.Add(1)
	}
	return nil
}
