package session

import (
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// pumpSubscriberCount reports how many attachments currently own the session's
// output. More than one means two clients are reading the same PTY.
func pumpSubscriberCount(t *testing.T, service *Service, id string) int {
	t.Helper()
	service.mu.Lock()
	session := service.sessions[id]
	backend := session.backend
	service.mu.Unlock()
	if backend == nil {
		return 0
	}
	backend.pump.mu.Lock()
	defer backend.pump.mu.Unlock()
	return len(backend.pump.subscribers)
}

// countGoroutines counts goroutines whose stack mentions name.
func countGoroutines(name string) int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), name)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitForNoGoroutine waits until no goroutine is running the named function, so
// a worker that is going away has provably gone away.
func waitForNoGoroutine(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if count := countGoroutines(name); count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines are still running %s", countGoroutines(name), name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReviewDetachRevokesOutputOwner is the reproduction from the review: an
// explicit detach used to clear the attached flag without revoking the old
// attachment, so a second attach left two live subscriptions on the same pump.
func TestReviewDetachRevokesOutputOwner(t *testing.T) {
	service := NewService()
	service.UseLocalTestBackend()
	created, err := service.Create(CreateRequest{ServerRef: "srv-1"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if got := pumpSubscriberCount(t, service, created.ID); got != 1 {
		t.Fatalf("subscribers while attached = %d, want 1", got)
	}

	if _, err := service.Control(created.ID, ControlRequest{Type: "detach"}); err != nil {
		t.Fatalf("detach: %v", err)
	}

	// The old attachment is told it lost ownership and stops receiving output.
	select {
	case <-first.Revoked:
	case <-time.After(5 * time.Second):
		t.Fatal("detach did not revoke the first attachment")
	}
	if got := pumpSubscriberCount(t, service, created.ID); got != 0 {
		t.Fatalf("subscribers after detach = %d, want 0", got)
	}
	select {
	case _, ok := <-first.Stdout:
		if ok {
			t.Fatal("the revoked attachment still received output")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked attachment's output stream did not end")
	}

	second, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if got := pumpSubscriberCount(t, service, created.ID); got != 1 {
		t.Fatalf("subscribers after re-attach = %d, want exactly one owner", got)
	}
	second.Cancel()
	second.Release()
	if got := pumpSubscriberCount(t, service, created.ID); got != 0 {
		t.Fatalf("subscribers after release = %d, want 0", got)
	}
}

// TestReviewCancelledAttachStopsWorkers is the exit-waiter reproduction: a shell
// that lives for hours must not accumulate one waiter — or one relay, or one
// input worker — per cancelled attachment.
func TestReviewCancelledAttachStopsWorkers(t *testing.T) {
	service := NewService()
	service.UseLocalTestBackend()
	created, err := service.Create(CreateRequest{ServerRef: "srv-1"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Upgrading a WebSocket and losing it, or detaching over and over, takes this
	// path: attach, then cancel and release without the session ever exiting.
	for i := 0; i < 8; i++ {
		stream, _, err := service.AttachStream(created.ID)
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		if got := pumpSubscriberCount(t, service, created.ID); got != 1 {
			t.Fatalf("cycle %d: subscribers = %d, want 1", i, got)
		}
		stream.Cancel()
		stream.Release()
		if got := pumpSubscriberCount(t, service, created.ID); got != 0 {
			t.Fatalf("cycle %d: subscribers after release = %d, want 0", i, got)
		}
	}

	// The session is still usable and nothing from the cancelled attachments is
	// left waiting on an outcome that has not happened yet.
	waitForNoGoroutine(t, "waitForExit")
	waitForNoGoroutine(t, "relayOutput")
	waitForNoGoroutine(t, "runInput")

	stream, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("attach after cycles: %v", err)
	}
	defer func() {
		stream.Cancel()
		stream.Release()
	}()
	if state, err := service.Get(created.ID); err != nil || state.State != "attached" {
		t.Fatalf("session = %+v (err %v), want the still-attached session", state, err)
	}
}

// blockingStdin records what reached it and parks the first write until the test
// releases it: a remote that stopped reading, with the send window full.
type blockingStdin struct {
	mu      sync.Mutex
	written []string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingStdin() *blockingStdin {
	return &blockingStdin{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (s *blockingStdin) Write(p []byte) (int, error) {
	s.started <- struct{}{}
	<-s.release
	s.mu.Lock()
	s.written = append(s.written, string(p))
	s.mu.Unlock()
	return len(p), nil
}

func (s *blockingStdin) Close() error { return nil }

func (s *blockingStdin) chunks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.written...)
}

func (s *blockingStdin) unblock() { s.once.Do(func() { close(s.release) }) }

// waitForChunks waits until the remote has received exactly want chunks.
func waitForChunks(t *testing.T, stdin *blockingStdin, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := stdin.chunks(); len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote received %d chunks, want %d: %v", len(stdin.chunks()), want, stdin.chunks())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestReviewStalledInputIsNotReplayedToTheNextOwner covers the input half of R10:
// with the remote no longer reading, detaching must end the old worker, must not
// deliver what was still queued, and must not keep the next attachment out.
func TestReviewStalledInputIsNotReplayedToTheNextOwner(t *testing.T) {
	previousGrace := attachmentGrace
	attachmentGrace = 200 * time.Millisecond
	t.Cleanup(func() { attachmentGrace = previousGrace })

	stdin := newBlockingStdin()
	useTestBackend(t, func() *interactiveBackend {
		return &interactiveBackend{stdin: stdin, pump: newOutputPump(), exitResult: newExitResult()}
	})
	service := NewService()
	service.UseLocalTestBackend()
	created, err := service.Create(CreateRequest{ServerRef: "srv-1"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if _, err := first.Input.Write([]byte("in-flight")); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	select {
	case <-stdin.started:
	case <-time.After(5 * time.Second):
		t.Fatal("input never reached the remote stdin")
	}
	// This chunk is queued behind the one the worker is parked on.
	if _, err := first.Input.Write([]byte("queued-behind")); err != nil {
		t.Fatalf("second chunk: %v", err)
	}

	first.Cancel()
	first.Release()

	// The next client must be able to attach without waiting for a remote that
	// stopped reading; the wait for the old worker is bounded.
	start := time.Now()
	second, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("re-attach waited %s for a stalled input worker", elapsed)
	}

	stdin.unblock()
	waitForChunks(t, stdin, 1)
	if _, err := second.Input.Write([]byte("new-owner")); err != nil {
		t.Fatalf("write after re-attach: %v", err)
	}
	if got := waitForChunks(t, stdin, 2); !reflect.DeepEqual(got, []string{"in-flight", "new-owner"}) {
		t.Fatalf("remote received %v, want the new owner's input only", got)
	}
	second.Cancel()
	second.Release()
}

// TestReviewDetachedAttachmentInputIsRefused checks that the old attachment's
// input handle stops accepting bytes instead of queueing them for a session it
// no longer owns.
func TestReviewDetachedAttachmentInputIsRefused(t *testing.T) {
	service := NewService()
	service.UseLocalTestBackend()
	created, err := service.Create(CreateRequest{ServerRef: "srv-1"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	first.Cancel()
	first.Release()

	if _, err := first.Input.Write([]byte("late")); err != ErrAttachmentClosed {
		t.Fatalf("write on a revoked attachment = %v, want ErrAttachmentClosed", err)
	}
}
