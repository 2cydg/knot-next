package sftp

import (
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/sshpool"
)

// assertSFTPCancellationSticks is the C08 contract on the SFTP side: once the
// client has ended an attempt, nothing that attempt was still waiting for may
// move the session again, and the remote never saw a subsystem.
func assertSFTPCancellationSticks(t *testing.T, service *Service, cancelled Session, srv *sshserver.Server, stage string) {
	t.Helper()

	if cancelled.State != "closed" {
		t.Fatalf("%s: state after close = %q, want closed", stage, cancelled.State)
	}
	if cancelled.ClosedAt == nil {
		t.Fatalf("%s: the cancelled session has no close time", stage)
	}
	cause := cancelled.DisconnectCause

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		current, err := service.Get(cancelled.ID)
		if err != nil {
			t.Fatalf("%s: get session: %v", stage, err)
		}
		if current.State != "closed" {
			t.Fatalf("%s: state = %q, want the cancelled state to stand", stage, current.State)
		}
		if current.DisconnectCause != cause {
			t.Fatalf("%s: cause = %q, want %q preserved", stage, current.DisconnectCause, cause)
		}
		if current.ClosedAt == nil || !current.ClosedAt.Equal(*cancelled.ClosedAt) {
			t.Fatalf("%s: the terminal timestamp was rewritten by a late outcome", stage)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A session that was cancelled during connection never opened a channel, so
	// there is no subsystem, channel or client reference left on the remote.
	if channels, sessions := srv.ChannelCount(), srv.SessionCount(); channels != 0 || sessions != 0 {
		t.Fatalf("%s: the cancelled attempt opened %d channels and %d sessions on the remote", stage, channels, sessions)
	}
}

// C08, SFTP: the same three stages as the SSH path, against a real SFTP server,
// so the cancellation is proven on the transport the client actually uses.
func TestReviewSFTPCancellationInEveryConnectStage(t *testing.T) {
	t.Run("connecting", func(t *testing.T) {
		handshake := make(chan struct{})
		srv := startSFTPServer(t)
		srv.SetHandshakeBarrier(handshake)
		service, serverID, _ := newSFTPAuthService(t, srv, testAuthPassword)

		created, err := service.Create(CreateRequest{
			ServerRef:     serverID,
			HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip,
		})
		if err != nil {
			t.Fatalf("create sftp session: %v", err)
		}
		waitForSFTPState(t, service, created.ID, 5*time.Second, "connecting")

		cancelled, err := service.Close(created.ID)
		if err != nil {
			t.Fatalf("close while connecting: %v", err)
		}

		close(handshake)
		assertSFTPCancellationSticks(t, service, cancelled, srv, "connecting")
	})

	t.Run("host_key", func(t *testing.T) {
		srv := startSFTPServer(t)
		service, serverID, _ := newSFTPAuthService(t, srv, testAuthPassword)

		// Created directly: the empty policy means ask, and asking is what holds
		// the session in the host key stage.
		created, err := service.Create(CreateRequest{
			ServerRef:     serverID,
			HostKeyPolicy: sshpool.HostKeyPolicyAsk,
		})
		if err != nil {
			t.Fatalf("create sftp session: %v", err)
		}
		waitForSFTPState(t, service, created.ID, 5*time.Second, "host_key_pending")

		cancelled, err := service.Close(created.ID)
		if err != nil {
			t.Fatalf("close while awaiting the host key: %v", err)
		}

		if _, err := service.RespondHostKeyChallenge(created.ID, ChallengeResponse{Accept: true}); err == nil {
			t.Fatal("a cancelled sftp session accepted a host key response")
		}
		assertSFTPCancellationSticks(t, service, cancelled, srv, "host_key")
	})

	t.Run("auth", func(t *testing.T) {
		srv := startSFTPServer(t)
		service, serverID, _ := newSFTPAuthService(t, srv, "wrong-sftp-password-c08")

		created, err := service.Create(CreateRequest{
			ServerRef:      serverID,
			HostKeyPolicy:  sshpool.HostKeyPolicyInsecureSkip,
			AllowAuthRetry: true,
		})
		if err != nil {
			t.Fatalf("create sftp session: %v", err)
		}
		waitForSFTPState(t, service, created.ID, 5*time.Second, "auth_pending")

		cancelled, err := service.Close(created.ID)
		if err != nil {
			t.Fatalf("close while awaiting credentials: %v", err)
		}

		if _, err := service.RespondAuthChallenge(created.ID, ChallengeResponse{Password: testAuthPassword}); err == nil {
			t.Fatal("a cancelled sftp session accepted a challenge response")
		}
		if _, err := service.RespondAuthChallenge(created.ID, ChallengeResponse{Password: testAuthPassword}); err == nil {
			t.Fatal("a cancelled sftp session accepted a challenge response on the retry")
		}
		assertSFTPCancellationSticks(t, service, cancelled, srv, "auth")
	})
}
