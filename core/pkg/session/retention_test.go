package session

import (
	"context"
	"errors"
	"fmt"
	"knot-core/internal/resourcepolicy"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func retentionSSH(t *testing.T) *Service {
	t.Helper()
	s := NewService()
	s.UseLocalTestBackend()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func settleSSH(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.workers.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestTerminalRetentionDoesNotBlockCreation(t *testing.T) {
	s := retentionSSH(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.policy = resourcepolicy.Policy{Active: 2, History: 2, TTL: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	active := createControlTestSession(t, s, "active", 24, 80)
	ids := []string{}
	for i := 0; i < 3; i++ {
		res := createControlTestSession(t, s, fmt.Sprint(i), 24, 80)
		ids = append(ids, res.ID)
		if _, err := s.Disconnect(res.ID); err != nil {
			t.Fatal(err)
		}
		settleSSH(t, s)
		clock.Add(int64(time.Second))
	}
	if _, err := s.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest history: %v", err)
	}
	final, err := s.Get(ids[2])
	if err != nil || final.ExitedAt == nil {
		t.Fatalf("retained result: %+v %v", final, err)
	}
	clock.Add(int64(time.Minute))
	if _, err := s.Get(ids[2]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired history: %v", err)
	}
	if res, err := s.Get(active.ID); err != nil || isTerminalState(res.State) {
		t.Fatalf("active pruned: %+v %v", res, err)
	}
	second := createControlTestSession(t, s, "second", 24, 80)
	if _, err := s.Create(CreateRequest{ServerRef: "test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("active capacity: %v", err)
	}
	if _, err := s.Disconnect(second.ID); err != nil {
		t.Fatal(err)
	}
	createControlTestSession(t, s, "replacement", 24, 80)
}
func TestSessionCreationBeyondLifetimeLimit(t *testing.T) {
	s := retentionSSH(t)
	for i := 0; i < s.policy.Active+32; i++ {
		res := createControlTestSession(t, s, "repeated", 24, 80)
		if _, err := s.Disconnect(res.ID); err != nil {
			t.Fatal(err)
		}
		settleSSH(t, s)
	}
	if len(s.List()) > s.policy.History || s.ActiveCount() != 0 {
		t.Fatal("history or active resources grew beyond policy")
	}
	createControlTestSession(t, s, "next", 24, 80)
}
func TestTerminalHistoryWaitsForBackendRelease(t *testing.T) {
	previous := teardownGrace
	teardownGrace = 10 * time.Millisecond
	t.Cleanup(func() { teardownGrace = previous })
	s := gatedService(t, nil)
	s.policy.Active = 1
	gateRes := createControlTestSession(t, s, "gate", 24, 80)
	gate := gatedStdin(t, s, gateRes.ID)
	t.Cleanup(gate.unblock)
	closed := make(chan error, 1)
	go func() { _, err := s.Disconnect(gateRes.ID); closed <- err }()
	gate.waitStarted(t, "close")
	if _, err := s.Create(CreateRequest{ServerRef: "test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending cleanup was unbounded: %v", err)
	}
	var clock atomic.Int64
	clock.Store(time.Now().Add(time.Hour).UnixNano())
	s.policy.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	if _, err := s.Get(gateRes.ID); err != nil {
		t.Fatalf("unfinished backend pruned: %v", err)
	}
	if err := <-closed; !errors.Is(err, ErrTeardownTimeout) {
		t.Fatalf("close must report pending work: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown hid pending backend: %v", err)
	}
	gate.unblock()
	settleSSH(t, s)
	if _, err := s.Get(gateRes.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settled expired backend retained: %v", err)
	}
	if gate.closeCalls() != 1 {
		t.Fatalf("close calls=%d", gate.closeCalls())
	}
}
func TestSessionSubscriberLimitsAndIdempotentCancel(t *testing.T) {
	s := retentionSSH(t)
	res := createControlTestSession(t, s, "subscribers", 24, 80)
	var cancels []func()
	var events []<-chan Event
	for i := 0; i < maxSessionSubscribers; i++ {
		ch, cancel, _, err := s.Subscribe(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ch)
		cancels = append(cancels, cancel)
	}
	if _, _, _, err := s.Subscribe(res.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("subscriber limit: %v", err)
	}
	var cwd []<-chan CWDNotify
	for i := 0; i < maxSessionCWDFollowers; i++ {
		ch, cancel, _, err := s.SubscribeCWD(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		cwd = append(cwd, ch)
		cancels = append(cancels, cancel)
	}
	if _, _, _, err := s.SubscribeCWD(res.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("follower limit: %v", err)
	}
	for i := 0; i < 100; i++ {
		s.updateCurrentDir(res.ID, fmt.Sprintf("/dir/%d", i))
	}
	cancels[0]()
	cancels[0]()
	_, replacement, _, err := s.Subscribe(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancels = append(cancels, replacement)
	if _, err := s.Disconnect(res.ID); err != nil {
		t.Fatal(err)
	}
	for _, cancel := range cancels {
		cancel()
		cancel()
	}
	for _, ch := range events {
		for range ch {
		}
	}
	for _, ch := range cwd {
		for range ch {
		}
	}
	s.mu.RLock()
	count := len(s.sessions[res.ID].subscribers) + len(s.sessions[res.ID].cwdSubscribers)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatalf("subscribers=%d", count)
	}
	ch, cancel, _, err := s.Subscribe(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("terminal subscription remains open")
	}
}
func TestSessionCallbackReentryAndPanic(t *testing.T) {
	s := retentionSSH(t)
	done := make(chan struct{})
	var once sync.Once
	s.OnEvent(func(event Event) {
		if event.Type == "session.created" {
			_, _ = s.Get(event.SessionID)
			panic("observer panic")
		}
		if event.Type == "session.connected" {
			once.Do(func() {
				if err := s.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
				close(done)
			})
		}
	})
	createControlTestSession(t, s, "callback", 24, 80)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("callback could not reenter shutdown")
	}
	if s.workers.Count() != 0 || s.ActiveCount() != 0 {
		t.Fatal("callback shutdown did not release resources")
	}
}
func TestSessionConcurrentCloseOwnership(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	s, id, _ := newSSHService(t, srv)
	for i := 0; i < 50; i++ {
		beforeExit := make(chan struct{})
		srv.SetShellBehavior(sshserver.ShellBehavior{Scripted: true, BeforeExit: beforeExit, SendExitStatus: true, ExitCode: 7})
		res := createSession(t, s, id, CreateRequest{})
		waitForState(t, s, res.ID, 5*time.Second, "connected")
		s.mu.RLock()
		key := s.sessions[res.ID].poolKeys[0]
		s.mu.RUnlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, fn := range []func(){func() { _, _ = s.Disconnect(res.ID) }, func() { _, _ = s.Disconnect(res.ID) }, func() {
			if i%2 == 0 {
				s.DisconnectByPoolKey(key)
			} else {
				s.pool.Clear()
			}
		}, func() { close(beforeExit) }} {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; fn() }()
		}
		close(start)
		wg.Wait()
		settleSSH(t, s)
		for _, stat := range s.pool.Stats() {
			if stat.RefCount != 0 {
				t.Fatalf("iteration %d ref=%d", i, stat.RefCount)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s.workers.Count() != 0 {
		t.Fatal("worker leak")
	}
}

func TestSessionMaintenancePrunesWithoutRequests(t *testing.T) {
	s := retentionSSH(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.policy.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	s.policy.SweepEvery = time.Millisecond
	res := createControlTestSession(t, s, "sweep", 24, 80)
	_, _ = s.Disconnect(res.ID)
	settleSSH(t, s)
	clock.Add(int64(time.Hour))
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartMaintenance(life)
	s.StartMaintenance(life)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		s.mu.RLock()
		count := len(s.sessions)
		s.mu.RUnlock()
		if count == 0 {
			break
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("cleaner did not prune idle history")
		}
	}
	cancel()
	settleSSH(t, s)
	if s.workers.Count() != 0 {
		t.Fatal("cleaner did not finish")
	}
}
func TestCanceledChannelOpenRetainsCleanupOwner(t *testing.T) {
	client, conn := stalledExecClient(t)
	conn.openStarted = make(chan struct{})
	conn.openGate = make(chan struct{})
	var gateOnce sync.Once
	unblock := func() { gateOnce.Do(func() { close(conn.openGate) }) }
	defer unblock()
	s := retentionSSH(t)
	res := &resource{Resource: Resource{ID: "pending", State: "failed"}, subscribers: map[chan Event]struct{}{}, cwdSubscribers: map[chan CWDNotify]struct{}{}}
	expired := time.Now().Add(-time.Hour)
	res.ExitedAt = &expired
	s.sessions[res.ID] = res
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = resourcepolicy.WithWork(ctx, func() func() { s.mu.Lock(); defer s.mu.Unlock(); return s.beginWorkLocked(res) })
	caller := make(chan error, 1)
	go func() { _, err := openSSHSession(ctx, client); caller <- err }()
	awaitExec(t, conn.openStarted)
	cancel()
	if err := <-caller; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Get(res.ID); err != nil {
		t.Fatalf("late channel owner pruned: %v", err)
	}
	budget, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if err := s.Shutdown(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late channel owner hidden: %v", err)
	}
	// Transport loss resolves an outstanding protocol open. The controlled fake
	// opens only after its explicit gate; the late channel is then closed locally.
	unblock()
	conn.ch.release()
	settleSSH(t, s)
	if _, err := s.Get(res.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settled late channel retained: %v", err)
	}
}

func TestSessionFailureClosesSubscribersAndContext(t *testing.T) {
	for _, failure := range []string{"connect", "host_key", "auth"} {
		t.Run(failure, func(t *testing.T) {
			s := NewService()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := &resource{Resource: Resource{ID: "pending", State: "connecting"}, cancel: cancel, subscribers: map[chan Event]struct{}{}, cwdSubscribers: map[chan CWDNotify]struct{}{}}
			s.sessions[res.ID] = res
			events, cancelEvents, _, err := s.Subscribe(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			cwd, cancelCWD, _, err := s.SubscribeCWD(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "connect" {
				s.failSession(res.ID, errors.New("connection failed"), config.RuntimeConfig{}, "failed")
			} else {
				s.failPendingChallenge(res.ID, failure, "challenge failed")
			}
			select {
			case <-ctx.Done():
			default:
				t.Fatal("terminal failure retained context")
			}
			for range events {
			}
			for range cwd {
			}
			cancelEvents()
			cancelEvents()
			cancelCWD()
			cancelCWD()
			if len(res.subscribers)+len(res.cwdSubscribers) != 0 {
				t.Fatal("terminal failure retained subscribers")
			}
		})
	}
}

func TestSessionCloseKeepsSharedReference(t *testing.T) {
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	s, id, _ := newSSHService(t, srv)
	guard := createSession(t, s, id, CreateRequest{})
	waitForState(t, s, guard.ID, 5*time.Second, "connected")
	exit := make(chan struct{})
	srv.SetShellBehavior(sshserver.ShellBehavior{Scripted: true, BeforeExit: exit, SendExitStatus: true})
	target := createSession(t, s, id, CreateRequest{})
	waitForState(t, s, target.ID, 5*time.Second, "connected")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, fn := range []func(){func() { _, _ = s.Disconnect(target.ID) }, func() { _, _ = s.Disconnect(target.ID) }, func() { close(exit) }} {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; fn() }()
	}
	close(start)
	wg.Wait()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		s.mu.RLock()
		pending := s.sessions[target.ID].workers
		s.mu.RUnlock()
		if pending == 0 {
			break
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("target worker did not settle")
		}
	}
	stats := s.pool.Stats()
	if len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatalf("shared owner lost reference: %+v", stats)
	}
	if _, err := s.Control(guard.ID, ControlRequest{Type: "resize", Rows: 30, Cols: 90}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Disconnect(guard.ID)
	settleSSH(t, s)
}

func TestSessionTeardownTimeoutRetainsPoolOwner(t *testing.T) {
	previous := teardownGrace
	teardownGrace = 10 * time.Millisecond
	defer func() { teardownGrace = previous }()
	srv := startSSHServer(t, sshserver.ShellBehavior{})
	s, id, _ := newSSHService(t, srv)
	res := createSession(t, s, id, CreateRequest{})
	waitForState(t, s, res.ID, 5*time.Second, "connected")
	gate := newGatedStdio(nil)
	defer gate.unblock()
	s.mu.Lock()
	backend := s.sessions[res.ID].backend
	backend.stdin = gate
	s.mu.Unlock()
	if _, err := s.Disconnect(res.ID); !errors.Is(err, ErrTeardownTimeout) {
		t.Fatalf("close must report pending release: %v", err)
	}
	gate.waitStarted(t, "backend close")
	if stats := s.pool.Stats(); len(stats) != 1 || stats[0].RefCount != 1 {
		t.Fatalf("bounded response returned actual cleanup reference: %+v", stats)
	}
	budget, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished release reported success: %v", err)
	}
	gate.unblock()
	settleSSH(t, s)
	if stats := s.pool.Stats(); len(stats) != 1 || stats[0].RefCount != 0 {
		t.Fatalf("settled backend retained reference: %+v", stats)
	}
}

func TestWatchInteractiveWithoutLease(t *testing.T) {
	client, conn := stalledExecClient(t)
	sshSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sshSession.Shell(); err != nil {
		t.Fatal(err)
	}
	pump := newOutputPump()
	pump.Start()
	backend := &interactiveBackend{session: sshSession, pump: pump, exitResult: newExitResult()}
	s := retentionSSH(t)
	id := "unpooled"
	s.sessions[id] = &resource{Resource: Resource{ID: id, State: "connected"}, backend: backend}
	conn.ch.release()
	// A valid SSH session may be supplied by an unpooled hook. Its lifecycle and
	// terminal diagnostics still settle without requiring a pool lease.
	s.watchInteractive(id, backend, config.RuntimeConfig{})
	settleSSH(t, s)
	result, err := s.Get(id)
	if err != nil || result.State != "failed" || result.DisconnectCause != causeExitStatusMissing {
		t.Fatalf("unpooled backend lost terminal diagnostics: %+v %v", result, err)
	}
	select {
	case <-backend.completion():
	default:
		t.Fatal("unpooled backend did not finish cleanup")
	}
}
