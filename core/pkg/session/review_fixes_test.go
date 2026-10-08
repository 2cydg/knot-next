package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"knot-core/internal/paths"
	"knot-core/internal/resourcepolicy"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/crypto"
	"knot-core/pkg/sshpool"
)

func TestReviewEnvironmentRejectedKeepsSessionUsable(t *testing.T) {
	remote := sshserver.New(t, sshserver.Config{User: testSSHUser, Password: testSSHPassword, RejectEnv: true})
	svc, id, _ := newSSHService(t, remote)
	warning := make(chan Event, 8)
	svc.OnEvent(func(e Event) {
		if e.Warning != nil {
			warning <- e
		}
	})
	created := createSession(t, svc, id, CreateRequest{Env: map[string]string{"LANG": "private-value", "COLORTERM": "truecolor"}})
	waitForState(t, svc, created.ID, 5*time.Second, "connected")
	select {
	case e := <-warning:
		if e.Warning.Kind != WarningEnvironmentRejected || e.Warning.Message != "server rejected optional environment variables" {
			t.Fatalf("warning: %+v", e.Warning)
		}
	case <-time.After(time.Second):
		t.Fatal("missing environment warning")
	}
	if len(remote.EnvRequests()) != 2 {
		t.Fatal("did not attempt both optional variables")
	}
	stream, _, err := svc.AttachStream(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Release()
	defer stream.Cancel()
	input := []byte{0, 255, 27, '[', 'A', '\r', '\n'}
	if _, err := stream.Input.Write(input); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-stream.Stdout:
		if !bytes.Equal(got, input) {
			t.Fatalf("raw bytes: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("shell cannot echo after env rejection")
	}
}

func TestReviewEnvironmentEntryLimit(t *testing.T) {
	svc := NewService()
	svc.UseLocalTestBackend()
	env := map[string]string{}
	for i := 0; i < maxSSHEnvironmentEntries+1; i++ {
		env[fmt.Sprintf("ENV_%d", i)] = "x"
	}
	if _, err := svc.Create(CreateRequest{ServerRef: "server", Env: env}); !errors.Is(err, ErrValidation) {
		t.Fatalf("env limit: %v", err)
	}
	if len(svc.List()) != 0 {
		t.Fatal("invalid request registered a session")
	}
}

func TestReviewEmptyRememberPreservesStoredAuth(t *testing.T) {
	layout := paths.NewLayout(filepath.Join(t.TempDir(), "cfg"), filepath.Join(t.TempDir(), "state"))
	cfg := config.NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
	if _, err := cfg.CreateServer(config.ServerProfile{ID: "server", Alias: "server", Host: "host", Port: 22, User: "u", AuthMethod: config.AuthMethodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetServerPassword("server", "stored-password"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.UseConfig(cfg)
	svc.sessions["attempt"] = &resource{Resource: Resource{ID: "attempt", State: "connecting"}, subscribers: map[chan Event]struct{}{}}
	for _, resp := range []ChallengeResponse{{Remember: true}, {Remember: true, Passphrase: "attempt-only"}} {
		before, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		svc.saveCredentials("attempt", "server", resp)
		after, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("empty/passphrase-only remember changed saved config")
		}
	}
	runtime, err := cfg.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, stop, _, err := svc.Subscribe("attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := svc.waitAuthResponse(ctx, "attempt", runtime.Servers["server"], runtime, sshpool.ErrAuthFailed, 1)
		done <- err
	}()
	for e := range events {
		if e.Type == "session.auth.challenge" {
			break
		}
	}
	if _, err := svc.RespondAuthChallenge("attempt", ChallengeResponse{Remember: true}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	svc.mu.RLock()
	candidate := svc.sessions["attempt"].pendingCredentials
	svc.mu.RUnlock()
	if candidate != nil {
		t.Fatal("empty response queued a persistent choice")
	}
}

// A successful first request must not give the blocked second request a fresh
// per-request deadline beyond the environment phase budget.
type reviewBudgetChannel struct {
	*execTestChannel
	calls         int
	secondEntered chan struct{}
}

func (c *reviewBudgetChannel) SendRequest(kind string, want bool, data []byte) (bool, error) {
	if kind != "env" {
		return true, nil
	}
	c.calls++
	if c.calls == 1 {
		return true, nil
	}
	close(c.secondEntered)
	<-c.closeCalled
	return false, context.Canceled
}

type reviewBudgetConn struct {
	*execTestConn
	channel *reviewBudgetChannel
}

func (c *reviewBudgetConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return c.channel, c.channel.requests, nil
}
func TestReviewEnvironmentSharedPhaseBudget(t *testing.T) {
	ch := &execTestChannel{requests: make(chan *ssh.Request), started: make(chan struct{}), closeCalled: make(chan struct{}), finish: make(chan struct{})}
	env := &reviewBudgetChannel{execTestChannel: ch, secondEntered: make(chan struct{})}
	conn := &reviewBudgetConn{execTestConn: &execTestConn{ch: ch, done: make(chan struct{})}, channel: env}
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
	defer sess.Close()
	done := make(chan error, 1)
	go func() {
		done <- setSSHSessionEnvironmentWithTimeout(ctx, sess, map[string]string{"A": "a", "B": "b"}, nil, 500*time.Millisecond)
	}()
	select {
	case <-env.secondEntered:
	case <-done:
		t.Fatal("phase ended before second request")
	case <-time.After(time.Second):
		t.Fatal("first env request stuck")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("phase timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second env request received a fresh 15-second budget")
	}
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := work.Wait(wait); err != nil {
		t.Fatal("environment workers not settled", err)
	}
}
