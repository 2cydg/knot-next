package session

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// countStackFrames counts the goroutines actually running fn. runtime.Stack
// prints the creating goroutine as a "created by ..." line, so a worker that has
// already retired is not mistaken for one still running because it left a write
// parked behind.
func countStackFrames(fn string) int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, block := range strings.Split(string(buf), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "created by ") {
				continue
			}
			if strings.Contains(line, fn+"(") {
				count++
				break
			}
		}
	}
	return count
}

// waitForStackFrames waits until exactly want goroutines are running fn. A worker
// that has just been started may not be on the scheduler yet, so the count is
// polled rather than read once.
func waitForStackFrames(t *testing.T, fn string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := countStackFrames(fn); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines are running %s, want %d", countStackFrames(fn), fn, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A blocked backend write owns the one slot across all attachments. Repeated
// revocation ends claim workers and discards their queued input without creating
// more blocked writers or replaying those queues after the remote resumes.
func TestReviewRepeatedHandoverKeepsOneInputOwner(t *testing.T) {
	stdin := newBlockingStdin()
	defer stdin.unblock()
	useTestBackend(t, func() *interactiveBackend {
		return &interactiveBackend{stdin: stdin, pump: newOutputPump(), exitResult: newExitResult()}
	})
	service := NewService()
	service.UseLocalTestBackend()
	created, err := service.Create(CreateRequest{ServerRef: "srv-1"})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	first.Input.Write([]byte("already-in-flight"))
	select {
	case <-stdin.started:
	case <-time.After(time.Second):
		t.Fatal("first write never entered stdin")
	}
	first.Cancel()
	first.Release()
	waitForStackFrames(t, "(*attachmentClaim).runInput", 0)
	for i := 0; i < 5; i++ {
		stream, _, err := service.AttachStream(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		waitForStackFrames(t, "(*attachmentClaim).runInput", 1)
		if _, err := stream.Input.Write([]byte("discard-on-revoke")); err != nil {
			t.Fatal(err)
		}
		stream.Cancel()
		stream.Release()
		waitForStackFrames(t, "(*attachmentClaim).runInput", 0)
		if n := countStackFrames("writeStdin"); n != 1 {
			t.Fatalf("%d blocked stdin writers after handover %d, want 1", n, i)
		}
	}
	current, _, err := service.AttachStream(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Cancel()
	defer current.Release()
	if _, err := current.Input.Write([]byte("current-owner")); err != nil {
		t.Fatal(err)
	}
	stdin.unblock()
	got := waitForChunks(t, stdin, 2)
	if len(got) != 2 || got[0] != "already-in-flight" || got[1] != "current-owner" {
		t.Fatalf("remote received %v", got)
	}
	waitForStackFrames(t, "writeStdin", 0)
}
