package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knot-core/internal/paths"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
	"knot-core/pkg/crypto"
)

func TestRegressionRememberPasswordMethod(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "cfg"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewService(layout, crypto.NewStaticProvider([]byte("audit-key")))
	if _, err := cfg.CreateServer(config.ServerProfile{ID: "srv", Alias: "srv", Host: "host", Port: 22, User: "user", AuthMethod: config.AuthMethodAgent}); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.UseConfig(cfg)
	svc.saveCredentials("session", "srv", ChallengeResponse{Password: "verified-password", Remember: true})
	runtime, err := cfg.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := runtime.Servers["srv"]
	if got.AuthMethod != config.AuthMethodPassword {
		t.Errorf("password remembered but auth_method=%q; next dial still uses agent", got.AuthMethod)
	}
	if got.Password != "verified-password" {
		t.Fatal("password was not stored")
	}
}
func TestRegressionRememberKeyClearsPassword(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "cfg"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewService(layout, crypto.NewStaticProvider([]byte("audit-key")))
	if _, err := cfg.CreateKey(config.KeyMetadata{ID: "key", Alias: "key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.CreateServer(config.ServerProfile{ID: "srv", Alias: "srv", Host: "host", Port: 22, User: "user", AuthMethod: config.AuthMethodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetServerPassword("srv", "old-password"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.UseConfig(cfg)
	svc.saveCredentials("session", "srv", ChallengeResponse{KeyID: "key", Remember: true})
	runtime, err := cfg.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := runtime.Servers["srv"]
	if got.Password != "" {
		t.Error("key remembered but old password retained despite saveCredentials clearing profile.Password")
	}
}

func TestRegressionAliasResolution(t *testing.T) {
	cfg := config.RuntimeConfig{Servers: map[string]config.ServerProfile{"a": {ID: "a", Alias: "dup", Host: "host-a"}, "z": {ID: "z", Alias: "dup", Host: "host-z"}}}
	for i := 0; i < 200; i++ {
		got, err := resolveServer(cfg, "dup")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "a" {
			t.Errorf("alias dup resolves to %q, inconsistent with config.ResolveServer stable ID order", got.ID)
			return
		}
	}
}

type auditEnvChannel struct {
	*execTestChannel
	entered chan struct{}
	gate    chan struct{}
}

func (c *auditEnvChannel) SendRequest(kind string, want bool, data []byte) (bool, error) {
	if kind == "env" {
		close(c.entered)
		select {
		case <-c.gate:
		case <-c.closeCalled:
			return false, context.Canceled
		}
	}
	return true, nil
}

type auditEnvConn struct {
	*execTestConn
	channel *auditEnvChannel
}

func (c *auditEnvConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return c.channel, c.channel.requests, nil
}
func TestRegressionEnvironmentCancellation(t *testing.T) {
	ch := &execTestChannel{requests: make(chan *ssh.Request), started: make(chan struct{}), closeCalled: make(chan struct{}), finish: make(chan struct{})}
	env := &auditEnvChannel{execTestChannel: ch, entered: make(chan struct{}), gate: make(chan struct{})}
	conn := &auditEnvConn{execTestConn: &execTestConn{ch: ch, done: make(chan struct{})}, channel: env}
	channels := make(chan ssh.NewChannel)
	close(channels)
	requests := make(chan *ssh.Request)
	close(requests)
	client := ssh.NewClient(conn, channels, requests)
	defer client.Close()
	parent, work := resourcepolicy.Scope(context.Background())
	ctx, cancel := context.WithTimeout(parent, 50*time.Millisecond)
	defer cancel()
	sess, err := openSSHSession(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- setSSHSessionEnvironment(ctx, sess, map[string]string{"LANG": "en_US"}, nil) }()
	<-env.entered
	<-ctx.Done()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("env cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("env setup still blocked after attempt context deadline; no cancellation wrapper")
	}
	select {
	case <-ch.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("owned channel was not closed")
	}
	waitCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := work.Wait(waitCtx); err != nil {
		t.Fatal("setup workers did not settle", err)
	}
}
func TestRegressionControlBeforeReady(t *testing.T) {
	for _, state := range []string{"connecting", "host_key_pending", "auth_pending"} {
		for _, req := range []ControlRequest{{Type: "resize", Rows: 40, Cols: 100}, {Type: "signal", Signal: "INT"}, {Type: "close_stdin"}} {
			svc := NewService()
			svc.sessions["pending"] = &resource{Resource: Resource{ID: "pending", State: state}, subscribers: map[chan Event]struct{}{}}
			_, err := svc.Control("pending", req)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("%s/%s: %v", state, req.Type, err)
			}
			if svc.sessions["pending"].Rows != 0 {
				t.Fatal("pending resize mutated state")
			}
		}
	}
}

func TestInteractiveSetupDefaultStageDeadline(t *testing.T) {
	// Exercise the production 15-second deadline without a mutable global timer.
	ch := &execTestChannel{requests: make(chan *ssh.Request), started: make(chan struct{}), closeCalled: make(chan struct{}), finish: make(chan struct{})}
	conn := &execTestConn{ch: ch, done: make(chan struct{})}
	channels := make(chan ssh.NewChannel)
	close(channels)
	requests := make(chan *ssh.Request)
	close(requests)
	client := ssh.NewClient(conn, channels, requests)
	defer client.Close()
	ctx, work := resourcepolicy.Scope(context.Background())
	sess, err := openSSHSession(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = sessionSetup(ctx, sess, func() error { <-ch.closeCalled; return context.Canceled })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("setup deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed < interactiveSetupTimeout {
		t.Fatalf("stage duration: %v", elapsed)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := work.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
}
