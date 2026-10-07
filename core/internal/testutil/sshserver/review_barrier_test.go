package sshserver

import (
	"net"
	"testing"
	"time"
)

func waitFixture(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fixture failed to stop")
	}
}

func TestServerCloseReleasesEveryStageBarrier(t *testing.T) {
	for _, stage := range []string{"handshake", "auth", "subsystem"} {
		t.Run(stage, func(t *testing.T) {
			barrier := make(chan struct{})
			srv := &Server{closing: make(chan struct{}), listener: &memoryListener{done: make(chan struct{})}}
			switch stage {
			case "handshake":
				srv.beforeHandshake = barrier
			case "auth":
				srv.beforeAuth = barrier
			case "subsystem":
				srv.beforeSubsystem = barrier
			}
			entered, done := make(chan struct{}), make(chan struct{})
			result := make(chan bool, 1)
			// Even a regression failure releases and joins the old blocked code.
			defer func() { close(barrier); <-done }()
			go func() {
				defer close(done)
				accepted := srv.waitBarrier(func(s *Server) chan struct{} {
					close(entered)
					switch stage {
					case "handshake":
						return s.beforeHandshake
					case "auth":
						return s.beforeAuth
					default:
						return s.beforeSubsystem
					}
				})
				result <- accepted
			}()
			waitFixture(t, entered)
			srv.Close()
			waitFixture(t, done)
			if <-result {
				t.Fatal("closed fixture resumed the blocked stage")
			}
		})
	}
}

func TestServerCloseReleasesPendingHandshakeTransport(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	listener := &memoryListener{conn: remote, done: make(chan struct{})}
	srv := newServer(t, Config{User: "test", Password: "test"}, listener)
	// Wait until the handshake tries to send its identification. The peer stays
	// open but never replies; closing only authenticated connections would hang.
	firstByte := make(chan struct{})
	go func() { var b [1]byte; _, _ = local.Read(b[:]); close(firstByte) }()
	waitFixture(t, firstByte)
	srv.Close()
	done := make(chan struct{})
	go func() { srv.Wait(); close(done) }()
	waitFixture(t, done)
}
