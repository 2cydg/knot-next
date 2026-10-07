package sftp

import (
	"context"
	"errors"
	"fmt"
	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/resourcepolicy"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func retentionSFTP(t *testing.T) *Service {
	t.Helper()
	s := NewService(t.TempDir())
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
func createRetentionSFTP(t *testing.T, s *Service) Session {
	t.Helper()
	res, err := s.Create(CreateRequest{ServerRef: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func settleSFTP(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.workers.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestTerminalRetentionDoesNotBlockCreation(t *testing.T) {
	s := retentionSFTP(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.policy = resourcepolicy.Policy{Active: 2, History: 2, TTL: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	active := createRetentionSFTP(t, s)
	var ids []string
	for i := 0; i < 3; i++ {
		res := createRetentionSFTP(t, s)
		ids = append(ids, res.ID)
		if _, err := s.Close(res.ID); err != nil {
			t.Fatal(err)
		}
		settleSFTP(t, s)
		clock.Add(int64(time.Second))
	}
	if _, err := s.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest: %v", err)
	}
	final, err := s.Get(ids[2])
	if err != nil || final.ClosedAt == nil {
		t.Fatalf("retained: %+v %v", final, err)
	}
	clock.Add(int64(time.Minute))
	if _, err := s.Get(ids[2]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := s.Get(active.ID); err != nil {
		t.Fatal("active pruned")
	}
	second := createRetentionSFTP(t, s)
	if _, err := s.Create(CreateRequest{ServerRef: "test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("active limit: %v", err)
	}
	if _, err := s.Close(second.ID); err != nil {
		t.Fatal(err)
	}
	createRetentionSFTP(t, s)
}
func TestSFTPCreationBeyondLifetimeLimit(t *testing.T) {
	s := retentionSFTP(t)
	for i := 0; i < s.policy.Active+32; i++ {
		res := createRetentionSFTP(t, s)
		if _, err := s.Close(res.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.ListSessions()) > s.policy.History || s.SessionCount() != 0 {
		t.Fatal("unbounded history")
	}
	createRetentionSFTP(t, s)
}
func TestTransferRetentionWaitsForWorker(t *testing.T) {
	s := retentionSFTP(t)
	s.policy.Active = 1
	res := createRetentionSFTP(t, s)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.transferPolicy = resourcepolicy.Policy{Active: 1, History: 1, TTL: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	s.policy.Now = s.transferPolicy.Now
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	tr, err := s.startTransfer(res.ID, "upload", "s", "t", nil, func(ctx context.Context, _ *backendView, _ *transferWork) (string, error) {
		close(entered)
		<-release
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitTransferSignal(t, entered)
	if _, err := s.startTransfer(res.ID, "upload", "s", "t", nil, func(context.Context, *backendView, *transferWork) (string, error) {
		t.Error("capacity admitted worker")
		return "", nil
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("transfer capacity: %v", err)
	}
	if _, err := s.Close(res.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(CreateRequest{ServerRef: "test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending cleanup was unbounded: %v", err)
	}
	clock.Add(int64(time.Hour))
	if _, err := s.Get(res.ID); err != nil {
		t.Fatalf("session with running worker pruned: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending worker hidden: %v", err)
	}
	unblock()
	settleSFTP(t, s)
	result, err := s.Transfer(res.ID, tr.ID)
	if err != nil || result.State != "canceled" {
		t.Fatalf("retained transfer: %+v %v", result, err)
	}
	clock.Add(int64(time.Hour))
	if _, err := s.Transfer(res.ID, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired transfer: %v", err)
	}
}
func TestSFTPSubscriberLimitsAndIdempotentCancel(t *testing.T) {
	s := retentionSFTP(t)
	res := createRetentionSFTP(t, s)
	var cancels []func()
	var events []<-chan Event
	var transfers []<-chan TransferEvent
	for i := 0; i < maxSessionSubs; i++ {
		ch, cancel, _, err := s.Subscribe(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ch)
		cancels = append(cancels, cancel)
	}
	if _, _, _, err := s.Subscribe(res.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("session subscriber limit: %v", err)
	}
	for i := 0; i < maxTransferSubs; i++ {
		ch, cancel, err := s.SubscribeTransfers(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		transfers = append(transfers, ch)
		cancels = append(cancels, cancel)
	}
	if _, _, err := s.SubscribeTransfers(res.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("transfer subscriber limit: %v", err)
	}
	s.mu.Lock()
	for i := 0; i < 100; i++ {
		s.publishSessionLocked(s.sessions[res.ID], Event{Type: fmt.Sprint(i)})
	}
	s.mu.Unlock()
	cancels[0]()
	cancels[0]()
	_, cancel, _, err := s.Subscribe(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancels = append(cancels, cancel)
	if _, err := s.Close(res.ID); err != nil {
		t.Fatal(err)
	}
	for _, fn := range cancels {
		fn()
		fn()
	}
	for _, ch := range events {
		for range ch {
		}
	}
	for _, ch := range transfers {
		for range ch {
		}
	}
	if len(s.subs)+len(s.transferSubs) != 0 {
		t.Fatal("subscriber maps leaked")
	}
}
func TestSFTPFollowCloseReleasesOwner(t *testing.T) {
	source := session.NewService()
	source.UseLocalTestBackend()
	defer source.Shutdown(context.Background())
	s := retentionSFTP(t)
	s.UseSession(source)
	for i := 0; i < 50; i++ {
		ssh, err := source.Create(session.CreateRequest{ServerRef: "test"})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Create(CreateRequest{ServerRef: "test", FollowSessionID: ssh.ID})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.Close(res.ID) }()
		go func() { defer wg.Done(); _, _ = source.Disconnect(ssh.ID) }()
		wg.Wait()
		settleSFTP(t, s)
		s.mu.RLock()
		owner := s.sessions[res.ID]
		pending := owner.workers
		follow := owner.followCancel
		s.mu.RUnlock()
		if pending != 0 || follow != nil {
			t.Fatalf("follower leaked: %d", pending)
		}
	}
}
func TestSFTPCallbackReentryAndPanic(t *testing.T) {
	s := retentionSFTP(t)
	done := make(chan struct{})
	var once sync.Once
	s.OnEvent(func(ev Event) {
		if ev.Type == "sftp.session.created" {
			_, _ = s.Get(ev.SessionID)
			panic("observer panic")
		}
		if ev.Type == "sftp.session.opened" {
			once.Do(func() {
				if err := s.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
				close(done)
			})
		}
	})
	createRetentionSFTP(t, s)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("callback shutdown blocked")
	}
	if s.workers.Count() != 0 {
		t.Fatal("worker leak")
	}
}

func TestTransferHistoryCapacityAndActiveProtection(t *testing.T) {
	s := retentionSFTP(t)
	res := createRetentionSFTP(t, s)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.policy.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	s.transferPolicy = resourcepolicy.Policy{Active: 2, History: 2, TTL: time.Minute, Now: s.policy.Now}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	active, err := s.startTransfer(res.ID, "upload", "s", "t", nil, func(context.Context, *backendView, *transferWork) (string, error) {
		close(started)
		<-release
		return "completed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitTransferSignal(t, started)
	var ids []string
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		tr, err := s.startTransfer(res.ID, "upload", "s", "t", nil, func(context.Context, *backendView, *transferWork) (string, error) {
			defer close(done)
			return "completed", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tr.ID)
		awaitTransferSignal(t, done)
		deadline := time.NewTimer(time.Second)
		poll := time.NewTicker(time.Millisecond)
		for {
			s.mu.RLock()
			busy := s.transfers[tr.ID].active
			s.mu.RUnlock()
			if !busy {
				break
			}
			select {
			case <-poll.C:
			case <-deadline.C:
				t.Fatal("worker did not settle")
			}
		}
		deadline.Stop()
		poll.Stop()
		clock.Add(int64(time.Second))
	}
	if _, err := s.Transfer(res.ID, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest transfer retained: %v", err)
	}
	if tr, err := s.Transfer(res.ID, ids[2]); err != nil || tr.State != "completed" {
		t.Fatalf("latest transfer lost: %+v %v", tr, err)
	}
	clock.Add(int64(time.Hour))
	if _, err := s.Transfer(res.ID, ids[2]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired transfer: %v", err)
	}
	if tr, err := s.Transfer(res.ID, active.ID); err != nil || tr.State != "running" {
		t.Fatalf("active transfer pruned: %+v %v", tr, err)
	}
}
func TestSFTPMaintenancePrunesWithoutRequests(t *testing.T) {
	s := retentionSFTP(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.policy.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	s.policy.SweepEvery = time.Millisecond
	res := createRetentionSFTP(t, s)
	_, _ = s.Close(res.ID)
	settleSFTP(t, s)
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
	settleSFTP(t, s)
	if s.workers.Count() != 0 {
		t.Fatal("cleaner did not finish")
	}
}

func TestSFTPConcurrentCloseOwnership(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	srv := sshserver.New(t, sshserver.Config{User: testAuthUser, Password: testAuthPassword, SFTPHandlers: &handlers})
	s, id, _ := newSFTPAuthService(t, srv, testAuthPassword)
	for i := 0; i < 50; i++ {
		res, err := s.Create(CreateRequest{ServerRef: id, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
		if err != nil {
			t.Fatal(err)
		}
		waitForSFTPState(t, s, res.ID, 5*time.Second, "open")
		s.mu.RLock()
		key := s.sessions[res.ID].poolKeys[0]
		s.mu.RUnlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, fn := range []func(){func() { _, _ = s.Close(res.ID) }, func() { _, _ = s.Close(res.ID) }, func() { s.CloseByPoolKey(key) }, func() { s.pool.Clear() }} {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; fn() }()
		}
		close(start)
		wg.Wait()
		settleSFTP(t, s)
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
func TestSFTPTransportDropNeedsNoCallback(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	srv := sshserver.New(t, sshserver.Config{User: testAuthUser, Password: testAuthPassword, SFTPHandlers: &handlers})
	s, id, _ := newSFTPAuthService(t, srv, testAuthPassword)
	res, err := s.Create(CreateRequest{ServerRef: id, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil {
		t.Fatal(err)
	}
	waitForSFTPState(t, s, res.ID, 5*time.Second, "open")
	s.pool.Clear()
	waitForSFTPState(t, s, res.ID, 5*time.Second, "disconnected")
	settleSFTP(t, s)
	if len(s.subs)+len(s.transferSubs) != 0 {
		t.Fatal("transport loss left subscriptions")
	}
}

func TestSFTPFailureReleasesSubscribersAndFollower(t *testing.T) {
	for _, failure := range []string{"connect", "host_key", "auth"} {
		t.Run(failure, func(t *testing.T) {
			s := retentionSFTP(t)
			opened := createRetentionSFTP(t, s)
			s.mu.Lock()
			res := s.sessions[opened.ID]
			s.mu.Unlock()
			events, cancelEvents, _, err := s.Subscribe(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			transfers, cancelTransfers, err := s.SubscribeTransfers(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			followDone := make(chan struct{})
			res.followCancel = func() { _, _ = s.Get(res.ID); close(followDone) }
			if failure == "connect" {
				s.failSession(res.ID, errors.New("connection failed"), config.RuntimeConfig{})
			} else {
				s.failPendingChallenge(res.ID, failure, "challenge failed")
			}
			settleSFTP(t, s)
			awaitTransferSignal(t, followDone)
			for range events {
			}
			for range transfers {
			}
			cancelEvents()
			cancelEvents()
			cancelTransfers()
			cancelTransfers()
			if len(s.subs)+len(s.transferSubs) != 0 || res.followCancel != nil {
				t.Fatal("failure retained subscriptions")
			}
		})
	}
}

func TestSFTPCloseKeepsSharedReference(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	srv := sshserver.New(t, sshserver.Config{User: testAuthUser, Password: testAuthPassword, SFTPHandlers: &handlers})
	s, id, _ := newSFTPAuthService(t, srv, testAuthPassword)
	guard, err := s.Create(CreateRequest{ServerRef: id, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil {
		t.Fatal(err)
	}
	waitForSFTPState(t, s, guard.ID, 5*time.Second, "open")
	target, err := s.Create(CreateRequest{ServerRef: id, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil {
		t.Fatal(err)
	}
	waitForSFTPState(t, s, target.ID, 5*time.Second, "open")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _ = s.Close(target.ID) }()
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
	if _, err := s.List(guard.ID, "/", ListOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Close(guard.ID)
	settleSFTP(t, s)
}
