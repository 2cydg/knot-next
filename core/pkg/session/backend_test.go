package session

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// chunkedReader returns at most chunk bytes per Read, so tests can force output
// to be split across many pump reads without relying on timing.
type chunkedReader struct {
	data  []byte
	chunk int
	pos   int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	if remaining := len(r.data) - r.pos; n > remaining {
		n = remaining
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// collectPump reads every chunk of a subscription until it closes.
func collectPump(t *testing.T, sub *pumpSubscription) ([]byte, []byte) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-sub.Chunks():
			if !ok {
				return stdout.Bytes(), stderr.Bytes()
			}
			if chunk.stream == streamStderr {
				stderr.Write(chunk.data)
			} else {
				stdout.Write(chunk.data)
			}
		case <-deadline:
			t.Fatal("timed out reading pump output")
			return nil, nil
		}
	}
}

// drainChunks reads exactly want chunks from a live subscription, which never
// closes on its own because the pump is still running.
func (p *outputPump) drainChunks(sub *pumpSubscription, want int) []pumpChunk {
	out := make([]pumpChunk, 0, want)
	for len(out) < want {
		chunk, ok := <-sub.Chunks()
		if !ok {
			return out
		}
		out = append(out, chunk)
	}
	return out
}

func TestPumpForwardsBytesWithoutInterpretation(t *testing.T) {
	// ANSI, OSC7, CR/LF, NUL, CJK and invalid UTF-8 must all survive verbatim,
	// including when the reads that carry them are split across frames.
	payload := []byte("\x1b[31mred\x1b[0m" +
		"\x1b]7;file://host/home/clax\x07" +
		"line1\r\nline2\rmid\n" +
		"\x00\x00" +
		"中文输出" +
		string([]byte{0xff, 0xfe, 0x80}))

	for _, chunk := range []int{1, 3, 7, len(payload)} {
		reader := &chunkedReader{data: payload, chunk: chunk}
		pump := newOutputPump()
		pump.AddReader(streamStdout, reader)
		pump.Start()
		sub, backlogTruncated := pump.Subscribe()
		if backlogTruncated {
			t.Fatalf("chunk=%d: backlog reported truncated", chunk)
		}

		got, stderr := collectPump(t, sub)
		if !bytes.Equal(got, payload) {
			t.Fatalf("chunk=%d: output = %q, want %q", chunk, got, payload)
		}
		if len(stderr) != 0 {
			t.Fatalf("chunk=%d: unexpected stderr bytes %q", chunk, stderr)
		}
		pump.Unsubscribe(sub)
	}
}

func TestPumpIgnoresLateAndEmptyReaders(t *testing.T) {
	pump := newOutputPump()
	pump.AddReader(streamStdout, nil) // a nil reader is ignored rather than read
	pump.AddReader(streamStdout, strings.NewReader("only reader"))
	pump.Start()
	// Adding a reader after Start is refused: the pump already owns its readers.
	pump.AddReader(streamStdout, strings.NewReader("late"))

	sub, _ := pump.Subscribe()
	got, _ := collectPump(t, sub)
	if string(got) != "only reader" {
		t.Fatalf("output = %q, want only the reader registered before Start", got)
	}
}

func TestPumpKeepsPerStreamOrder(t *testing.T) {
	stdoutData := []byte("stdout-1|stdout-2|stdout-3")
	stderrData := []byte("stderr-1|stderr-2")

	pump := newOutputPump()
	pump.AddReader(streamStdout, &chunkedReader{data: stdoutData, chunk: 3})
	pump.AddReader(streamStderr, &chunkedReader{data: stderrData, chunk: 2})
	pump.Start()
	sub, _ := pump.Subscribe()

	got, gotErr := collectPump(t, sub)
	if !bytes.Equal(got, stdoutData) {
		t.Fatalf("stdout = %q, want %q", got, stdoutData)
	}
	if !bytes.Equal(gotErr, stderrData) {
		t.Fatalf("stderr = %q, want %q", gotErr, stderrData)
	}
	pump.Unsubscribe(sub)
}

func TestPumpDoneClosesSubscriberChannels(t *testing.T) {
	pump := newOutputPump()
	pump.AddReader(streamStdout, strings.NewReader("data"))
	pump.Start()
	sub, _ := pump.Subscribe()

	collectPump(t, sub)

	// A consumer that sees its stream end can rely on the pump being finished.
	select {
	case <-pump.Done():
	default:
		t.Fatal("pump not done after its only reader reached EOF")
	}
	if sub.Overflowed() {
		t.Fatal("a completed stream must not be reported as truncated")
	}
	if !pump.WaitDone(time.Second) {
		t.Fatal("WaitDone did not observe completion")
	}
}

func TestPumpOverflowIsExplicitNotSilent(t *testing.T) {
	// One byte per read, more reads than the queue can hold, and a consumer that
	// never reads: the pump must stop delivering and say so rather than dropping
	// bytes silently. Subscribing before Start keeps the interleaving exact.
	reads := pumpQueueDepth + 32
	reader := &chunkedReader{data: bytes.Repeat([]byte("x"), reads), chunk: 1}

	pump := newOutputPump()
	pump.AddReader(streamStdout, reader)
	sub, backlogTruncated := pump.Subscribe()
	if backlogTruncated {
		t.Fatal("nothing was produced yet, so no backlog can be truncated")
	}
	pump.Start()

	select {
	case <-sub.Overflow():
	case <-time.After(5 * time.Second):
		t.Fatal("subscription was not marked as overflowed")
	}

	// The stream is closed so the consumer cannot block, and the reason is
	// distinguishable from a normal end of stream.
	if !sub.Overflowed() {
		t.Fatal("Overflowed() = false after overflow")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-sub.Chunks():
			if !ok {
				pump.Unsubscribe(sub)
				return
			}
		case <-deadline:
			t.Fatal("chunks channel not closed after overflow")
		}
	}
}

func TestPumpSlowSubscriberDoesNotBlockFastSubscriber(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 4096)
	pump := newOutputPump()
	pump.AddReader(streamStdout, &chunkedReader{data: payload, chunk: 64})
	pump.Start()

	fast, _ := pump.Subscribe()
	slow, _ := pump.Subscribe() // never read: must not stall the pump

	got, _ := collectPump(t, fast)
	if !bytes.Equal(got, payload) {
		t.Fatalf("fast subscriber got %d bytes, want %d", len(got), len(payload))
	}
	pump.Unsubscribe(fast)
	pump.Unsubscribe(slow)

	if !pump.WaitDone(2 * time.Second) {
		t.Fatal("pump did not finish")
	}
}

func TestPumpUnsubscribeClosesChannelOnce(t *testing.T) {
	blockingReader, writer := io.Pipe()
	defer writer.Close()
	pump := newOutputPump()
	pump.AddReader(streamStdout, blockingReader)
	pump.Start()

	sub, _ := pump.Subscribe()
	pump.Unsubscribe(sub)
	pump.Unsubscribe(sub) // must be a no-op, not a double close

	select {
	case _, ok := <-sub.Chunks():
		if ok {
			t.Fatal("channel delivered a value after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed by unsubscribe")
	}
}

func TestPumpAbortUnblocksConsumers(t *testing.T) {
	blockingReader, writer := io.Pipe()
	defer writer.Close()
	pump := newOutputPump()
	pump.AddReader(streamStdout, blockingReader)
	pump.Start()

	sub, _ := pump.Subscribe()
	pump.Abort()
	pump.Abort() // idempotent

	select {
	case _, ok := <-sub.Chunks():
		if ok {
			t.Fatal("channel delivered a value after abort")
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not close the subscription")
	}
	if sub.Overflowed() {
		t.Fatal("a deliberate abort must not look like a truncation")
	}
}

func TestPumpSubscribeAfterFinishClosesAfterBacklog(t *testing.T) {
	pump := newOutputPump()
	pump.AddReader(streamStdout, strings.NewReader("done"))
	pump.Start()
	<-pump.Done()

	sub, truncated := pump.Subscribe()
	if truncated {
		t.Fatal("a small backlog must not be reported as truncated")
	}
	// The stream is already complete, so the subscription ends as soon as the
	// backlog it was handed has been consumed.
	got, _ := collectPump(t, sub)
	if string(got) != "done" {
		t.Fatalf("late subscription got %q, want the retained backlog", got)
	}
	if sub.Overflowed() {
		t.Fatal("late subscription reported truncation")
	}
}

func TestExitResultBroadcastToOneOwner(t *testing.T) {
	result := newExitResult()
	expected := ExitOutcome{Code: intPtr(42)}

	const observers = 16
	var wg sync.WaitGroup
	got := make([]ExitOutcome, observers)
	for i := 0; i < observers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got[idx] = result.Wait()
		}(i)
	}

	// A racing second result must never replace the first one.
	other := 7
	result.Set(expected)
	result.Set(ExitOutcome{Code: &other})

	wg.Wait()
	for i, outcome := range got {
		if outcome.Code == nil || *outcome.Code != 42 {
			t.Fatalf("observer %d got %+v, want exit code 42", i, outcome)
		}
	}
}

func TestExitOutcomeErrSemantics(t *testing.T) {
	zero, seven, signal := 0, 7, -1
	tests := []struct {
		name    string
		outcome ExitOutcome
		wantErr bool
	}{
		{name: "clean exit 0", outcome: ExitOutcome{Code: &zero}},
		{name: "non-zero exit", outcome: ExitOutcome{Code: &seven}, wantErr: true},
		{name: "signal exit", outcome: ExitOutcome{Code: &signal, DisconnectCause: causeRemoteSignal}, wantErr: true},
		{name: "missing exit status", outcome: ExitOutcome{DisconnectCause: causeExitStatusMissing}, wantErr: true},
		{name: "client disconnected", outcome: ExitOutcome{DisconnectCause: causeClientDisconnected}, wantErr: true},
		{name: "network error", outcome: ExitOutcome{FrameworkError: "connection reset", DisconnectCause: causeNetworkError}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.outcome.Err()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Err() = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.outcome.Succeeded() == tt.wantErr {
				t.Fatalf("Succeeded() = %v inconsistent with wantErr = %v", tt.outcome.Succeeded(), tt.wantErr)
			}
		})
	}
}

func TestLocalTestBackendReportsCleanExitAfterStdinClose(t *testing.T) {
	backend := newLocalInteractiveBackend()

	select {
	case <-backend.exitResult.Ready():
		t.Fatal("local backend reported exit before stdin was closed")
	default:
	}

	backend.CloseStdin()
	backend.CloseStdin() // idempotent

	outcome := backend.exitResult.Wait()
	if !outcome.Succeeded() {
		t.Fatalf("outcome = %+v, want clean exit", outcome)
	}
	if outcome.Code == nil || *outcome.Code != 0 {
		t.Fatalf("outcome code = %v, want explicit 0", outcome.Code)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	backend.Close() // idempotent
}

func intPtr(i int) *int {
	return &i
}

func TestPumpDeliversBacklogToFirstAttachment(t *testing.T) {
	// A shell prints its prompt before any client can attach; that output must
	// still reach the first attachment rather than being discarded.
	payload := []byte("motd line\r\n$ ")
	pump := newOutputPump()
	pump.AddReader(streamStdout, &chunkedReader{data: payload, chunk: 4})
	pump.Start()
	if !pump.WaitDone(2 * time.Second) {
		t.Fatal("pump did not finish")
	}

	sub, backlogTruncated := pump.Subscribe()
	if backlogTruncated {
		t.Fatal("a small backlog must not be reported as truncated")
	}
	got, _ := collectPump(t, sub)
	if !bytes.Equal(got, payload) {
		t.Fatalf("backlog = %q, want %q", got, payload)
	}

	// A second attachment must not replay bytes the first one already received.
	second, _ := pump.Subscribe()
	got, _ = collectPump(t, second)
	if len(got) != 0 {
		t.Fatalf("second attachment replayed %q, want nothing", got)
	}
}

func TestPumpReportsTruncatedBacklog(t *testing.T) {
	// More output than the retained backlog: the newest bytes are kept and the
	// next attachment is told that its backlog is incomplete.
	payload := bytes.Repeat([]byte("z"), pumpBacklogLimit+pumpReadBuffer)
	pump := newOutputPump()
	pump.AddReader(streamStdout, bytes.NewReader(payload))
	pump.Start()
	if !pump.WaitDone(2 * time.Second) {
		t.Fatal("pump did not finish")
	}

	sub, backlogTruncated := pump.Subscribe()
	if !backlogTruncated {
		t.Fatal("an oversized backlog must be reported as truncated")
	}
	got, _ := collectPump(t, sub)
	if len(got) > pumpBacklogLimit {
		t.Fatalf("delivered %d bytes, want at most the %d byte limit", len(got), pumpBacklogLimit)
	}
	if !bytes.Equal(got, payload[len(payload)-len(got):]) {
		t.Fatal("the retained backlog is not the newest output")
	}
}

// R12: retention is promised in bytes, so a stream that is read in tiny pieces
// must still be replayed whole. Before the fix the replay was also limited by
// the live queue depth, so 1 KiB read one byte at a time delivered only the
// first 256 bytes and reported a truncation that never happened.
func TestReviewSmallBacklogKeepsAllBytes(t *testing.T) {
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}

	for _, chunk := range []int{1, 3, 7} {
		pump := newOutputPump()
		pump.AddReader(streamStdout, &chunkedReader{data: payload, chunk: chunk})
		pump.Start()
		if !pump.WaitDone(5 * time.Second) {
			t.Fatalf("chunk=%d: pump did not finish", chunk)
		}

		sub, backlogTruncated := pump.Subscribe()
		if backlogTruncated {
			t.Fatalf("chunk=%d: %d bytes are within the %d byte retention limit and must not be truncated",
				chunk, len(payload), pumpBacklogLimit)
		}
		got, _ := collectPump(t, sub)
		if !bytes.Equal(got, payload) {
			t.Fatalf("chunk=%d: replayed %d bytes, want all %d", chunk, len(got), len(payload))
		}
	}
}

// R12: over the retention limit the replay is the newest bytes of the original
// output, and the truncation is announced rather than silently delivered.
func TestReviewOversizedBacklogKeepsNewestBytes(t *testing.T) {
	payload := make([]byte, pumpBacklogLimit+8192)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	pump := newOutputPump()
	pump.AddReader(streamStdout, &chunkedReader{data: payload, chunk: 1})
	pump.Start()
	if !pump.WaitDone(10 * time.Second) {
		t.Fatal("pump did not finish")
	}

	sub, backlogTruncated := pump.Subscribe()
	if !backlogTruncated {
		t.Fatal("output beyond the retention limit must be reported as truncated")
	}
	got, _ := collectPump(t, sub)
	if len(got) != pumpBacklogLimit {
		t.Fatalf("replayed %d bytes, want exactly the %d byte retention limit", len(got), pumpBacklogLimit)
	}
	if !bytes.Equal(got, payload[len(payload)-len(got):]) {
		t.Fatal("the replayed backlog is not the newest suffix of the output")
	}

	// Retention is bounded in memory too, not just in bytes delivered: a stream
	// read one byte at a time must not leave one retained unit per byte.
	pump.mu.Lock()
	units := len(pump.backlog)
	pump.mu.Unlock()
	if units > pumpQueueDepth {
		t.Fatalf("the cleared backlog left %d units, want a bounded number", units)
	}
}

// R12: stdout and stderr keep their own order. Merging back-to-back output of
// one stream must not reorder or drop bytes when the two streams interleave.
func TestReviewBacklogKeepsPerStreamOrder(t *testing.T) {
	var stdoutBytes, stderrBytes []byte
	pump := newOutputPump()
	for i := range 4096 {
		// Alternate the producer so the backlog is built from many small units of
		// two different streams.
		if i%2 == 0 {
			data := []byte{byte('a' + i%26)}
			stdoutBytes = append(stdoutBytes, data...)
			pump.broadcast(pumpChunk{stream: streamStdout, data: data})
			continue
		}
		data := []byte{byte('A' + i%26)}
		stderrBytes = append(stderrBytes, data...)
		pump.broadcast(pumpChunk{stream: streamStderr, data: data})
	}

	sub, backlogTruncated := pump.Subscribe()
	if backlogTruncated {
		t.Fatal("a backlog within the retention limit must not be reported as truncated")
	}
	// The pump here has no readers of its own, so end it explicitly; the retained
	// backlog is already queued and still has to be delivered.
	pump.Abort()
	gotStdout, gotStderr := collectPump(t, sub)
	if !bytes.Equal(gotStdout, stdoutBytes) {
		t.Fatalf("stdout replay = %q, want %q", gotStdout, stdoutBytes)
	}
	if !bytes.Equal(gotStderr, stderrBytes) {
		t.Fatalf("stderr replay = %q, want %q", gotStderr, stderrBytes)
	}
}

// R12: the retained backlog is a suffix of what the previous attachment did not
// already receive.
func TestReviewBacklogIsSuffixAfterPartialDelivery(t *testing.T) {
	pump := newOutputPump()
	pump.broadcast(pumpChunk{stream: streamStdout, data: []byte("already-seen")})

	first, truncated := pump.Subscribe()
	if truncated {
		t.Fatal("the first attachment's backlog must not be reported as truncated")
	}
	var seen []byte
	for _, chunk := range pump.drainChunks(first, 1) {
		seen = append(seen, chunk.data...)
	}
	pump.Unsubscribe(first)
	if string(seen) != "already-seen" {
		t.Fatalf("first attachment saw %q", seen)
	}

	pump.broadcast(pumpChunk{stream: streamStdout, data: []byte("-detached")})

	second, truncated := pump.Subscribe()
	if truncated {
		t.Fatal("a small detached backlog must not be reported as truncated")
	}
	pump.Abort()
	got, _ := collectPump(t, second)
	if string(got) != "-detached" {
		t.Fatalf("second attachment saw %q, want only the output it missed", got)
	}
}
