package sftp

import (
	"context"
	"errors"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
)

// createThroughBarrier starts an SFTP session whose subsystem reply the server
// holds, and returns once the client is blocked waiting for it. The session
// channel being open proves the client got past the handshake, authenticated and
// issued the subsystem request.
func createThroughBarrier(t *testing.T, service *Service, srv *sshserver.Server, serverID string) Session {
	t.Helper()
	created, err := service.Create(CreateRequest{
		ServerRef:     serverID,
		HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for srv.ChannelCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the client never opened a session channel")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return created
}

// TestReviewSubsystemWaitHonoursCancellation covers RR03. Opening the SFTP
// subsystem is a network round-trip after authentication, so a client that stops
// waiting — a DELETE, or any caller whose context ends — must end the attempt
// instead of sitting in the library's constructor until the remote answers.
func TestReviewSubsystemWaitHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := startSFTPServer(t)
	srv.SetSubsystemBarrier(release)

	service, serverID, _ := newSFTPAuthService(t, srv, testAuthPassword)
	created := createThroughBarrier(t, service, srv, serverID)

	closed, err := service.Close(created.ID)
	if err != nil {
		t.Fatalf("close while the subsystem waits: %v", err)
	}
	if closed.State != "closed" {
		t.Fatalf("state = %q, want closed", closed.State)
	}

	// Releasing the held reply must not publish a subsystem into a session the
	// client already ended: the attempt that was cancelled must not come back to
	// life as a usable resource.
	close(release)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		current, err := service.Get(created.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if current.State != "closed" {
			t.Fatalf("state = %q, want the cancelled state to stand", current.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReviewCancelledSubsystemReleasesPoolReference covers the accounting half of
// RR03: the cancelled attempt must give back the reference it took on the shared
// connection, and the connection itself must survive so other sessions keep
// working.
func TestReviewCancelledSubsystemReleasesPoolReference(t *testing.T) {
	release := make(chan struct{})
	srv := startSFTPServer(t)
	srv.SetSubsystemBarrier(release)

	serverID := "loopback"
	pool := sshpool.NewPool()
	t.Cleanup(func() { pool.CloseAll() })

	service := NewService(t.TempDir())
	service.UseConfig(newRecordingConfigProvider(config.RuntimeConfig{
		Settings: config.Settings{KeepaliveInterval: "-1s"},
		Servers: map[string]config.ServerProfile{
			serverID: {
				ID: serverID, Alias: "loopback",
				Host: srv.Host(), Port: srv.Port(),
				User:       testAuthUser,
				AuthMethod: config.AuthMethodPassword,
				Password:   testAuthPassword,
			},
		},
	}))
	service.UsePool(pool)

	created := createThroughBarrier(t, service, srv, serverID)
	if _, err := service.Close(created.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The cancelled attempt held a reference while it waited; it must be returned
	// even though the subsystem never opened.
	waitForNoPoolReferences(t, pool)

	// The shared connection is still usable, so the cancelled attempt did not tear
	// it down on its way out.
	close(release)
	second := createThroughBarrier(t, service, srv, serverID)
	waitForSFTPState(t, service, second.ID, 5*time.Second, "open")
}

// waitForNoPoolReferences asserts every pooled connection has returned to zero
// references, which is what a cancelled creation must leave behind.
func waitForNoPoolReferences(t *testing.T, pool *sshpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []sshpool.EntryStat
	for time.Now().Before(deadline) {
		last = pool.Stats()
		if len(last) == 0 {
			return
		}
		held := false
		for _, stat := range last {
			if stat.RefCount != 0 {
				held = true
			}
		}
		if !held {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("a cancelled creation kept pool references: %+v", last)
}

// TestReviewLateChallengeCannotResurrect covers RR04. A challenge callback that
// arrives after the client ended the attempt must not move the session back to
// pending: doing so would let the cancellation branch rewrite the recorded
// outcome, its time and its cause.
func TestReviewLateChallengeCannotResurrect(t *testing.T) {
	t.Run("host key", func(t *testing.T) {
		service := NewService(t.TempDir())
		closedAt := time.Now().UTC().Add(-time.Minute)
		service.sessions["fake"] = &resource{Session: Session{
			ID:              "fake",
			State:           "closed",
			DisconnectCause: "client requested close",
			ClosedAt:        &closedAt,
		}}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if service.waitHostKeyResponse(ctx, "fake", config.ServerProfile{ID: "fake"}, sshpool.HostKeyPrompt{}) {
			t.Fatal("a late host key callback was accepted")
		}
		assertTerminalUntouched(t, service, "fake", closedAt, "client requested close")
	})

	t.Run("auth retry", func(t *testing.T) {
		service := NewService(t.TempDir())
		closedAt := time.Now().UTC().Add(-time.Minute)
		service.sessions["fake"] = &resource{Session: Session{
			ID:              "fake",
			State:           "failed",
			DisconnectCause: "connecting cancelled",
			ClosedAt:        &closedAt,
		}}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := service.waitAuthResponse(ctx, "fake", config.ServerProfile{ID: "fake"}, config.RuntimeConfig{}, errors.New("artificial auth failure"), 1); err == nil {
			t.Fatal("a late auth retry callback was accepted")
		}
		assertTerminalUntouched(t, service, "fake", closedAt, "connecting cancelled")
	})
}

// TestReviewResponseAfterCloseIsRefused covers the other end of RR04: a response
// that races a close must be refused, and the credential candidate it carries
// must not be left for a later attempt to persist.
func TestReviewResponseAfterCloseIsRefused(t *testing.T) {
	service := NewService(t.TempDir())
	service.sessions["fake"] = &resource{Session: Session{ID: "fake", State: "connecting"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		_, _ = service.waitAuthResponse(ctx, "fake", config.ServerProfile{ID: "fake"}, config.RuntimeConfig{}, errors.New("artificial auth failure"), 1)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		challenge, err := service.challenge("fake", "auth")
		if err == nil && challenge.Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the auth challenge was never registered")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if _, err := service.Close("fake"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := service.RespondAuthChallenge("fake", ChallengeResponse{Password: "artificial-secret", Remember: true}); err == nil {
		t.Fatal("a closed session accepted a challenge response")
	}
	cancel()
	<-waiting

	service.mu.RLock()
	res, ok := service.sessions["fake"]
	service.mu.RUnlock()
	if !ok {
		t.Fatal("the closed session disappeared from the service")
	}
	if res.pendingCredentials != nil {
		t.Fatal("a closed session stored a credential candidate")
	}
	if res.State != "closed" {
		t.Fatalf("state = %q, want closed", res.State)
	}
}

// assertTerminalUntouched re-reads a session and asserts its recorded outcome,
// time and pending markers survived a late callback.
func assertTerminalUntouched(t *testing.T, service *Service, id string, closedAt time.Time, cause string) {
	t.Helper()
	service.mu.RLock()
	res, ok := service.sessions[id]
	service.mu.RUnlock()
	if !ok {
		t.Fatalf("session %s disappeared", id)
	}
	if !isTerminalSessionState(res.State) {
		t.Fatalf("state = %q, want the terminal state to stand", res.State)
	}
	if res.ClosedAt == nil || !res.ClosedAt.Equal(closedAt) {
		t.Fatalf("the terminal timestamp was rewritten: %v", res.ClosedAt)
	}
	if res.DisconnectCause != cause {
		t.Fatalf("cause = %q, want %q", res.DisconnectCause, cause)
	}
	if res.hostKeyChallenge != nil || res.authChallenge != nil || res.HostKeyPending || res.AuthPending {
		t.Fatal("a late callback left a pending challenge on a terminal session")
	}
}
