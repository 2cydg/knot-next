package sshpool

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"

	"golang.org/x/crypto/ssh"
)

// startSilentServer accepts TCP connections and never speaks SSH. It is the
// reproduction for R07's second gap: after the TCP connection is established a
// remote that sends nothing must not block the attempt forever, because
// ssh.ClientConfig.Timeout does not cover the handshake performed by
// ssh.NewClientConn.
func startSilentServer(t *testing.T) (config.ServerProfile, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := &atomic.Int32{}
	var mu sync.Mutex
	closed := false
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			accepted.Add(1)
			mu.Unlock()
			// Hold the connection open without ever replying.
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		closed = true
		mu.Unlock()
		_ = listener.Close()
	})

	host, portStr, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return config.ServerProfile{
		ID:             "silent",
		Alias:          "silent",
		Host:           host,
		Port:           port,
		User:           "tester",
		Password:       "secret",
		AuthMethod:     config.AuthMethodPassword,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
	}, accepted
}

// realServerProfile builds a profile for the controlled SSH test server, which
// speaks the real protocol on a real TCP socket.
func realServerProfile(t *testing.T, srv *sshserver.Server, password string) config.ServerProfile {
	t.Helper()
	return config.ServerProfile{
		ID:             "loopback",
		Alias:          "loopback",
		Host:           srv.Host(),
		Port:           srv.Port(),
		User:           "testuser",
		Password:       password,
		AuthMethod:     config.AuthMethodPassword,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
	}
}

// C06/C01: a remote that never completes the SSH handshake must not hold the
// attempt open; the connection phase deadline ends it.
func TestReviewStalledHandshakeIsBounded(t *testing.T) {
	server, accepted := startSilentServer(t)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()

	start := time.Now()
	_, _, _, err := pool.GetClientContext(context.Background(), server, cfg, nil, DialOptions{
		HostKeyPolicy: HostKeyPolicyAcceptNew,
		Timeout:       300 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a server that never speaks SSH produced a connection")
	}
	if accepted.Load() == 0 {
		t.Fatal("the attempt never reached the stalled server")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the stalled handshake was bounded only after %s", elapsed)
	}
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool holds %d entries after a failed handshake, want none", got)
	}
}

// C06: cancelling the caller ends its own attempt instead of waiting out the
// connection phase deadline.
func TestReviewCallerCancellationEndsStalledHandshake(t *testing.T) {
	server, _ := startSilentServer(t)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, _, err := pool.GetClientContext(ctx, server, cfg, nil, DialOptions{
		HostKeyPolicy: HostKeyPolicyAcceptNew,
		Timeout:       30 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled attempt reported success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s to end the attempt", elapsed)
	}
	// Nothing may be left behind: the caller was the only waiter, so the creation
	// it abandoned has to be cleaned up.
	waitFor(t, 2*time.Second, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.inflight) == 0
	})
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool holds %d entries after a cancelled attempt, want none", got)
	}
}

// C07: the last waiter to leave ends the creation nobody is waiting for.
func TestReviewLastWaiterLeavingCleansUpCreation(t *testing.T) {
	server, _ := startSilentServer(t)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, ctx := range []context.Context{ctxA, ctxB} {
		wg.Add(1)
		go func(ctx context.Context) {
			defer wg.Done()
			_, _, _, err := pool.GetClientContext(ctx, server, cfg, nil, DialOptions{
				HostKeyPolicy: HostKeyPolicyAcceptNew,
				Timeout:       30 * time.Second,
			})
			errs <- err
		}(ctx)
	}

	// Let both callers join the same in-flight creation before either gives up.
	waitFor(t, 2*time.Second, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		for _, inflight := range pool.inflight {
			return inflight.waiters == 2
		}
		return false
	})

	cancelA()
	cancelB()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("a cancelled waiter reported success")
		}
	}

	waitFor(t, 5*time.Second, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.inflight) == 0
	})
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool holds %d entries, want none: the abandoned creation was published", got)
	}
}

// C07: two interactive attempts against the same server must not share a
// creation. Each session sees its own host key prompt, cancelling one leaves the
// other alone, and the connection that succeeds is still reusable afterwards.
func TestReviewInteractiveCreationsAreIsolated(t *testing.T) {
	srv := sshserver.New(t, sshserver.Config{User: "testuser", Password: "secret"})
	defer srv.Close()

	// The two attempts deliberately use the same password, so only the isolation
	// of the creations keeps their prompts apart.
	server := realServerProfile(t, srv, "secret")
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	var mu sync.Mutex
	promptA, promptB := 0, 0
	aPrompted := make(chan struct{})
	var aOnce sync.Once

	confirmA := func(prompt HostKeyPrompt) bool {
		mu.Lock()
		promptA++
		mu.Unlock()
		aOnce.Do(func() { close(aPrompted) })
		// Cancelling the session must release its own prompt, the way the real
		// session service does when it stops waiting for an answer.
		<-ctxA.Done()
		return false
	}
	// Both prompts reject, so the shared known_hosts file stays empty and each
	// attempt really has to ask. A session is not shown a prompt its owner did
	// not ask for.
	confirmB := func(prompt HostKeyPrompt) bool {
		mu.Lock()
		promptB++
		mu.Unlock()
		return false
	}

	type outcome struct {
		client *ssh.Client
		key    string
		err    error
	}
	results := make(chan outcome, 2)
	go func() {
		client, keys, _, err := pool.GetClientContext(ctxA, server, cfg, confirmA, DialOptions{
			HostKeyPolicy: HostKeyPolicyAsk,
			Timeout:       15 * time.Second,
		})
		var key string
		if len(keys) > 0 {
			key = keys[0]
		}
		results <- outcome{client: client, key: key, err: err}
	}()
	go func() {
		client, keys, _, err := pool.GetClientContext(context.Background(), server, cfg, confirmB, DialOptions{
			HostKeyPolicy: HostKeyPolicyAsk,
			Timeout:       15 * time.Second,
		})
		var key string
		if len(keys) > 0 {
			key = keys[0]
		}
		results <- outcome{client: client, key: key, err: err}
	}()

	// B must finish on its own answer even while A is still undecided. If the two
	// creations were merged, B would be waiting for A's decision here.
	var first outcome
	select {
	case first = <-results:
	case <-time.After(10 * time.Second):
		mu.Lock()
		a, b := promptA, promptB
		mu.Unlock()
		t.Fatalf("no attempt finished; prompts seen: A=%d B=%d", a, b)
	}

	// A must have reached its own prompt while B was completing: the two
	// creations are genuinely separate and neither is waiting on the other.
	select {
	case <-aPrompted:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled session never saw a prompt of its own")
	}

	cancelA()
	second := <-results
	if second.err == nil {
		t.Fatal("the cancelled attempt reported success")
	}
	mu.Lock()
	a, b := promptA, promptB
	mu.Unlock()
	if a != 1 || b != 1 {
		t.Fatalf("each session must answer its own prompt: A=%d B=%d", a, b)
	}

	// Isolation also means neither attempt failed because of the other: the one
	// that answered for itself failed on its own rejection, not on A's
	// cancellation, and A failed on its cancellation, not on B's answer.
	for _, res := range []outcome{first, second} {
		if !errors.Is(res.err, ErrHostKeyReject) && !errors.Is(res.err, context.Canceled) {
			t.Fatalf("unexpected failure from an isolated attempt: %v", res.err)
		}
	}

	// A connection that succeeds afterwards is still pooled and reusable, so the
	// isolation of interactive creations did not cost the sharing of results.
	accepted := func(prompt HostKeyPrompt) bool { return true }
	client, keys, created, err := pool.GetClientContext(context.Background(), server, cfg, accepted, DialOptions{
		HostKeyPolicy: HostKeyPolicyAsk,
		Timeout:       15 * time.Second,
	})
	if err != nil {
		t.Fatalf("successful attempt after the isolated ones: %v", err)
	}
	if !created {
		t.Fatal("the first successful attempt did not create a connection")
	}

	reused, reuseKeys, created, err := pool.GetClientContext(context.Background(), server, cfg, nil, DialOptions{
		HostKeyPolicy: HostKeyPolicyAsk,
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("reuse after isolation failure: %v", err)
	}
	if created {
		t.Fatal("the successful connection was not reusable")
	}
	if reused != client {
		t.Fatal("reuse returned a different client than the one that succeeded")
	}
	if len(reuseKeys) != 1 || reuseKeys[0] != keys[0] {
		t.Fatalf("reuse key = %v, want %q", reuseKeys, keys)
	}
}

// C08: the time a client spends deciding about a host key is not charged against
// the connection phase deadline, and the connection that results does not carry
// that deadline into its working life.
func TestReviewHostKeyPromptOutlivesShortNetworkDeadline(t *testing.T) {
	srv := sshserver.New(t, sshserver.Config{User: "testuser", Password: "secret"})
	defer srv.Close()

	server := realServerProfile(t, srv, "secret")
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()

	const timeout = 300 * time.Millisecond
	const decision = 1500 * time.Millisecond

	prompted := make(chan HostKeyPrompt, 1)
	confirm := func(prompt HostKeyPrompt) bool {
		prompted <- prompt
		time.Sleep(decision)
		return true
	}

	start := time.Now()
	client, _, _, err := pool.GetClientContext(context.Background(), server, cfg, confirm, DialOptions{
		HostKeyPolicy: HostKeyPolicyAsk,
		Timeout:       timeout,
	})
	if err != nil {
		t.Fatalf("a client decision longer than the network deadline broke the connection: %v", err)
	}
	if elapsed := time.Since(start); elapsed < decision {
		t.Fatalf("the prompt returned after %s, want at least the %s the client took", elapsed, decision)
	}
	select {
	case prompt := <-prompted:
		if prompt.Fingerprint == "" {
			t.Fatal("the prompt carried no fingerprint")
		}
	default:
		t.Fatal("the host key prompt was never shown")
	}

	// The handshake deadline must be gone: a connection that still carried it
	// would die as soon as it expired.
	time.Sleep(2 * timeout)
	if !pool.IsAlive(connKey(server), client) {
		t.Fatal("the connection inherited the handshake deadline")
	}
	if _, err := client.NewSession(); err != nil {
		t.Fatalf("the pooled connection is unusable after the handshake: %v", err)
	}
}

// C09: a change of credentials must not be served by the connection that was
// authenticated with the old ones, while a session still using that connection
// keeps it.
func TestReviewIdentityChangeDoesNotReuseStaleClient(t *testing.T) {
	srv := sshserver.New(t, sshserver.Config{User: "testuser", Password: "secret"})
	defer srv.Close()

	server := realServerProfile(t, srv, "secret")
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	defer pool.CloseAll()

	opts := DialOptions{HostKeyPolicy: HostKeyPolicyInsecureSkip, Timeout: 5 * time.Second}
	first, firstKeys, created, err := pool.GetClientContext(context.Background(), server, cfg, nil, opts)
	if err != nil {
		t.Fatalf("first connection: %v", err)
	}
	if !created {
		t.Fatal("the first attempt did not create a connection")
	}
	// A session is using it, so it must survive the credential change.
	pool.IncRef(firstKeys...)
	defer pool.DecRef(firstKeys...)
	originalKey := firstKeys[0]

	// The password changes: the cached connection was authenticated with the old
	// one and must not answer for the new identity.
	changed := server
	changed.Password = "replaced"
	changedCfg := testRuntimeConfig(changed)
	if _, _, _, err := pool.GetClientContext(context.Background(), changed, changedCfg, nil, opts); err == nil {
		t.Fatal("the stale connection answered for changed credentials")
	}

	// The live session still holds its connection.
	if !pool.IsAlive(originalKey, first) {
		t.Fatal("the credential change closed a connection a session was using")
	}
	if _, err := first.NewSession(); err != nil {
		t.Fatalf("the retained connection is unusable: %v", err)
	}

	// Another key or another proxy body is a different identity too, even though
	// the non-secret pool key does not change.
	withKey := server
	withKey.AuthMethod = config.AuthMethodKey
	withKey.KeyID = "missing-key"
	keyCfg := testRuntimeConfig(withKey)
	if _, _, _, err := pool.GetClientContext(context.Background(), withKey, keyCfg, nil, opts); err == nil {
		t.Fatal("a key-auth attempt was served by the password connection")
	}

	// Reverting to the original material reuses the connection that the live
	// session is holding, rather than dialing again.
	again, keys, created, err := pool.GetClientContext(context.Background(), server, cfg, nil, opts)
	if err != nil {
		t.Fatalf("reuse with the original credentials: %v", err)
	}
	if created {
		t.Fatal("the original credentials dialed again instead of reusing")
	}
	if again != first || keys[0] != originalKey {
		t.Fatalf("reuse returned %v/%v, want the original connection", keys, again)
	}
}

// C09: changing the body of a proxy under the same ProxyID must invalidate the
// cached connection built through the old proxy.
func TestReviewProxyChangeInvalidatesCachedClient(t *testing.T) {
	server := config.ServerProfile{
		ID:             "proxied",
		Alias:          "proxied",
		Host:           "127.0.0.1",
		Port:           22,
		User:           "tester",
		Password:       "secret",
		AuthMethod:     config.AuthMethodPassword,
		ProxyID:        "corp",
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
	}
	cfg := testRuntimeConfig(server)
	cfg.Proxies["corp"] = config.ProxyProfile{
		ID: "corp", Type: config.ProxyTypeSOCKS5, Host: "127.0.0.1", Port: 1080,
		Username: "user", Password: "first",
	}

	base := clientIdentity(server, cfg, DialOptions{HostKeyPolicy: HostKeyPolicyInsecureSkip})
	if len(base) == 0 {
		t.Fatal("the identity digest is empty")
	}

	moved := cfg
	moved.Proxies = map[string]config.ProxyProfile{
		"corp": {ID: "corp", Type: config.ProxyTypeSOCKS5, Host: "127.0.0.1", Port: 1081,
			Username: "user", Password: "first"},
	}
	if got := clientIdentity(server, moved, DialOptions{HostKeyPolicy: HostKeyPolicyInsecureSkip}); string(got) == string(base) {
		t.Fatal("a different proxy host kept the same identity")
	}

	rotated := cfg
	rotated.Proxies = map[string]config.ProxyProfile{
		"corp": {ID: "corp", Type: config.ProxyTypeSOCKS5, Host: "127.0.0.1", Port: 1080,
			Username: "user", Password: "second"},
	}
	if got := clientIdentity(server, rotated, DialOptions{HostKeyPolicy: HostKeyPolicyInsecureSkip}); string(got) == string(base) {
		t.Fatal("a rotated proxy credential kept the same identity")
	}

	// The digest must not carry the secret itself: it is compared in memory and
	// must never be usable as a pool key or in stats.
	digest := string(base)
	if strings.Contains(digest, "first") || strings.Contains(digest, "secret") {
		t.Fatal("the identity digest contains plaintext material")
	}
}

// C06: a pool that closes while an attempt is in flight ends that attempt
// instead of leaving its dial and handshake running.
func TestReviewPoolCloseEndsInFlightAttempt(t *testing.T) {
	server, _ := startSilentServer(t)
	cfg := testRuntimeConfig(server)
	pool := NewPool()
	t.Cleanup(func() { pool.CloseAll() })

	done := make(chan error, 1)
	go func() {
		_, _, _, err := pool.GetClientContext(context.Background(), server, cfg, nil, DialOptions{
			HostKeyPolicy: HostKeyPolicyAcceptNew,
			Timeout:       30 * time.Second,
		})
		done <- err
	}()

	// Allow scheduler headroom during parallel full-suite runs. This observes
	// setup only; the shutdown completion budget below remains five seconds.
	waitFor(t, 10*time.Second, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.inflight) > 0
	})
	pool.CloseAll()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an attempt in flight during shutdown reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown left the in-flight attempt running")
	}
	waitFor(t, 5*time.Second, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.inflight) == 0
	})
}
