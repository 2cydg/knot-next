package session

import (
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/sshpool"
)

// assertCancellationSticks is the C08 contract: once the client has ended a
// connection attempt, nothing that attempt was still waiting for may move the
// session again. The terminal state, its cause and its timestamp are the ones
// the cancellation committed, and the server never saw a session channel.
func assertCancellationSticks(t *testing.T, service *Service, cancelled Resource, srv *sshserver.Server, stage string) {
	t.Helper()

	if cancelled.State != "closed" {
		t.Fatalf("%s: state after disconnect = %q, want closed", stage, cancelled.State)
	}
	if cancelled.ExitedAt == nil {
		t.Fatalf("%s: the cancelled session has no exit time", stage)
	}
	if cancelled.DisconnectCause != causeClientDisconnected {
		t.Fatalf("%s: cause = %q, want %q", stage, cancelled.DisconnectCause, causeClientDisconnected)
	}
	if cancelled.FrameworkError != "" {
		t.Fatalf("%s: framework error = %q, want none for a client cancellation", stage, cancelled.FrameworkError)
	}

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		current, err := service.Get(cancelled.ID)
		if err != nil {
			t.Fatalf("%s: get session: %v", stage, err)
		}
		if current.State != "closed" {
			t.Fatalf("%s: state = %q, want the cancelled state to stand", stage, current.State)
		}
		if current.DisconnectCause != causeClientDisconnected {
			t.Fatalf("%s: cause = %q, want it preserved", stage, current.DisconnectCause)
		}
		if current.FrameworkError != "" {
			t.Fatalf("%s: a late failure rewrote the framework error: %q", stage, current.FrameworkError)
		}
		if current.ExitedAt == nil || !current.ExitedAt.Equal(*cancelled.ExitedAt) {
			t.Fatalf("%s: the terminal timestamp was rewritten by a late outcome", stage)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The cancelled stage never got as far as opening a channel or a session on
	// the remote, so nothing was left behind there either.
	if channels, sessions := srv.ChannelCount(), srv.SessionCount(); channels != 0 || sessions != 0 {
		t.Fatalf("%s: the cancelled attempt opened %d channels and %d sessions on the remote", stage, channels, sessions)
	}
}

// C08, SSH: a DELETE lands in the connecting, host key and authentication
// stages. Each one has a barrier holding that stage; the client ends the
// session while the barrier is held, the barrier is then released, and the
// stage's late completion must not revive anything.
func TestReviewCancellationInEveryConnectStage(t *testing.T) {
	t.Run("connecting", func(t *testing.T) {
		handshake := make(chan struct{})
		srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, SendExitStatus: true})
		srv.SetHandshakeBarrier(handshake)

		service, serverID, _ := newSSHService(t, srv)
		created := createSession(t, service, serverID, CreateRequest{
			HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip,
		})
		waitForState(t, service, created.ID, 5*time.Second, "connecting")

		cancelled, err := service.Disconnect(created.ID)
		if err != nil {
			t.Fatalf("disconnect while connecting: %v", err)
		}

		// The handshake the client was waiting for now completes; the attempt it
		// belongs to is already cancelled.
		close(handshake)
		assertCancellationSticks(t, service, cancelled, srv, "connecting")
	})

	t.Run("host_key", func(t *testing.T) {
		auth := make(chan struct{})
		srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, SendExitStatus: true})
		srv.SetAuthBarrier(auth)

		service, serverID, _ := newSSHService(t, srv)
		// Create is called directly: the shared helper fills in an empty policy
		// with insecure-skip, and ask is spelled as the empty string.
		created, err := service.Create(CreateRequest{
			ServerRef:     serverID,
			HostKeyPolicy: sshpool.HostKeyPolicyAsk,
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		waitForState(t, service, created.ID, 5*time.Second, "host_key_pending")

		cancelled, err := service.Disconnect(created.ID)
		if err != nil {
			t.Fatalf("disconnect while awaiting the host key: %v", err)
		}

		// Releasing the stage means answering the prompt the client is holding.
		// A cancelled session must refuse it rather than return to a pending
		// state, and the authentication it would have led to must not happen.
		if _, err := service.RespondHostKeyChallenge(created.ID, ChallengeResponse{Accept: true}); err == nil {
			t.Fatal("a cancelled session accepted a host key response")
		}
		close(auth)
		assertCancellationSticks(t, service, cancelled, srv, "host_key")
	})

	t.Run("auth", func(t *testing.T) {
		srv := startSSHServer(t, sshserver.ShellBehavior{Scripted: true, SendExitStatus: true})

		// The stored password is wrong, so the first attempt fails and the client
		// asks the user: that is the authentication stage the delete lands in. The
		// barrier holding this stage is the pending challenge itself, which the
		// client answers below.
		service, serverID, _ := newAuthRetryService(t, srv, "wrong-password-c08")
		created := createSession(t, service, serverID, CreateRequest{
			HostKeyPolicy:  sshpool.HostKeyPolicyInsecureSkip,
			AllowAuthRetry: true,
		})
		waitForState(t, service, created.ID, 5*time.Second, "auth_pending")

		cancelled, err := service.Disconnect(created.ID)
		if err != nil {
			t.Fatalf("disconnect while awaiting credentials: %v", err)
		}

		// Releasing the stage means supplying the credentials the client is
		// waiting for; the cancelled session must refuse them, and the retry they
		// would have started must never reach the remote.
		if _, err := service.RespondAuthChallenge(created.ID, ChallengeResponse{Password: testSSHPassword}); err == nil {
			t.Fatal("a cancelled session accepted a challenge response")
		}
		if _, err := service.RespondAuthChallenge(created.ID, ChallengeResponse{Password: testSSHPassword}); err == nil {
			t.Fatal("a cancelled session accepted a challenge response on the retry")
		}
		assertCancellationSticks(t, service, cancelled, srv, "auth")
	})
}
