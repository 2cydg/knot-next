package sftp

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"
)

const (
	testAuthUser     = "testuser"
	testAuthPassword = "testpass"
)

// recordingConfigProvider records every credential save with the server identity
// it was addressed to, and can be programmed to fail with an error that echoes
// the credential back.
type recordingConfigProvider struct {
	mu       sync.Mutex
	cfg      config.RuntimeConfig
	saves    []savedSFTPCredential
	saveFail string
	saved    chan struct{}
}

type savedSFTPCredential struct {
	ServerID string
	Password string
	KeyID    string
}

func newRecordingConfigProvider(cfg config.RuntimeConfig) *recordingConfigProvider {
	return &recordingConfigProvider{cfg: cfg, saved: make(chan struct{}, 8)}
}

func (p *recordingConfigProvider) RuntimeConfig() (config.RuntimeConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneRuntimeConfig(p.cfg), nil
}

func (p *recordingConfigProvider) failSavesWith(prefix string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveFail = prefix
}

func (p *recordingConfigProvider) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saveFail != "" {
		return config.ServerProfileView{}, errors.New(p.saveFail + password)
	}
	p.saves = append(p.saves, savedSFTPCredential{ServerID: id, Password: password})
	server := p.cfg.Servers[id]
	server.Password = password
	server.AuthMethod = config.AuthMethodPassword
	p.cfg.Servers[id] = server
	select {
	case p.saved <- struct{}{}:
	default:
	}
	return config.ServerProfileView{ID: id, PasswordSet: password != ""}, nil
}

func (p *recordingConfigProvider) UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saveFail != "" {
		return config.ServerProfileView{}, errors.New(p.saveFail + server.KeyID)
	}
	p.saves = append(p.saves, savedSFTPCredential{ServerID: id, KeyID: server.KeyID})
	p.cfg.Servers[id] = server
	select {
	case p.saved <- struct{}{}:
	default:
	}
	return config.ServerProfileView{ID: id, KeyID: server.KeyID, AuthMethod: server.AuthMethod}, nil
}

func (p *recordingConfigProvider) savedCredentials() []savedSFTPCredential {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]savedSFTPCredential(nil), p.saves...)
}

func (p *recordingConfigProvider) waitForSave(t *testing.T) {
	t.Helper()
	select {
	case <-p.saved:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a credential save")
	}
}

// startSFTPServer starts a controlled SSH server with a real SFTP subsystem.
func startSFTPServer(t *testing.T) *sshserver.Server {
	t.Helper()
	return sshserver.New(t, sshserver.Config{
		User:     testAuthUser,
		Password: testAuthPassword,
		SFTPRoot: t.TempDir(),
	})
}

// newSFTPAuthService wires an SFTP service to a controlled server whose profile
// starts with an intentionally wrong password, so the flow has to go through the
// real challenge/auth chain.
func newSFTPAuthService(t *testing.T, srv *sshserver.Server, initialPassword string) (*Service, string, *recordingConfigProvider) {
	t.Helper()

	serverID := "loopback"
	provider := newRecordingConfigProvider(config.RuntimeConfig{
		Settings: config.Settings{KeepaliveInterval: "-1s"},
		Servers: map[string]config.ServerProfile{
			serverID: {
				ID:         serverID,
				Alias:      "loopback",
				Host:       srv.Host(),
				Port:       srv.Port(),
				User:       testAuthUser,
				AuthMethod: config.AuthMethodPassword,
				Password:   initialPassword,
			},
		},
	})

	pool := sshpool.NewPool()
	t.Cleanup(func() { pool.CloseAll() })

	service := NewService(t.TempDir())
	service.UseConfig(provider)
	service.UsePool(pool)
	return service, serverID, provider
}

// waitForSFTPState polls until the session reaches one of the wanted states.
func waitForSFTPState(t *testing.T, service *Service, id string, timeout time.Duration, states ...string) Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Session
	for time.Now().Before(deadline) {
		current, err := service.Get(id)
		if err != nil {
			t.Fatalf("get sftp session %s: %v", id, err)
		}
		last = current
		for _, state := range states {
			if current.State == state {
				return current
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("sftp session %s did not reach %v within %s (last state %q, cause %q)",
		id, states, timeout, last.State, last.DisconnectCause)
	return last
}

// respondSFTPAuthChallenge answers the pending auth retry and waits for the
// client to leave the pending state, so a later wait can only observe a new
// registration.
func respondSFTPAuthChallenge(t *testing.T, service *Service, id string, resp ChallengeResponse) {
	t.Helper()
	if _, err := service.RespondAuthChallenge(id, resp); err != nil {
		t.Fatalf("respond sftp auth challenge: %v", err)
	}
	waitForSFTPState(t, service, id, 5*time.Second, "connecting")
}

// R05: building the next attempt's config is a pure decision. The subsystem does
// not exist yet, so the candidate must not reach the configuration writer even
// though the client asked to remember it.
func TestReviewSFTPCandidateDoesNotPersist(t *testing.T) {
	provider := newRecordingConfigProvider(config.RuntimeConfig{
		Servers: map[string]config.ServerProfile{
			"manual": {
				ID:         "manual",
				Alias:      "manual",
				Host:       "127.0.0.1",
				Port:       22,
				User:       "remote-user",
				AuthMethod: config.AuthMethodPassword,
				Password:   "original-password",
			},
		},
	})
	service := NewService(t.TempDir())
	service.UseConfig(provider)

	cfg, err := provider.RuntimeConfig()
	if err != nil {
		t.Fatalf("runtime config: %v", err)
	}
	server := cfg.Servers["manual"]

	next, err := service.runtimeConfigForAuthResponse(cfg, server, ChallengeResponse{
		Password: "candidate-password",
		Remember: true,
	})
	if err != nil {
		t.Fatalf("runtimeConfigForAuthResponse: %v", err)
	}
	if next.Servers["manual"].Password != "candidate-password" {
		t.Fatalf("attempt password = %q, want the candidate used for this attempt", next.Servers["manual"].Password)
	}

	if saved := provider.savedCredentials(); len(saved) != 0 {
		t.Fatalf("saved credentials = %+v, want none before the subsystem opens", saved)
	}
	persisted, err := provider.RuntimeConfig()
	if err != nil {
		t.Fatalf("runtime config: %v", err)
	}
	if persisted.Servers["manual"].Password != "original-password" {
		t.Fatalf("persisted password = %q, want the original untouched", persisted.Servers["manual"].Password)
	}
}

// R05/C10: against a real SSH + SFTP chain, the rejected candidate is replaced by
// the final one and only the successful value is written, once.
func TestReviewSFTPOnlyFinalSuccessfulCandidateIsSaved(t *testing.T) {
	srv := startSFTPServer(t)
	service, serverID, provider := newSFTPAuthService(t, srv, "wrong-sftp-password")

	created, err := service.Create(CreateRequest{
		ServerRef:      serverID,
		Alias:          "client-chosen-alias",
		AllowAuthRetry: true,
		HostKeyPolicy:  sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondSFTPAuthChallenge(t, service, created.ID, ChallengeResponse{Password: "wrong-sftp-password", Remember: true})
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondSFTPAuthChallenge(t, service, created.ID, ChallengeResponse{Password: testAuthPassword, Remember: true})
	waitForSFTPState(t, service, created.ID, 5*time.Second, "open")

	provider.waitForSave(t)

	saved := provider.savedCredentials()
	if len(saved) != 1 {
		t.Fatalf("saved credentials = %+v, want exactly one", saved)
	}
	if saved[0].Password != testAuthPassword {
		t.Fatalf("saved password = %q, want the password that actually worked", saved[0].Password)
	}
	if saved[0].ServerID != serverID {
		t.Fatalf("saved to %q, want the resolved server ID %q (Alias is display-only)", saved[0].ServerID, serverID)
	}
}

// R05: a final attempt that is not to be remembered must not be written, and it
// must clear the candidate left by the rejected attempt before it.
func TestReviewSFTPNotRememberedSuccessSavesNothing(t *testing.T) {
	srv := startSFTPServer(t)
	service, serverID, provider := newSFTPAuthService(t, srv, "wrong-sftp-password-2")

	created, err := service.Create(CreateRequest{
		ServerRef:      serverID,
		AllowAuthRetry: true,
		HostKeyPolicy:  sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondSFTPAuthChallenge(t, service, created.ID, ChallengeResponse{Password: "wrong-sftp-password-2", Remember: true})
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondSFTPAuthChallenge(t, service, created.ID, ChallengeResponse{Password: testAuthPassword, Remember: false})
	waitForSFTPState(t, service, created.ID, 5*time.Second, "open")

	if saved := provider.savedCredentials(); len(saved) != 0 {
		t.Fatalf("saved credentials = %+v, want none: the rejected candidate survived", saved)
	}
}

// R08: after the client cancels a session that is waiting on a challenge, a late
// failure must not rewrite its terminal state, time or cause.
func TestReviewSFTPFailureCannotOverwriteCancellation(t *testing.T) {
	srv := startSFTPServer(t)
	service, serverID, _ := newSFTPAuthService(t, srv, "wrong-sftp-password-3")

	created, err := service.Create(CreateRequest{
		ServerRef:      serverID,
		AllowAuthRetry: true,
		HostKeyPolicy:  sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	cancelled, err := service.Close(created.ID)
	if err != nil {
		t.Fatalf("close sftp session: %v", err)
	}
	if cancelled.State != "closed" {
		t.Fatalf("state after close = %q, want closed", cancelled.State)
	}
	if cancelled.ClosedAt == nil {
		t.Fatal("cancelled sftp session has no close time")
	}
	cause := cancelled.DisconnectCause

	// A client may still try to answer the stale challenge; a cancelled session
	// must refuse it instead of returning to a pending state.
	if _, err := service.RespondAuthChallenge(created.ID, ChallengeResponse{Password: testAuthPassword}); err == nil {
		t.Fatal("a cancelled sftp session accepted a challenge response")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := service.Get(created.ID)
		if err != nil {
			t.Fatalf("get sftp session: %v", err)
		}
		if current.State != "closed" {
			t.Fatalf("state = %q, want the cancelled state to stand", current.State)
		}
		if current.DisconnectCause != cause {
			t.Fatalf("cause = %q, want %q preserved", current.DisconnectCause, cause)
		}
		if current.ClosedAt == nil || !current.ClosedAt.Equal(*cancelled.ClosedAt) {
			t.Fatal("the terminal timestamp was rewritten by a late failure")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Repeated closes stay idempotent and keep the first recorded outcome.
	again, err := service.Close(created.ID)
	if err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	if again.State != "closed" || again.DisconnectCause != cause {
		t.Fatalf("repeat close produced %q/%q, want closed/%q", again.State, again.DisconnectCause, cause)
	}
}

// §15.1/C05: a save failure keeps the working SFTP session usable and reports a
// warning that does not leak the credential.
func TestReviewSFTPSaveFailurePublishesSanitizedWarning(t *testing.T) {
	srv := startSFTPServer(t)
	service, serverID, provider := newSFTPAuthService(t, srv, "wrong-sftp-password-4")

	provider.failSavesWith("cannot write password ")

	events := make(chan Event, 64)
	service.OnEvent(func(event Event) {
		select {
		case events <- event:
		default:
		}
	})

	created, err := service.Create(CreateRequest{
		ServerRef:      serverID,
		AllowAuthRetry: true,
		HostKeyPolicy:  sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondSFTPAuthChallenge(t, service, created.ID, ChallengeResponse{Password: "wrong-sftp-password-4", Remember: false})
	waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondSFTPAuthChallenge(t, service, created.ID, ChallengeResponse{Password: testAuthPassword, Remember: true})
	waitForSFTPState(t, service, created.ID, 5*time.Second, "open")

	warning := waitForSFTPWarning(t, events, created.ID)
	if warning.Kind != session.WarningCredentialSaveFailed {
		t.Fatalf("warning kind = %q, want %q", warning.Kind, session.WarningCredentialSaveFailed)
	}
	if strings.Contains(warning.Message, testAuthPassword) {
		t.Fatalf("warning leaked the credential: %q", warning.Message)
	}
	if warning.Message != "password was not saved" {
		t.Fatalf("warning = %q, want fixed safe save message", warning.Message)
	}

	// The session still works: the failure is a warning, never a session failure.
	final, err := service.Get(created.ID)
	if err != nil {
		t.Fatalf("get sftp session: %v", err)
	}
	if final.State != "open" {
		t.Fatalf("state = %q, want the working SFTP session preserved", final.State)
	}
	if _, err := service.List(created.ID, "/", ListOptions{}); err != nil {
		t.Fatalf("list after failed save: %v", err)
	}
}

func waitForSFTPWarning(t *testing.T, events <-chan Event, sessionID string) session.Warning {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.SessionID == sessionID && event.Warning != nil {
				return *event.Warning
			}
		case <-deadline:
			t.Fatal("no warning was published for the failed sftp credential save")
			return session.Warning{}
		}
	}
}

func (p *recordingConfigProvider) RememberServerAuth(id string, choice config.AuthChoice) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saveFail != "" {
		return errors.New(p.saveFail + choice.Password + choice.KeyID)
	}
	profile := p.cfg.Servers[id]
	profile.AuthMethod, profile.KeyID, profile.Password = choice.Method, choice.KeyID, choice.Password
	p.cfg.Servers[id] = profile
	p.saves = append(p.saves, savedSFTPCredential{ServerID: id, Password: choice.Password, KeyID: choice.KeyID})
	select {
	case p.saved <- struct{}{}:
	default:
	}
	return nil
}
