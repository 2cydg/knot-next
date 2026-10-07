package sshpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"knot-core/pkg/config"
)

func leaseClient() *ssh.Client {
	conn := &reviewRouteConn{Release: make(chan struct{})}
	channels := make(chan ssh.NewChannel)
	requests := make(chan *ssh.Request)
	close(channels)
	close(requests)
	return ssh.NewClient(conn, channels, requests)
}

func leaseFixture(t *testing.T) (*Pool, config.ServerProfile, config.RuntimeConfig) {
	t.Helper()
	original := dialClient
	dialClient = func(context.Context, config.ServerProfile, config.RuntimeConfig, *ssh.Client, func(HostKeyPrompt) bool, DialOptions) (*ssh.Client, error) {
		return leaseClient(), nil
	}
	t.Cleanup(func() { dialClient = original })
	p := NewPool()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	profile := config.ServerProfile{ID: "fake", Host: "fake", Port: 22, User: "test", Password: "artificial-old"}
	cfg := config.RuntimeConfig{Settings: config.Settings{KeepaliveInterval: "-1s"}}
	return p, profile, cfg
}

func TestLeaseRejectsReplacedClient(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	// Deterministically reproduce the legacy Get/IncRef boundary: a new identity
	// replaces this idle client before acquisition validates the returned entry.
	old, keys, _, err := p.GetClientContext(context.Background(), profile, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rotated := profile
	rotated.Password = "artificial-new"
	newClient, newKeys, _, err := p.GetClientContext(context.Background(), rotated, cfg, nil, DialOptions{})
	if err != nil || newKeys[0] != keys[0] || newClient == old {
		t.Fatalf("idle replacement not reproduced: %v %v", newKeys, err)
	}
	if _, _, held := p.retainClient(keys[0], old); held {
		t.Fatal("stale client acquired a replacement's reference")
	}
	if stats := p.Stats(); len(stats) != 1 || stats[0].RefCount != 0 {
		t.Fatalf("replacement reference was incremented: %+v", stats)
	}
	lease, err := p.AcquireClientContext(context.Background(), profile, cfg, nil, DialOptions{})
	if err != nil || !p.IsAlive(lease.Keys[0], lease.Client) {
		t.Fatalf("retry did not obtain a live client: %v", err)
	}
	defer lease.Release()
	other, err := p.AcquireClientContextWithPrompt(context.Background(), rotated, cfg, func(context.Context, HostKeyPrompt) bool { return true }, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if other.Keys[0] == lease.Keys[0] || !p.IsAlive(lease.Keys[0], lease.Client) {
		t.Fatal("new identity replaced an acquired owner")
	}
	for _, stat := range p.Stats() {
		if stat.RefCount != 1 {
			t.Fatalf("lease was not held on return: %+v", stat)
		}
	}
}

func TestLeaseClearAndRepeatedReleaseKeepNewOwner(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	old, err := p.AcquireClientContext(context.Background(), profile, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Clear()
	current, err := p.AcquireClientContext(context.Background(), profile, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	if current.Keys[0] != old.Keys[0] {
		t.Fatal("test needs a reused key")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); old.Release() }()
	}
	wg.Wait()
	if stats := p.Stats(); len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatalf("old release affected new owner: %+v", stats)
	}
	current.Release()
	current.Release()
	if stats := p.Stats(); len(stats) != 1 || stats[0].RefCount != 0 {
		t.Fatalf("lease release leaked references: %+v", stats)
	}
}

func TestLeaseHoldsJumpDuringTargetDial(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "fail"}[fail], func(t *testing.T) {
			p, jump, cfg := leaseFixture(t)
			jump.ID = "jump"
			target := config.ServerProfile{ID: "target", Host: "target", Port: 22, JumpHostIDs: []string{jump.ID}}
			cfg.Servers = map[string]config.ServerProfile{jump.ID: jump, target.ID: target}
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			dialClient = func(_ context.Context, profile config.ServerProfile, _ config.RuntimeConfig, via *ssh.Client, _ func(HostKeyPrompt) bool, _ DialOptions) (*ssh.Client, error) {
				if profile.ID == target.ID {
					if via == nil {
						t.Error("target has no jump client")
					}
					close(started)
					<-release
					if fail {
						return nil, errors.New("artificial dial failure")
					}
				}
				return leaseClient(), nil
			}
			done := make(chan struct{})
			var targetLease *ClientLease
			var targetErr error
			go func() {
				targetLease, targetErr = p.AcquireClientContext(context.Background(), target, cfg, nil, DialOptions{})
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("target dial never started")
			}
			stats := p.Stats()
			if len(stats) != 1 || stats[0].RefCount != 2 {
				t.Fatalf("jump unowned during target dial: %+v", stats)
			}
			oldKey := stats[0].Key
			jump.Password = "artificial-rotated"
			rotated, err := p.AcquireClientContext(context.Background(), jump, cfg, nil, DialOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer rotated.Release()
			if rotated.Keys[0] == oldKey || !p.HasKey(oldKey) {
				t.Fatal("rotation replaced jump used by target creation")
			}
			unblock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("target acquisition did not finish")
			}
			if fail {
				if targetErr == nil {
					t.Fatal("target unexpectedly succeeded")
				}
			} else {
				if targetErr != nil {
					t.Fatal(targetErr)
				}
				if targetLease.Keys[0] != oldKey {
					t.Fatal("target switched jump owner")
				}
				targetLease.Release()
			}
			for _, stat := range p.Stats() {
				if stat.Key != rotated.Keys[0] && stat.RefCount != 0 {
					t.Fatalf("partial/complete route release leaked: %+v", stat)
				}
			}
		})
	}
}

func TestPoolShutdownDropsPendingObservers(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	p.ConnectCallback = func(string, *ssh.Client) {
		close(started)
		<-release
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		close(finished)
	}
	unexpected := make(chan struct{}, 1)
	p.DisconnectCallback = func(string) { unexpected <- struct{}{} }
	lease, err := p.AcquireClientContext(context.Background(), profile, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	<-started
	p.notifyDisconnect(lease.Keys[0])
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("running observer could not reenter shutdown")
	}
	select {
	case <-unexpected:
		t.Fatal("shutdown retained a queued observer")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestLeaseSharedPublicationUsesActualRevisionKey(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	dialClient = func(context.Context, config.ServerProfile, config.RuntimeConfig, *ssh.Client, func(HostKeyPrompt) bool, DialOptions) (*ssh.Client, error) {
		close(started)
		<-release
		return leaseClient(), nil
	}
	var lease *ClientLease
	var acquireErr error
	done := make(chan struct{})
	go func() {
		lease, acquireErr = p.AcquireClientContext(context.Background(), profile, cfg, nil, DialOptions{})
		close(done)
	}()
	<-started
	routes, err := routeChain(profile, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rotated := profile
	rotated.Password = "artificial-new"
	client := leaseClient()
	key, _, _, err := p.publish(routes[0].key, routes[0], clientIdentity(rotated, cfg, DialOptions{}), client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, releaseOther, held := p.retainClient(key, client)
	if !held {
		t.Fatal("other identity was not acquired")
	}
	defer releaseOther()
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shared creation did not return its revised key")
	}
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	defer lease.Release()
	if lease.Keys[0] == key || !p.IsAlive(lease.Keys[0], lease.Client) {
		t.Fatal("creation returned its initial key instead of publication key")
	}
	for _, stat := range p.Stats() {
		if stat.RefCount != 1 {
			t.Fatalf("publication acquired the wrong entry: %+v", stat)
		}
	}
}

type leaseBlockedClose struct {
	*reviewRouteConn
	started chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (c *leaseBlockedClose) Close() error {
	c.once.Do(func() { close(c.started) })
	<-c.gate
	return c.reviewRouteConn.Close()
}

func TestLeaseRetriesReplacementDuringAcquisition(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	routes, err := routeChain(profile, cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn := &leaseBlockedClose{reviewRouteConn: &reviewRouteConn{Release: make(chan struct{})}, started: make(chan struct{}), gate: make(chan struct{})}
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(conn.gate) }) }
	defer unblock()
	channels, requests := make(chan ssh.NewChannel), make(chan *ssh.Request)
	close(channels)
	close(requests)
	old := ssh.NewClient(conn, channels, requests)
	if _, _, _, err := p.publish(routes[0].key, routes[0], clientIdentity(profile, cfg, DialOptions{}), old, cfg); err != nil {
		t.Fatal(err)
	}
	intermediate := profile
	intermediate.Password = "artificial-intermediate"
	var lease *ClientLease
	var acquireErr error
	done := make(chan struct{})
	go func() {
		lease, acquireErr = p.AcquireClientContextWithPrompt(context.Background(), intermediate, cfg, func(context.Context, HostKeyPrompt) bool { return true }, DialOptions{})
		close(done)
	}()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("replacement did not reach old-client close")
	}
	// The intermediate publication is paused after installing its idle entry
	// but before returning it. A third identity replaces it and acquires the key.
	latest := profile
	latest.Password = "artificial-latest"
	current, err := p.AcquireClientContext(context.Background(), latest, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acquisition did not retry the replaced client")
	}
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	defer lease.Release()
	if lease.Keys[0] == current.Keys[0] || !p.IsAlive(lease.Keys[0], lease.Client) || !p.IsAlive(current.Keys[0], current.Client) {
		t.Fatal("retry returned the closed client or disturbed the new owner")
	}
	for _, stat := range p.Stats() {
		if stat.RefCount != 1 {
			t.Fatalf("retry acquired a replacement's reference: %+v", stat)
		}
	}
}

func TestLeaseValidationAndCanceledAcquisition(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	cfg.Settings.IdleTimeout = "1m"
	lease, err := p.AcquireClientContextWithPrompt(nil, profile, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	invalid := profile
	invalid.JumpHostIDs = []string{"missing"}
	if _, err := p.AcquireClientContext(context.Background(), invalid, cfg, nil, DialOptions{}); err == nil {
		t.Fatal("invalid route accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.AcquireClientContext(ctx, profile, cfg, nil, DialOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition accepted: %v", err)
	}
	for _, stat := range p.Stats() {
		if stat.RefCount != 0 {
			t.Fatalf("validation retained references: %+v", stat)
		}
	}
}

func TestLeaseCanceledDialKeepsJumpWorkerOwner(t *testing.T) {
	p, jump, cfg := leaseFixture(t)
	jump.ID = "jump"
	target := config.ServerProfile{ID: "target", Host: "target", Port: 22, JumpHostIDs: []string{jump.ID}}
	cfg.Servers = map[string]config.ServerProfile{jump.ID: jump, target.ID: target}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	dialClient = func(ctx context.Context, profile config.ServerProfile, _ config.RuntimeConfig, _ *ssh.Client, _ func(HostKeyPrompt) bool, _ DialOptions) (*ssh.Client, error) {
		if profile.ID == target.ID {
			close(started)
			<-release // Model a third-party dial that has not settled after cancel.
			return nil, ctx.Err()
		}
		return leaseClient(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := p.AcquireClientContext(ctx, target, cfg, nil, DialOptions{}); returned <- err }()
	<-started
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller did not return: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller waited for actual dial cleanup")
	}
	stats := p.Stats()
	if len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatalf("caller cancellation returned pending dial's jump reference: %+v", stats)
	}
	oldKey := stats[0].Key
	p.mu.Lock()
	pendingJump := p.entries[oldKey]
	p.mu.Unlock()
	jump.Password = "artificial-rotated"
	current, err := p.AcquireClientContext(context.Background(), jump, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	if current.Keys[0] == oldKey || !p.HasKey(oldKey) {
		t.Fatal("rotation replaced the pending worker's jump")
	}
	// Pool shutdown owns the pending dial and reports a deadline until it ends.
	budget, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if err := p.Shutdown(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending dial was reported released: %v", err)
	}
	unblock()
	budget2, stop2 := context.WithTimeout(context.Background(), time.Second)
	defer stop2()
	if err := p.Shutdown(budget2); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	refs := pendingJump.refCount
	p.mu.Unlock()
	if refs != 0 {
		t.Fatalf("settled dial retained jump references: %d", refs)
	}
}

func TestPrefixReleaseConcurrentAndIdleCleanup(t *testing.T) {
	p, jump, cfg := leaseFixture(t)
	jump.ID = "jump"
	target := config.ServerProfile{ID: "target", Host: "target", Port: 22, JumpHostIDs: []string{jump.ID}}
	cfg.Servers = map[string]config.ServerProfile{jump.ID: jump, target.ID: target}
	lease, err := p.AcquireClientContext(context.Background(), target, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	p.mu.Lock()
	release := p.retainPrefixLocked(lease.entries)
	p.mu.Unlock()
	defer release()
	for _, stat := range p.Stats() {
		if stat.RefCount != 2 {
			t.Fatalf("prefix borrow did not retain route: %+v", stat)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	release()
	for _, stat := range p.Stats() {
		if stat.RefCount != 1 {
			t.Fatalf("duplicate prefix release affected original owner: %+v", stat)
		}
	}
	lease.Release()
	p.mu.Lock()
	p.idleTimeout = time.Minute
	for _, ent := range p.entries {
		ent.lastAccess = time.Now().Add(-time.Hour)
	}
	p.mu.Unlock()
	p.cleanupIdle()
	if p.Count() != 0 {
		t.Fatal("settled route could not be reclaimed as idle")
	}
}

func TestCreationPanicReleasesPrefix(t *testing.T) {
	p, profile, cfg := leaseFixture(t)
	lease, err := p.AcquireClientContext(context.Background(), profile, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	p.mu.Lock()
	release := p.retainPrefixLocked(lease.entries)
	p.workers.Add()
	p.mu.Unlock()
	dialClient = func(context.Context, config.ServerProfile, config.RuntimeConfig, *ssh.Client, func(HostKeyPrompt) bool, DialOptions) (*ssh.Client, error) {
		panic("artificial dial panic")
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		p.runCreation(context.Background(), "target", routeStep{server: profile}, nil, cfg, lease.Client, DialOptions{}, &inflightRoute{done: make(chan struct{})}, release)
	}()
	if recovered != "artificial dial panic" {
		t.Fatalf("did not exercise dial unwinding: %v", recovered)
	}
	if stats := p.Stats(); len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatalf("unwinding leaked the worker's prefix reference: %+v", stats)
	}
}
