package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecPrunePreservesPendingCleanup(t *testing.T) {
	svc := NewService()
	now := time.Unix(10000, 0)
	svc.execPolicy.History = 2
	svc.execPolicy.Now = func() time.Time { return now }
	svc.execs["pending"] = Exec{ID: "pending", CompletedAt: time.Unix(1, 0), CleanupError: "cleanup_timeout"}
	svc.execActive["pending"] = &execOperation{done: make(chan struct{})}
	for i := 1; i <= svc.execPolicy.History+1; i++ {
		id := fmt.Sprintf("history-%d", i)
		svc.execs[id] = Exec{ID: id, CompletedAt: now.Add(time.Duration(i) * -time.Second)}
	}
	svc.pruneExecsLocked()
	if result, ok := svc.execs["pending"]; !ok || result.CleanupError != "cleanup_timeout" {
		t.Fatal("pruning erased the pending cleanup failure")
	}
	if len(svc.execs) != svc.execPolicy.History+1 {
		t.Fatal("settled history was not pruned")
	}

	// If every entry is live, pruning must neither delete live diagnostics nor
	// index past the empty list of eligible history records.
	for i := len(svc.execs); i < svc.execPolicy.Active; i++ {
		id := fmt.Sprintf("live-%d", i)
		svc.execs[id] = Exec{ID: id}
	}
	for id := range svc.execs {
		svc.execActive[id] = &execOperation{done: make(chan struct{})}
	}
	svc.pruneExecsLocked()
	if len(svc.execs) != svc.execPolicy.Active {
		t.Fatal("live records were deleted")
	}
}

// observedWaitContext signals when ShutdownExec has taken its active snapshot
// and entered the completion wait, avoiding a scheduling sleep in the test.
type observedWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestExecShutdownKeepsResultAfterHistoryPrune(t *testing.T) {
	svc := NewService()
	now := time.Unix(10000, 0)
	svc.execPolicy.History = 2
	svc.execPolicy.Now = func() time.Time { return now }
	operation := &execOperation{done: make(chan struct{})}
	svc.execActive["pending"] = operation
	budget, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx := &observedWaitContext{Context: budget, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- svc.ShutdownExec(ctx) }()
	awaitExec(t, ctx.entered)
	svc.mu.Lock()
	operation.result = Exec{ID: "pending", CompletedAt: now.Add(-time.Minute), CleanupError: "cleanup_timeout"}
	svc.execs["pending"] = operation.result
	delete(svc.execActive, "pending")
	close(operation.done)
	for i := 1; i <= svc.execPolicy.History; i++ {
		id := fmt.Sprintf("history-%d", i)
		svc.execs[id] = Exec{ID: id, CompletedAt: now.Add(time.Duration(i) * -time.Second)}
	}
	svc.pruneExecsLocked()
	_, retained := svc.execs["pending"]
	svc.mu.Unlock()
	if retained {
		t.Fatal("settled pending record was not pruned")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cleanup_timeout") {
			t.Fatalf("shutdown lost cleanup error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown failed to finish")
	}
}

func TestExecCompletedResultWinsConcurrentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 1000; i++ {
		ran := make(chan error, 1)
		ran <- nil
		if err, completed := waitExecResult(ctx, ran); err != nil || !completed {
			t.Fatalf("completed result became cancellation: %v %v", err, completed)
		}
	}
	remoteError := errors.New("remote result")
	ran := make(chan error, 1)
	ran <- remoteError
	if err, completed := waitExecResult(ctx, ran); !errors.Is(err, remoteError) || !completed {
		t.Fatalf("published failure was replaced: %v %v", err, completed)
	}
	if err, completed := waitExecResult(ctx, make(chan error)); !errors.Is(err, context.Canceled) || completed {
		t.Fatalf("unfinished command ignored cancellation: %v %v", err, completed)
	}
}

func TestExecHistoryCapacityAndTTL(t *testing.T) {
	s := NewService()
	now := time.Unix(10000, 0)
	s.execPolicy.History = 2
	s.execPolicy.TTL = time.Minute
	s.execPolicy.Now = func() time.Time { return now }
	s.execs["pending"] = Exec{ID: "pending", CompletedAt: now.Add(-time.Hour), CleanupError: "cleanup_timeout"}
	s.execActive["pending"] = &execOperation{done: make(chan struct{})}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint(i)
		s.execs[id] = Exec{ID: id, CompletedAt: now.Add(time.Duration(i-4) * time.Second)}
	}
	s.pruneExecsLocked()
	if len(s.execs) != 3 || s.execs["2"].ID == "" || s.execs["3"].ID == "" {
		t.Fatalf("capacity did not retain newest results: %+v", s.execs)
	}
	if _, exists := s.execs["1"]; exists {
		t.Fatal("oldest unexpired result survived capacity pressure")
	}
	now = now.Add(time.Minute)
	s.pruneExecsLocked()
	if len(s.execs) != 1 || s.execs["pending"].CleanupError != "cleanup_timeout" {
		t.Fatalf("TTL erased live cleanup or retained expired history: %+v", s.execs)
	}
	delete(s.execActive, "pending")
	s.pruneExecsLocked()
	if len(s.execs) != 0 {
		t.Fatal("settled expired cleanup survived")
	}
}
