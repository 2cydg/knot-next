package forward

import (
	"context"
	"errors"
	"io"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
	"net"
	"sync"
	"testing"
	"time"
)

func forwardingFixture(t *testing.T) (*Service, *sshpool.Pool, *sshpool.ClientLease, *sshserver.Server) {
	t.Helper()
	remote := sshserver.New(t, sshserver.Config{User: "fixture", Password: "artificial-password"})
	pool := sshpool.NewPool()
	profile := config.ServerProfile{ID: "srv", Alias: "web", Host: remote.Host(), Port: remote.Port(), User: "fixture", AuthMethod: "password", Password: "artificial-password"}
	cfg := config.RuntimeConfig{Servers: map[string]config.ServerProfile{profile.ID: profile}, Settings: config.Settings{KeepaliveInterval: "0s"}}
	lease, err := pool.AcquireClientContext(t.Context(), profile, cfg, nil, sshpool.DialOptions{HostKeyPolicy: "insecure-skip"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(pool)
	t.Cleanup(func() {
		lease.Release()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = pool.Shutdown(ctx)
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return service, pool, lease, remote
}
func TestRemoteStopKeepsLeaseUntilLateRequestSettles(t *testing.T) {
	service, pool, lease, remote := forwardingFixture(t)
	barrier := make(chan struct{})
	started := make(chan struct{}, 1)
	remote.SetForwardBarrier(barrier, started)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(barrier) }) })
	instance, err := service.Create(Request{ServerID: "srv", ConnectionID: lease.ConnectionID, Type: "remote", Bind: "127.0.0.1:0", Target: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	// The setup worker has registered before Stop, even if the remote has not yet
	// received the global request. A late reply remains owned by this instance.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("remote forward request not received")
	}
	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	view, err := service.Stop(budget, instance.ID)
	if !errors.Is(err, context.DeadlineExceeded) || view.State != "stopping" {
		t.Fatalf("hidden late setup: %+v %v", view, err)
	}
	stats := pool.Stats()
	if len(stats) != 1 || stats[0].RefCount != 2 {
		t.Fatalf("lease released before late request settled: %+v", stats)
	}
	release.Do(func() { close(barrier) })
	budget2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	view, err = service.Stop(budget2, instance.ID)
	if err != nil || view.State != "stopped" {
		t.Fatalf("late cleanup: %+v %v", view, err)
	}
	if stats = pool.Stats(); len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatalf("forward released somebody else's owner: %+v", stats)
	}
}
func TestConcurrentAutoRegistrationAndTerminalRetention(t *testing.T) {
	service, _, lease, _ := forwardingFixture(t)
	defs := []config.ForwardDefinition{{ID: "rule", Type: "dynamic", Bind: "127.0.0.1:0", Enabled: true}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); service.EnsureAutoForwards(lease.ConnectionID, "srv", defs) }()
	}
	wg.Wait()
	instances := service.List("srv", lease.ConnectionID)
	if len(instances) != 1 {
		t.Fatalf("concurrent auto duplicates: %+v", instances)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := service.Stop(ctx, instances[0].ID); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.policy.History = 0
	service.mu.Unlock()
	if _, err := service.Get(instances[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("settled history did not prune")
	}
	service.EnsureAutoForwards(lease.ConnectionID, "srv", defs)
	if len(service.List("srv", lease.ConnectionID)) != 0 {
		t.Fatal("history pruning restarted automatic snapshot")
	}
}

func TestShutdownClosesHandshakeStreamsAndKeepsOtherTransportOwner(t *testing.T) {
	service, pool, lease, _ := forwardingFixture(t)
	v, err := service.Create(Request{ServerID: "srv", Type: "dynamic", Bind: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		v, err = service.Get(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == "active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(v)
		}
		time.Sleep(time.Millisecond)
	}
	conn, err := net.DialTimeout("tcp", v.BoundAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Force a real SOCKS greeting read in progress before shutdown.
	_, _ = conn.Write([]byte{5, 1, 0})
	var method [2]byte
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err = io.ReadFull(conn, method[:]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = service.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err = conn.Read(b[:]); err == nil {
		t.Fatal("shutdown retained accepted stream")
	}
	stats := pool.Stats()
	if len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatal("shutdown released other transport owner", stats)
	}
	channel, err := lease.Client.NewSession()
	if err != nil {
		t.Fatal("forward shutdown closed shared transport", err)
	}
	_ = channel.Close()
}
