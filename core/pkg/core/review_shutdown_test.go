package core

import (
	"context"
	"testing"
	"time"

	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

const (
	shutdownTestUser     = "testuser"
	shutdownTestPassword = "testpass"
	shutdownTestServerID = "loopback"
)

// shutdownTestConfig is a deterministic config source: the review forbids tests
// that depend on a running platform credential store.
type shutdownTestConfig struct {
	runtime config.RuntimeConfig
}

func (c *shutdownTestConfig) RuntimeConfig() (config.RuntimeConfig, error) {
	return c.runtime, nil
}

func (c *shutdownTestConfig) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	return config.ServerProfileView{ID: id, PasswordSet: password != ""}, nil
}

func (c *shutdownTestConfig) UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error) {
	return config.ServerProfileView{ID: id, KeyID: server.KeyID, AuthMethod: server.AuthMethod}, nil
}

// shutdownFixture holds a core service wired to live session and SFTP resources
// over one shared pool, all on loopback.
type shutdownFixture struct {
	core     *Service
	sessions *session.Service
	sftpSvc  *sftp.Service
	pool     *sshpool.Pool
	server   *sshserver.Server
	sshID    string
	sftpID   string
}

func newShutdownFixture(t *testing.T) *shutdownFixture {
	t.Helper()

	handlers := protocolsftp.InMemHandler()
	srv := sshserver.New(t, sshserver.Config{
		User:         shutdownTestUser,
		Password:     shutdownTestPassword,
		SFTPHandlers: &handlers,
	})
	t.Cleanup(func() {
		srv.Close()
		srv.Wait()
	})

	provider := &shutdownTestConfig{runtime: config.RuntimeConfig{
		Settings: config.Settings{KeepaliveInterval: "-1s"},
		Servers: map[string]config.ServerProfile{
			shutdownTestServerID: {
				ID:         shutdownTestServerID,
				Alias:      "loopback",
				Host:       srv.Host(),
				Port:       srv.Port(),
				User:       shutdownTestUser,
				AuthMethod: config.AuthMethodPassword,
				Password:   shutdownTestPassword,
			},
		},
	}}

	pool := sshpool.NewPool()
	t.Cleanup(func() { pool.CloseAll() })

	sessions := session.NewService()
	sessions.UseConfig(provider)
	sessions.UsePool(pool)

	sftpSvc := sftp.NewService(t.TempDir())
	sftpSvc.UseConfig(provider)
	sftpSvc.UsePool(pool)

	service := New("test", time.Now())
	service.UseSession(sessions)
	service.UseSFTP(sftpSvc)
	service.UseSSHPool(pool)

	return &shutdownFixture{core: service, sessions: sessions, sftpSvc: sftpSvc, pool: pool, server: srv}
}

// startResources opens one live SSH session and one live SFTP session, both over
// the same pooled connection.
func (f *shutdownFixture) startResources(t *testing.T) {
	t.Helper()

	created, err := f.sessions.Create(session.CreateRequest{
		ServerRef:     shutdownTestServerID,
		HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create ssh session: %v", err)
	}
	f.sshID = created.ID
	waitForState(t, func() (string, error) {
		res, err := f.sessions.Get(f.sshID)
		return res.State, err
	}, "connected")

	sftpCreated, err := f.sftpSvc.Create(sftp.CreateRequest{
		ServerRef:     shutdownTestServerID,
		HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip,
	})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	f.sftpID = sftpCreated.ID
	waitForState(t, func() (string, error) {
		res, err := f.sftpSvc.Get(f.sftpID)
		return res.State, err
	}, "open")

	if f.pool.Count() == 0 {
		t.Fatal("a live session left no pooled connection behind")
	}
}

func waitForState(t *testing.T, read func() (string, error), want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		state, err := read()
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		last = state
		for _, candidate := range want {
			if state == candidate {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("resource stayed in state %q, want one of %v", last, want)
}

func terminalSessionState(state string) bool {
	return state == "closed" || state == "failed"
}

func terminalSFTPState(state string) bool {
	return state == "closed" || state == "failed" || state == "disconnected"
}

// totalRefs sums the references the pool holds across every connection.
func totalRefs(pool *sshpool.Pool) int {
	total := 0
	for _, stat := range pool.Stats() {
		total += stat.RefCount
	}
	return total
}

// TestReviewShutdownReleasesLiveResources covers RR01 with the resources the
// review asked for: a real SSH session and a real SFTP subsystem sharing one
// pooled connection. Shutdown must close both, return the pool references they
// held, and leave no worker running — checking the HTTP port or a connection
// tracker would pass even while both stayed alive.
func TestReviewShutdownReleasesLiveResources(t *testing.T) {
	fixture := newShutdownFixture(t)
	fixture.startResources(t)

	if refs := totalRefs(fixture.pool); refs == 0 {
		t.Fatal("live resources hold no pool references, so the test cannot observe them being released")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fixture.core.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// The three classes race, and two of them can legitimately end the same
	// session: an explicit close records "closed", the shared connection going
	// away records "disconnected". Either is a released resource; still being
	// live is not.
	sshSession, err := fixture.sessions.Get(fixture.sshID)
	if err != nil {
		t.Fatalf("get ssh session: %v", err)
	}
	if !terminalSessionState(sshSession.State) {
		t.Fatalf("ssh session state = %q, want a terminal state", sshSession.State)
	}
	sftpSession, err := fixture.sftpSvc.Get(fixture.sftpID)
	if err != nil {
		t.Fatalf("get sftp session: %v", err)
	}
	if !terminalSFTPState(sftpSession.State) {
		t.Fatalf("sftp session state = %q, want a terminal state", sftpSession.State)
	}
	if sftpSession.ClosedAt == nil {
		t.Fatal("a released sftp session recorded no close time")
	}

	if refs := totalRefs(fixture.pool); refs != 0 {
		t.Fatalf("shutdown left %d pool references behind", refs)
	}
	if count := fixture.pool.Count(); count != 0 {
		t.Fatalf("shutdown left %d pooled connections behind", count)
	}
}

// TestReviewShutdownReportsAnExpiredBudget keeps the other direction honest: a
// shutdown that could not release everything must not report success. The
// budget is already gone before the first release starts, so every class is
// skipped and the caller has to see that.
func TestReviewShutdownReportsAnExpiredBudget(t *testing.T) {
	fixture := newShutdownFixture(t)
	fixture.startResources(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fixture.core.Shutdown(ctx)
	if err == nil {
		t.Fatal("shutdown with an expired budget reported success")
	}
	if res, getErr := fixture.sessions.Get(fixture.sshID); getErr != nil {
		t.Fatalf("get ssh session: %v", getErr)
	} else if res.State == "closed" {
		t.Fatal("an expired budget still closed a resource, so the report and the work disagree")
	}
}

// TestReviewShutdownIsIdempotent checks that running teardown twice — the
// lifecycle runner and a direct caller — neither fails nor rewrites the
// recorded outcome of the sessions it already released.
func TestReviewShutdownIsIdempotent(t *testing.T) {
	fixture := newShutdownFixture(t)
	fixture.startResources(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fixture.core.Shutdown(ctx); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	first, err := fixture.sftpSvc.Get(fixture.sftpID)
	if err != nil {
		t.Fatalf("get sftp session: %v", err)
	}
	if err := fixture.core.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	second, err := fixture.sftpSvc.Get(fixture.sftpID)
	if err != nil {
		t.Fatalf("get sftp session: %v", err)
	}
	if !second.ClosedAt.Equal(*first.ClosedAt) {
		t.Fatalf("the second shutdown rewrote the close time: %v then %v", first.ClosedAt, second.ClosedAt)
	}
}
