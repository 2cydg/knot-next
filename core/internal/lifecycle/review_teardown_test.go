package lifecycle

import (
	"context"
	"testing"
	"time"
)

// TestReviewCleanupDeadlineActuallyBoundsTeardown covers RR01 on the runner
// side. Handing a cleanup a deadline only bounds it if the runner stops waiting
// when the deadline passes: a cleanup that ignores its context would otherwise
// hold the whole teardown open, and the budget would be advisory rather than
// real.
func TestReviewCleanupDeadlineActuallyBoundsTeardown(t *testing.T) {
	runner, err := New(Config{
		GracePeriod:   20 * time.Millisecond,
		WorkerTimeout: 20 * time.Millisecond,
		Logf:          quietLogf,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	runner.cleanups = []Cleanup{func(ctx context.Context) error {
		close(entered)
		<-release
		return ctx.Err()
	}}

	go runner.teardown()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the cleanup never started")
	}

	select {
	case <-runner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("teardown stayed blocked on a cleanup that outlived its budget")
	}

	// The step is left to finish on its own; releasing it must not panic or
	// block, and the runner has already reported the expiry.
	close(release)
}

// TestReviewCleanupExpiryIsRecorded checks that an over-budget cleanup is
// observable rather than silently swallowed: a caller reading a clean shutdown
// must not be told a resource was released when its cleanup never returned.
func TestReviewCleanupExpiryIsRecorded(t *testing.T) {
	runner, err := New(Config{
		GracePeriod:   20 * time.Millisecond,
		WorkerTimeout: 20 * time.Millisecond,
		Logf:          quietLogf,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	release := make(chan struct{})
	runner.cleanups = []Cleanup{func(ctx context.Context) error {
		<-release
		return nil
	}}

	runner.teardown()
	close(release)

	if err := runner.Err(); err == nil {
		t.Fatal("a cleanup that outlived its budget was reported as a clean shutdown")
	}
}

// TestReviewCleanupWithinBudgetIsNotReportedAsFailure keeps the other direction
// honest: a cleanup that returns inside its budget must not be flagged just
// because the shared budget expired while later steps were running.
func TestReviewCleanupWithinBudgetIsNotReportedAsFailure(t *testing.T) {
	runner, err := New(Config{
		GracePeriod:   time.Second,
		WorkerTimeout: 20 * time.Millisecond,
		Logf:          quietLogf,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ran := 0
	runner.cleanups = []Cleanup{
		func(context.Context) error { ran++; return nil },
		func(context.Context) error { ran++; return nil },
	}

	runner.teardown()

	if ran != 2 {
		t.Fatalf("%d cleanups ran, want both", ran)
	}
	if err := runner.Err(); err != nil {
		t.Fatalf("a teardown inside its budget reported %v", err)
	}
}
