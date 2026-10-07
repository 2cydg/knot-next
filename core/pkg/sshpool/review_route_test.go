package sshpool

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"knot-core/pkg/config"
)

func TestReviewIntermediateJumpUsesNewPrefixIdentity(t *testing.T) {
	original := dialClient
	defer func() { dialClient = original }()
	calls := map[string]int{}
	var clients []*ssh.Client
	dialClient = func(_ context.Context, s config.ServerProfile, _ config.RuntimeConfig, _ *ssh.Client, _ func(HostKeyPrompt) bool, _ DialOptions) (*ssh.Client, error) {
		calls[s.ID]++
		conn := &reviewRouteConn{Release: make(chan struct{})}
		channels := make(chan ssh.NewChannel)
		close(channels)
		reqs := make(chan *ssh.Request)
		close(reqs)
		client := ssh.NewClient(conn, channels, reqs)
		clients = append(clients, client)
		return client, nil
	}
	a := config.ServerProfile{ID: "a", Host: "first-hop", User: "fake", Port: 22, AuthMethod: config.AuthMethodPassword, Password: "artificial-old"}
	b := config.ServerProfile{ID: "b", Host: "second-hop", User: "fake", Port: 22, AuthMethod: config.AuthMethodPassword, Password: "artificial-b"}
	target := config.ServerProfile{ID: "target", Host: "target", User: "fake", Port: 22, JumpHostIDs: []string{"a", "b"}, AuthMethod: config.AuthMethodPassword, Password: "artificial-target"}
	cfg := config.RuntimeConfig{Settings: config.Settings{KeepaliveInterval: "-1s"}, Servers: map[string]config.ServerProfile{"a": a, "b": b, "target": target}}
	pool := NewPool()
	defer pool.CloseAll()
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()
	_, keys, _, err := pool.GetClientContext(context.Background(), target, cfg, func(HostKeyPrompt) bool { return true }, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pool.IncRef(keys...)
	defer pool.DecRef(keys...)
	oldClients := append([]*ssh.Client(nil), clients...)
	a.Password = "artificial-new"
	cfg.Servers["a"] = a
	_, newKeys, _, err := pool.GetClientContext(context.Background(), target, cfg, func(HostKeyPrompt) bool { return true }, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		if !pool.IsAlive(key, oldClients[i]) {
			t.Fatalf("rotation closed old active route %s", key)
		}
		if newKeys[i] == key {
			t.Fatalf("new route reused old key %s", key)
		}
	}
	if calls["a"] != 2 || calls["b"] != 2 || calls["target"] != 2 {
		t.Fatalf("dial counts=%v: second jump reused a cached client built over old first-hop credentials; target digest change alone does not rebuild route", calls)
	}
}

func TestReviewPoolCloseEndsContextAwarePrompt(t *testing.T) {
	original := dialClient
	defer func() { dialClient = original }()
	entered := make(chan struct{})
	callbackDone := make(chan struct{})
	dialClient = func(ctx context.Context, _ config.ServerProfile, _ config.RuntimeConfig, _ *ssh.Client, confirm func(HostKeyPrompt) bool, _ DialOptions) (*ssh.Client, error) {
		confirm(HostKeyPrompt{})
		return nil, ctx.Err()
	}
	pool := NewPool()
	defer pool.CloseAll()
	done := make(chan error, 1)
	server := config.ServerProfile{ID: "fake", Host: "fake", Port: 22}
	go func() {
		_, _, _, err := pool.GetClientContextWithPrompt(context.Background(), server, config.RuntimeConfig{}, func(ctx context.Context, _ HostKeyPrompt) bool {
			close(entered)
			<-ctx.Done()
			close(callbackDone)
			return false
		}, DialOptions{})
		done <- err
	}()
	<-entered
	pool.CloseAll()
	select {
	case <-callbackDone:
	default:
		t.Fatal("prompt still active after CloseAll")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled prompt succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("creation still active")
	}
}
