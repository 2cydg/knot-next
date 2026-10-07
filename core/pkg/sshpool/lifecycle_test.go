package sshpool

import (
	"context"
	"errors"
	"golang.org/x/crypto/ssh"
	"knot-core/pkg/config"
	"sync"
	"testing"
	"time"
)

func TestPoolShutdownWaitsForCreationAndCleanup(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "interactive"}[interactive], func(t *testing.T) {
			original := dialClient
			defer func() { dialClient = original }()
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			dialClient = func(ctx context.Context, _ config.ServerProfile, _ config.RuntimeConfig, _ *ssh.Client, _ func(HostKeyPrompt) bool, _ DialOptions) (*ssh.Client, error) {
				close(started)
				<-release
				return nil, ctx.Err()
			}
			p := NewPool()
			defer p.CloseAll()
			profile := config.ServerProfile{ID: "pending", Host: "fake", User: "test", Port: 22}
			cfg := config.RuntimeConfig{Servers: map[string]config.ServerProfile{profile.ID: profile}}
			done := make(chan error, 1)
			go func() {
				var prompt func(HostKeyPrompt) bool
				if interactive {
					prompt = func(HostKeyPrompt) bool { return true }
				}
				_, _, _, err := p.GetClientContext(context.Background(), profile, cfg, prompt, DialOptions{HostKeyPolicy: HostKeyPolicyInsecureSkip})
				done <- err
			}()
			<-started
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unfinished dial hidden: %v", err)
			}
			if p.workers.Count() == 0 {
				t.Fatal("dial not tracked")
			}
			unblock()
			ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
			defer cancel2()
			if err := p.Shutdown(ctx2); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("closed pool published client")
				}
			case <-ctx2.Done():
				t.Fatal("caller did not finish")
			}
			if p.workers.Count() != 0 || p.Count() != 0 || len(p.inflight) != 0 {
				t.Fatal("pool ownership leaked")
			}
		})
	}
}
func TestPoolShutdownWaitsForKeepaliveAndCallbackReentry(t *testing.T) {
	p := NewPool()
	defer p.CloseAll()
	conn := &reviewRouteConn{Release: make(chan struct{})}
	channels := make(chan ssh.NewChannel)
	requests := make(chan *ssh.Request)
	close(channels)
	close(requests)
	client := ssh.NewClient(conn, channels, requests)
	callbackDone := make(chan struct{})
	p.ConnectCallback = func(string, *ssh.Client) { _ = p.Stats(); panic("callback panic") }
	p.DisconnectCallback = func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		close(callbackDone)
	}
	profile := config.ServerProfile{ID: "fake", Host: "fake", User: "test", Port: 22}
	key, _, _, err := p.publish("fake", routeStep{server: profile}, []byte("test"), client, config.RuntimeConfig{Settings: config.Settings{KeepaliveInterval: "1ms"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Ordinary transport loss delivers the advisory notification. Shutdown
	// itself drops pending observers, so trigger reentry before stopping the pool.
	p.dropEntryIfMatch(key, client, true)
	select {
	case <-callbackDone:
	case <-ctx.Done():
		t.Fatal("callback reentry blocked")
	}
	if p.workers.Count() != 0 {
		t.Fatal("keepalive or cleanup still running")
	}
}

func TestPoolIdleReplacementKeepsNewOwner(t *testing.T) {
	p := NewPool()
	defer p.CloseAll()
	newClient := func() *ssh.Client {
		conn := &reviewRouteConn{Release: make(chan struct{})}
		channels := make(chan ssh.NewChannel)
		requests := make(chan *ssh.Request)
		close(channels)
		close(requests)
		return ssh.NewClient(conn, channels, requests)
	}
	callbackStarted, release, fence := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	p.ConnectCallback = func(string, *ssh.Client) { first.Do(func() { close(callbackStarted); <-release }) }
	var mu sync.Mutex
	disconnects := 0
	p.DisconnectCallback = func(key string) { mu.Lock(); disconnects++; mu.Unlock(); p.DecRef(key) }
	profile := config.ServerProfile{ID: "replacement", Host: "test", User: "test", Port: 22}
	cfg := config.RuntimeConfig{Settings: config.Settings{KeepaliveInterval: "-1s"}}
	old := newClient()
	_, _, _, err := p.publish("same-key", routeStep{server: profile}, []byte("old material"), old, cfg)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("observer did not start")
	}
	replacement := newClient()
	key, _, _, err := p.publish("same-key", routeStep{server: profile}, []byte("new material"), replacement, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.IncRef(key)
	// This fence runs after any notification caused by replacing the old idle
	// connection, so an erroneous delayed disconnect cannot evade the assertion.
	p.callbacks.Send(func() { close(fence) })
	unblock()
	select {
	case <-fence:
	case <-time.After(time.Second):
		t.Fatal("callback queue did not drain")
	}
	mu.Lock()
	n := disconnects
	mu.Unlock()
	stats := p.Stats()
	if n != 0 || len(stats) != 1 || stats[0].RefCount != 1 || !p.IsAlive(key, replacement) {
		t.Fatalf("replacement owner affected: callbacks=%d stats=%+v", n, stats)
	}
	p.DecRef(key)
}
