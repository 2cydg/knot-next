package sshpool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knot-core/pkg/config"

	"golang.org/x/crypto/ssh"
)

// TestReviewJumpIdentityPropagatesToTarget covers RR06. A pooled target
// connection was authenticated by its jump host as well, so rotating the jump's
// credentials must change what the target's cache entry is keyed on: otherwise a
// later attempt reuses a connection that still runs over the old route.
func TestReviewJumpIdentityPropagatesToTarget(t *testing.T) {
	target := config.ServerProfile{ID: "target", Host: "127.0.0.1", Port: 22, JumpHostIDs: []string{"jump"}}
	jump := config.ServerProfile{
		ID: "jump", Host: "127.0.0.1", Port: 22,
		Password: "first-artificial", AuthMethod: config.AuthMethodPassword,
	}
	cfg := config.RuntimeConfig{Servers: map[string]config.ServerProfile{"target": target, "jump": jump}}

	first := clientIdentity(target, cfg, DialOptions{})

	rotated := jump
	rotated.Password = "second-artificial"
	cfg.Servers["jump"] = rotated
	second := clientIdentity(target, cfg, DialOptions{})

	if string(first) == string(second) {
		t.Fatal("the target's identity did not change when its jump host's credentials rotated")
	}

	// Other jump material participates too, not only the password.
	for name, change := range map[string]func(p *config.ServerProfile){
		"host":        func(p *config.ServerProfile) { p.Host = "10.0.0.9" },
		"user":        func(p *config.ServerProfile) { p.User = "other" },
		"key":         func(p *config.ServerProfile) { p.KeyID = "other-key" },
		"auth":        func(p *config.ServerProfile) { p.AuthMethod = config.AuthMethodKey },
		"known-hosts": func(p *config.ServerProfile) { p.KnownHostsPath = "/tmp/other-known-hosts" },
	} {
		t.Run("jump "+name, func(t *testing.T) {
			mutated := rotated
			change(&mutated)
			next := config.RuntimeConfig{Servers: map[string]config.ServerProfile{"target": target, "jump": mutated}}
			if string(clientIdentity(target, next, DialOptions{})) == string(second) {
				t.Fatalf("the target's identity ignored a change to its jump host's %s", name)
			}
		})
	}
}

// TestReviewSourceKeyContentParticipatesInIdentity covers the second half of
// RR06: the external key file's contents, not only its path, decide the
// identity, so replacing the file at the same path invalidates a cached
// connection that was authenticated with the old key.
func TestReviewSourceKeyContentParticipatesInIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	server := config.ServerProfile{ID: "fake", KeyID: "key", AuthMethod: config.AuthMethodKey}
	cfg := config.RuntimeConfig{Keys: map[string]config.KeyMetadata{"key": {ID: "key", SourcePath: path}}}

	if err := os.WriteFile(path, []byte("first-artificial-key-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := clientIdentity(server, cfg, DialOptions{})

	if err := os.WriteFile(path, []byte("second-artificial-key-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := clientIdentity(server, cfg, DialOptions{})

	if string(first) == string(second) {
		t.Fatal("replacing the key file at the same path did not change the identity")
	}

	// A key that cannot be read is not the same identity as an absent one, and
	// neither is the same as an inline key.
	missing := config.RuntimeConfig{Keys: map[string]config.KeyMetadata{"key": {ID: "key", SourcePath: filepath.Join(t.TempDir(), "absent")}}}
	if string(clientIdentity(server, missing, DialOptions{})) == string(second) {
		t.Fatal("an unreadable key file shares an identity with a readable one")
	}
}

// TestReviewPoolCloseCancelsInteractiveCreation covers RR05. An attempt that
// carries its own host key prompt is not registered as a shared inflight route,
// so nothing but the pool context can reach it: CloseAll must cancel it and wait
// for it to unwind rather than leave the dial running against a torn-down pool.
func TestReviewPoolCloseCancelsInteractiveCreation(t *testing.T) {
	original := dialClient
	t.Cleanup(func() { dialClient = original })

	previousGrace := attemptGrace
	attemptGrace = 2 * time.Second
	t.Cleanup(func() { attemptGrace = previousGrace })

	entered := make(chan struct{})
	release := make(chan struct{})
	dialClient = func(ctx context.Context, _ config.ServerProfile, _ config.RuntimeConfig, _ *ssh.Client, _ func(HostKeyPrompt) bool, _ DialOptions) (*ssh.Client, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, context.Canceled
		}
	}

	pool := NewPool()
	defer pool.CloseAll()
	defer close(release)

	server := config.ServerProfile{
		ID: "fake", Host: "127.0.0.1", Port: 22,
		AuthMethod: config.AuthMethodPassword, Password: "artificial",
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := pool.GetClientContext(context.Background(), server, config.RuntimeConfig{}, func(HostKeyPrompt) bool { return true }, DialOptions{})
		done <- err
	}()
	<-entered

	pool.CloseAll()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseAll did not cancel an interactive creation; the dial is still running")
	}

	// A pool that is already closed refuses a new interactive attempt outright
	// instead of dialling into a pool that no longer exists.
	if _, _, _, err := pool.GetClientContext(context.Background(), server, config.RuntimeConfig{}, func(HostKeyPrompt) bool { return true }, DialOptions{}); err == nil {
		t.Fatal("a closed pool accepted a new interactive creation")
	}
}
