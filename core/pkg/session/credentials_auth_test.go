package session

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
)

// recordingConfigService records every credential save together with the server
// identity it was addressed to, and can be programmed to fail so the warning
// path is exercised with a real error.
type recordingConfigService struct {
	mu       sync.Mutex
	runtime  config.RuntimeConfig
	saves    []savedCredential
	saveFail string
	saved    chan struct{}
}

type savedCredential struct {
	ServerID string
	Password string
	KeyID    string
}

func newRecordingConfigService(runtime config.RuntimeConfig) *recordingConfigService {
	return &recordingConfigService{runtime: runtime, saved: make(chan struct{}, 8)}
}

func (s *recordingConfigService) RuntimeConfig() (config.RuntimeConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRuntimeConfig(s.runtime), nil
}

// failSavesWith makes every save fail with an error that echoes the credential
// back, which is what a real configuration writer may do.
func (s *recordingConfigService) failSavesWith(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveFail = prefix
}

func (s *recordingConfigService) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveFail != "" {
		return config.ServerProfileView{}, errors.New(s.saveFail + password)
	}
	s.saves = append(s.saves, savedCredential{ServerID: id, Password: password})
	server := s.runtime.Servers[id]
	server.Password = password
	server.AuthMethod = config.AuthMethodPassword
	s.runtime.Servers[id] = server
	select {
	case s.saved <- struct{}{}:
	default:
	}
	return config.ServerProfileView{ID: id, PasswordSet: password != ""}, nil
}

func (s *recordingConfigService) UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveFail != "" {
		return config.ServerProfileView{}, errors.New(s.saveFail + server.KeyID)
	}
	s.saves = append(s.saves, savedCredential{ServerID: id, KeyID: server.KeyID})
	s.runtime.Servers[id] = server
	select {
	case s.saved <- struct{}{}:
	default:
	}
	return config.ServerProfileView{ID: id, KeyID: server.KeyID, AuthMethod: server.AuthMethod}, nil
}

func (s *recordingConfigService) savedCredentials() []savedCredential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]savedCredential(nil), s.saves...)
}

// waitForSave blocks until one more credential save happened; the save runs on
// a background goroutine, so a signal replaces a sleep.
func (s *recordingConfigService) waitForSave(t *testing.T) {
	t.Helper()
	select {
	case <-s.saved:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a credential save")
	}
}

// newAuthRetryService wires a session service to a controlled SSH server whose
// profile starts with an intentionally wrong password, so every test here has
// to go through the real challenge/auth chain rather than a fake.
func newAuthRetryService(t *testing.T, srv *sshserver.Server, initialPassword string) (*Service, string, *recordingConfigService) {
	t.Helper()

	serverID := "loopback"
	cfgService := newRecordingConfigService(config.RuntimeConfig{
		Settings: config.Settings{KeepaliveInterval: "-1s"},
		Servers: map[string]config.ServerProfile{
			serverID: {
				ID:         serverID,
				Alias:      "loopback",
				Host:       srv.Host(),
				Port:       srv.Port(),
				User:       testSSHUser,
				AuthMethod: config.AuthMethodPassword,
				Password:   initialPassword,
			},
		},
	})

	pool := sshpool.NewPool()
	t.Cleanup(func() { pool.CloseAll() })

	service := NewService()
	service.UseConfig(cfgService)
	service.UsePool(pool)
	return service, serverID, cfgService
}

func startAuthRetryServer(t *testing.T) *sshserver.Server {
	t.Helper()
	return startSSHServer(t, sshserver.ShellBehavior{Scripted: true, ReadStdin: true, SendExitStatus: true})
}

// respondAuthChallenge answers the pending auth retry and waits until the
// client is back in the "connecting" gap, so a later wait for "auth_pending"
// can only observe the next registration.
func respondAuthChallenge(t *testing.T, service *Service, id string, resp ChallengeResponse) {
	t.Helper()
	if _, err := service.RespondAuthChallenge(id, resp); err != nil {
		t.Fatalf("respond auth challenge: %v", err)
	}
	waitForState(t, service, id, 5*time.Second, "connecting")
}

// R06/C04: the candidate is bound to the attempt that produced it. A rejected
// attempt is remembered, the attempt that succeeds is not, and nothing at all
// may be persisted - in particular not the earlier rejected password.
func TestReviewFailedRememberCandidateIsReplaced(t *testing.T) {
	srv := startAuthRetryServer(t)
	service, serverID, cfg := newAuthRetryService(t, srv, "wrong-password-1")

	created := createSession(t, service, serverID, CreateRequest{AllowAuthRetry: true})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondAuthChallenge(t, service, created.ID, ChallengeResponse{Password: "wrong-password-1", Remember: true})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondAuthChallenge(t, service, created.ID, ChallengeResponse{Password: testSSHPassword, Remember: false})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	if saved := cfg.savedCredentials(); len(saved) != 0 {
		t.Fatalf("saved credentials = %+v, want none: the rejected attempt's candidate survived", saved)
	}
}

// R06/C04: only the final successful candidate is written, exactly once, and to
// the resolved server ID even when the client supplied its own Alias.
func TestReviewOnlyFinalSuccessfulCandidateIsSaved(t *testing.T) {
	srv := startAuthRetryServer(t)
	service, serverID, cfg := newAuthRetryService(t, srv, "wrong-password-2")

	created := createSession(t, service, serverID, CreateRequest{
		AllowAuthRetry: true,
		Alias:          "client-chosen-alias",
	})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondAuthChallenge(t, service, created.ID, ChallengeResponse{Password: "wrong-password-2", Remember: true})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")

	respondAuthChallenge(t, service, created.ID, ChallengeResponse{Password: testSSHPassword, Remember: true})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	cfg.waitForSave(t)

	saved := cfg.savedCredentials()
	if len(saved) != 1 {
		t.Fatalf("saved credentials = %+v, want exactly one", saved)
	}
	if saved[0].Password != testSSHPassword {
		t.Fatalf("saved password = %q, want the password that actually worked", saved[0].Password)
	}
	if saved[0].ServerID != serverID {
		t.Fatalf("saved to %q, want the resolved server ID %q (Alias is display-only)", saved[0].ServerID, serverID)
	}
}

// R08: a background failure that arrives after the client cancelled the session
// must not rewrite the terminal state, its time or its cause.
func TestReviewFailureCannotOverwriteCancellation(t *testing.T) {
	srv := startAuthRetryServer(t)
	service, serverID, _ := newAuthRetryService(t, srv, "wrong-password-4")

	created := createSession(t, service, serverID, CreateRequest{AllowAuthRetry: true})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")

	cancelled, err := service.Disconnect(created.ID)
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if cancelled.State != "closed" {
		t.Fatalf("state after disconnect = %q, want closed", cancelled.State)
	}
	if cancelled.ExitedAt == nil {
		t.Fatal("cancelled session has no exit time")
	}

	// The challenge waiter is unblocked by the cancellation and reports its own
	// failure; and a client may still try to answer the stale challenge. Neither
	// may move the session out of the state the client asked for.
	if _, err := service.RespondAuthChallenge(created.ID, ChallengeResponse{Password: testSSHPassword}); err == nil {
		t.Fatal("a cancelled session accepted a challenge response")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := service.Get(created.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if current.State != "closed" {
			t.Fatalf("state = %q, want the cancelled state to stand", current.State)
		}
		if current.DisconnectCause != causeClientDisconnected {
			t.Fatalf("cause = %q, want %q", current.DisconnectCause, causeClientDisconnected)
		}
		if current.FrameworkError != "" {
			t.Fatalf("framework error = %q, want none for a client cancellation", current.FrameworkError)
		}
		if current.ExitedAt == nil || !current.ExitedAt.Equal(*cancelled.ExitedAt) {
			t.Fatal("the terminal timestamp was rewritten by a late failure")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The host key path is the same terminal commit, so a late prompt is refused
	// there too instead of re-entering a pending state.
	if _, err := service.RespondHostKeyChallenge(created.ID, ChallengeResponse{Accept: true}); err == nil {
		t.Fatal("a cancelled session accepted a host key response")
	}
}

// §15.1/C05: a failed save leaves the connection usable and tells the client
// through a warning that never carries the credential back.
func TestReviewCredentialSaveFailurePublishesSanitizedWarning(t *testing.T) {
	srv := startAuthRetryServer(t)
	service, serverID, cfg := newAuthRetryService(t, srv, "wrong-password-5")

	// The writer reports the credential back in its error, the way a real one
	// can; the warning must strip it before publishing.
	cfg.failSavesWith("cannot write password ")

	events := make(chan Event, 64)
	service.OnEvent(func(event Event) {
		select {
		case events <- event:
		default:
		}
	})

	created := createSession(t, service, serverID, CreateRequest{AllowAuthRetry: true})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")
	respondAuthChallenge(t, service, created.ID, ChallengeResponse{Password: "wrong-password-5", Remember: false})
	waitForState(t, service, created.ID, 5*time.Second, "auth_pending")
	respondAuthChallenge(t, service, created.ID, ChallengeResponse{Password: testSSHPassword, Remember: true})
	waitForState(t, service, created.ID, 5*time.Second, "connected")

	warning := waitForWarning(t, events, created.ID)
	if warning.Kind != WarningCredentialSaveFailed {
		t.Fatalf("warning kind = %q, want %q", warning.Kind, WarningCredentialSaveFailed)
	}
	if warning.Message == "" {
		t.Fatal("warning carries no message")
	}
	if strings.Contains(warning.Message, testSSHPassword) {
		t.Fatalf("warning leaked the credential: %q", warning.Message)
	}
	if warning.Message != "password was not saved" {
		t.Fatalf("warning = %q, want fixed safe save message", warning.Message)
	}

	// The connection stays usable: the failure is reported, never fatal.
	final, err := service.Get(created.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if final.State != "connected" {
		t.Fatalf("state = %q, want the working connection preserved", final.State)
	}
	if final.FrameworkError != "" {
		t.Fatalf("framework error = %q, want a warning instead of a failure", final.FrameworkError)
	}
}

func waitForWarning(t *testing.T, events <-chan Event, sessionID string) Warning {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.SessionID == sessionID && event.Warning != nil {
				return *event.Warning
			}
		case <-deadline:
			t.Fatal("no warning was published for the failed credential save")
			return Warning{}
		}
	}
}
