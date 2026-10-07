package sftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// These fixtures need no SSH pool and therefore start no pool cleanup goroutines.
func newTransferTestService(t *testing.T) *Service {
	t.Helper()
	svc := NewService(t.TempDir())
	svc.sessions["test"] = &resource{Session: Session{ID: "test", State: "open", Root: svc.root, Backend: "local-sandbox"}}
	return svc
}

// Register a queued resource without scheduling its worker, so queued cancellation
// is tested before any I/O can start. Execution still uses the production state machine.
func queueTestTransfer(svc *Service, id string, items []TransferItem) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	svc.mu.Lock()
	svc.transfers[id] = &transferState{Transfer: Transfer{ID: id, SessionID: "test", State: "queued", StartedAt: time.Now().UTC(), Items: cloneTransferItems(items)}, cancel: cancel}
	svc.mu.Unlock()
	return ctx
}

func awaitTransferSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for transfer barrier")
	}
}

func transferBarrier(t *testing.T) (chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

// Register this after starting the task so it runs before TempDir cleanup,
// including when an assertion exits while a worker is held at a barrier.
func cleanupTestTransfer(t *testing.T, svc *Service, id string, release func()) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = svc.Close("test")
		release()
		_ = waitTransferState(t, svc, "test", id)
	})
}

func TestTransferCancelQueued(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeSession), func(t *testing.T) {
			svc := newTransferTestService(t)
			ctx := queueTestTransfer(svc, "queued", []TransferItem{{State: "queued"}})
			if closeSession {
				if _, err := svc.Close("test"); err != nil {
					t.Fatal(err)
				}
			} else {
				before, err := svc.CancelTransfer("test", "queued")
				if err != nil || before.State != "queued" {
					t.Fatalf("cancel snapshot=%+v err=%v", before, err)
				}
			}
			calls := 0
			svc.executeTransfer(ctx, "queued", func(context.Context, *backendView, *transferWork) (string, error) {
				calls++
				return "completed", nil
			})
			final := waitTransferState(t, svc, "test", "queued")
			if calls != 0 || final.State != "canceled" || final.Items[0].State != "canceled" || final.CompletedAt.IsZero() {
				t.Fatalf("worker calls=%d final=%+v", calls, final)
			}
			again, err := svc.CancelTransfer("test", "queued")
			if err != nil || !reflect.DeepEqual(final, again) {
				t.Fatalf("repeat cancel=%+v err=%v", again, err)
			}
		})
	}
}

// First copy a prefix, then hold the next read until the test has canceled.
// Returning a transport error also checks that cancellation survives I/O errors.
type transferGateReader struct {
	prefix  []byte
	ready   chan struct{}
	release <-chan struct{}
	err     error
}

func (r *transferGateReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	close(r.ready)
	<-r.release
	return 0, r.err
}

func TestTransferCancelRunning(t *testing.T) {
	svc := newTransferTestService(t)
	ready, done := make(chan struct{}), make(chan struct{})
	release, unblock := transferBarrier(t)
	var dst bytes.Buffer
	transfer, err := svc.startTransfer("test", "upload", "source", "target", nil, func(ctx context.Context, _ *backendView, work *transferWork) (string, error) {
		defer close(done)
		work.BytesTotal, work.FilesTotal = 20, 1
		r := &transferGateReader{prefix: []byte("prefix"), ready: ready, release: release, err: io.ErrClosedPipe}
		return "", svc.copyWithProgress(ctx, work, "target", r, &dst, 20, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestTransfer(t, svc, transfer.ID, unblock)
	awaitTransferSignal(t, ready)
	before, err := svc.CancelTransfer("test", transfer.ID)
	if err != nil || before.State != "running" || before.BytesCopied != 6 {
		t.Fatalf("cancel snapshot=%+v err=%v", before, err)
	}
	unblock()
	awaitTransferSignal(t, done)
	final := waitTransferState(t, svc, "test", transfer.ID)
	if final.State != "canceled" || final.BytesCopied != 6 || final.FilesDone != 0 || dst.String() != "prefix" {
		t.Fatalf("final=%+v copied=%q", final, dst.String())
	}
}

func TestBatchCancelStopsSubsequentItems(t *testing.T) {
	for _, cancelAt := range []int{0, 1, 2} {
		for _, ioError := range []error{context.Canceled, io.ErrClosedPipe} {
			t.Run(fmt.Sprintf("item=%d/error=%v", cancelAt, ioError), func(t *testing.T) {
				svc := newTransferTestService(t)
				ready, done := make(chan struct{}), make(chan struct{})
				release, unblock := transferBarrier(t)
				calls := [3]int{}
				items := []TransferItem{{State: "queued"}, {State: "queued"}, {State: "queued"}}
				transfer, err := svc.startTransfer("test", "upload", "", "target", items, func(ctx context.Context, _ *backendView, work *transferWork) (string, error) {
					defer close(done)
					return svc.runBatch(ctx, work, "upload", len(items), func(i int) error {
						calls[i]++
						if i == cancelAt {
							close(ready)
							<-release
							return ioError
						}
						if i == 0 && cancelAt == 2 {
							return os.ErrNotExist
						}
						work.FilesDone++
						return nil
					})
				})
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestTransfer(t, svc, transfer.ID, unblock)
				awaitTransferSignal(t, ready)
				if _, err := svc.CancelTransfer("test", transfer.ID); err != nil {
					t.Fatal(err)
				}
				unblock()
				awaitTransferSignal(t, done)
				final := waitTransferState(t, svc, "test", transfer.ID)
				if final.State != "canceled" {
					t.Fatalf("final=%+v", final)
				}
				for i, item := range final.Items {
					wantState, wantCalls := "canceled", 0
					if i <= cancelAt {
						wantCalls = 1
					}
					if i < cancelAt {
						wantState = "completed"
					}
					if i == 0 && cancelAt == 2 {
						wantState = "failed"
					}
					if item.State != wantState || calls[i] != wantCalls {
						t.Fatalf("item %d=%+v calls=%d want %s/%d", i, item, calls[i], wantState, wantCalls)
					}
				}
				if cancelAt == 2 && final.Items[0].Error == "" {
					t.Fatal("previous failure was lost")
				}
			})
		}
	}
}

func TestTransferCancelAndCompleteRace(t *testing.T) {
	svc := newTransferTestService(t)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("race_%d", i)
		ctx := queueTestTransfer(svc, id, nil)
		ready, release := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make(chan error, 1)
		go func() {
			defer wg.Done()
			svc.executeTransfer(ctx, id, func(ctx context.Context, _ *backendView, _ *transferWork) (string, error) {
				close(ready)
				<-release
				return "", ctx.Err()
			})
		}()
		awaitTransferSignal(t, ready)
		go func() { defer wg.Done(); <-release; _, err := svc.CancelTransfer("test", id); errs <- err }()
		close(release)
		wg.Wait()
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		final := waitTransferState(t, svc, "test", id)
		if (final.State != "completed" && final.State != "canceled") || final.CompletedAt.IsZero() {
			t.Fatalf("final=%+v", final)
		}
		again, err := svc.CancelTransfer("test", id)
		if err != nil || !reflect.DeepEqual(final, again) {
			t.Fatalf("terminal changed: %+v -> %+v err=%v", final, again, err)
		}
	}
}

func TestTerminalStateProtection(t *testing.T) {
	for _, state := range []string{"completed", "failed", "partial_failed", "canceled"} {
		t.Run(state, func(t *testing.T) {
			svc := newTransferTestService(t)
			ctx := queueTestTransfer(svc, state, nil)
			events, cancel, err := svc.SubscribeTransfers("test")
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			svc.finishTransfer(state, state, "original result")
			original, err := svc.Transfer("test", state)
			if err != nil {
				t.Fatal(err)
			}
			work := &transferWork{Transfer: Transfer{ID: state, BytesCopied: 999, Items: []TransferItem{{State: "running"}}}}
			svc.publishWorkerProgress(work)
			svc.finishTransferFromWorker(state, work, "completed", "late")
			svc.finishTransfer(state, "failed", "late")
			if _, _, ok := svc.markTransferRunning(ctx, state); ok {
				t.Fatal("terminal transfer restarted")
			}
			again, err := svc.CancelTransfer("test", state)
			if err != nil || !reflect.DeepEqual(original, again) {
				t.Fatalf("terminal changed: %+v -> %+v", original, again)
			}
			if event := <-events; event.Type != "sftp.transfer."+state {
				t.Fatalf("event=%+v", event)
			}
			select {
			case event := <-events:
				t.Fatalf("late event=%+v", event)
			default:
			}
		})
	}
}

func TestConcurrentTransferOperations(t *testing.T) {
	svc := newTransferTestService(t)
	ctx := queueTestTransfer(svc, "concurrent", []TransferItem{{State: "queued"}})
	_, work, ok := svc.markTransferRunning(ctx, "concurrent")
	if !ok {
		t.Fatal("worker not started")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for reader := 0; reader < 3; reader++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				var err error
				switch reader {
				case 0:
					_, err = svc.Transfer("test", "concurrent")
				case 1:
					_, err = svc.ListTransfers("test")
				case 2:
					_, cancel, _, subErr := svc.SubscribeTransfersWithSnapshot("test")
					err = subErr
					if cancel != nil {
						cancel()
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(reader)
	}
	close(start)
	for i := 1; i <= 100; i++ {
		work.BytesTotal, work.FilesTotal = 100, 1
		work.BytesCopied = int64(i)
		work.Items[0].BytesCopied = int64(i)
		work.Items[0].State = "running"
		svc.publishWorkerProgress(work)
	}
	work.FilesDone, work.Items[0].State = 1, "completed"
	svc.finishTransferFromWorker("concurrent", work, "completed", "")
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	final := waitTransferState(t, svc, "test", "concurrent")
	if final.BytesCopied != 100 || final.FilesDone != 1 || final.Items[0].State != "completed" {
		t.Fatalf("final=%+v", final)
	}
}

func TestTransferSnapshotsAndSlowSubscriber(t *testing.T) {
	svc := newTransferTestService(t)
	ctx := queueTestTransfer(svc, "snapshot", []TransferItem{{State: "queued"}})
	_, work, ok := svc.markTransferRunning(ctx, "snapshot")
	if !ok {
		t.Fatal("worker not started")
	}
	events, cancel, snapshot, err := svc.SubscribeTransfersWithSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	work.Items[0].State = "running"
	svc.publishWorkerProgress(work)
	if snapshot[0].Items[0].State != "queued" {
		t.Fatal("snapshot shares worker items")
	}
	for i := 0; i < maxTransferSubs+2; i++ {
		work.BytesCopied++
		svc.publishWorkerProgress(work)
	}
	work.Items[0].State = "completed"
	svc.finishTransferFromWorker(work.ID, work, "completed", "")
	// The unread subscriber is full, so the terminal event was dropped.
	if len(events) != maxTransferSubs {
		t.Fatalf("event buffer=%d", len(events))
	}
	for len(events) > 0 {
		if isTerminalState((<-events).State) {
			t.Fatal("expected a dropped terminal event")
		}
	}
	final := waitTransferState(t, svc, "test", work.ID)
	if final.State != "completed" {
		t.Fatalf("GET could not recover: %+v", final)
	}
	final.Items[0].State = "changed by caller"
	list, err := svc.ListTransfers("test")
	if err != nil {
		t.Fatal(err)
	}
	list[0].Items[0].State = "changed list"
	_, lateCancel, late, err := svc.SubscribeTransfersWithSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	defer lateCancel()
	if late[0].Items[0].State != "completed" {
		t.Fatal("GET/list mutated shared items")
	}
	late[0].Items[0].State = "changed snapshot"
	again, err := svc.Transfer("test", work.ID)
	if err != nil || again.Items[0].State != "completed" {
		t.Fatalf("late snapshot mutated resource: %+v err=%v", again, err)
	}
}

func TestTransferSubscriptionIsolationAndFinish(t *testing.T) {
	svc := newTransferTestService(t)
	svc.sessions["other"] = &resource{Session: Session{ID: "other", State: "open", Root: t.TempDir()}}
	ready, done := make(chan struct{}), make(chan struct{})
	release, unblock := transferBarrier(t)
	transfer, err := svc.startTransfer("test", "upload", "source", "target", nil, func(context.Context, *backendView, *transferWork) (string, error) {
		defer close(done)
		close(ready)
		<-release
		return "completed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestTransfer(t, svc, transfer.ID, unblock)
	awaitTransferSignal(t, ready)
	events, cancel, snapshot, err := svc.SubscribeTransfersWithSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	other, otherCancel, otherSnapshot, err := svc.SubscribeTransfersWithSnapshot("other")
	if err != nil {
		t.Fatal(err)
	}
	defer otherCancel()
	if len(snapshot) != 1 || snapshot[0].State != "running" || len(otherSnapshot) != 0 {
		t.Fatalf("snapshots=%+v / %+v", snapshot, otherSnapshot)
	}
	if _, err := svc.Transfer("other", transfer.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-session GET=%v", err)
	}
	if _, err := svc.CancelTransfer("other", transfer.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-session cancel=%v", err)
	}
	unblock()
	awaitTransferSignal(t, done)
	final := waitTransferState(t, svc, "test", transfer.ID)
	select {
	case event := <-events:
		if event.Type != "sftp.transfer.completed" || event.State != final.State {
			t.Fatalf("event=%+v final=%+v", event, final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing terminal event")
	}
	select {
	case event := <-other:
		t.Fatalf("cross-session event=%+v", event)
	default:
	}
}

func TestTransferEmptyFile(t *testing.T) {
	svc := newTransferTestService(t)
	src := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(src, nil, 0600); err != nil {
		t.Fatal(err)
	}
	transfer, err := svc.Upload("test", TransferRequest{Source: src, Target: "/empty"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitTransferState(t, svc, "test", transfer.ID)
	info, err := os.Stat(filepath.Join(svc.root, "empty"))
	if err != nil || info.Size() != 0 || final.State != "completed" || final.FilesDone != 1 || final.BytesCopied != 0 {
		t.Fatalf("file=%v final=%+v err=%v", info, final, err)
	}
}

func TestTransferSubscriberReconnectDuringTransfer(t *testing.T) {
	svc := newTransferTestService(t)
	ready, done := make(chan struct{}), make(chan struct{})
	release, unblock := transferBarrier(t)
	transfer, err := svc.startTransfer("test", "upload", "source", "target", nil, func(context.Context, *backendView, *transferWork) (string, error) {
		defer close(done)
		close(ready)
		<-release
		return "completed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestTransfer(t, svc, transfer.ID, unblock)
	awaitTransferSignal(t, ready)
	_, disconnect, _, err := svc.SubscribeTransfersWithSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	disconnect()
	before, err := svc.Transfer("test", transfer.ID)
	if err != nil || before.State != "running" {
		t.Fatalf("unsubscribe stopped transfer: %+v err=%v", before, err)
	}
	_, reconnectCancel, recovered, err := svc.SubscribeTransfersWithSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	defer reconnectCancel()
	if len(recovered) != 1 || recovered[0].ID != transfer.ID || recovered[0].State != "running" {
		t.Fatalf("recovery=%+v", recovered)
	}
	unblock()
	awaitTransferSignal(t, done)
	final := waitTransferState(t, svc, "test", transfer.ID)
	if final.State != "completed" {
		t.Fatalf("final=%+v", final)
	}
	list, err := svc.ListTransfers("test")
	if err != nil || len(list) != 1 {
		t.Fatalf("duplicate transfer on reconnect: %+v err=%v", list, err)
	}
}
