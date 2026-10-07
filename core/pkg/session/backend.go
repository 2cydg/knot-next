package session

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Output stream identifiers for a PTY-backed interactive session.
type outputStream uint8

const (
	streamStdout outputStream = iota
	streamStderr
)

const (
	// pumpReadBuffer is the read size used per stream. 8 KiB balances syscall
	// overhead against the added latency of buffering interactive output.
	pumpReadBuffer = 8 * 1024
	// pumpQueueDepth bounds how far a single consumer may fall behind. It is
	// deliberately finite: a consumer that cannot keep up is disconnected with
	// an explicit overflow signal instead of growing without bound.
	pumpQueueDepth = 256
	// pumpBacklogLimit bounds output retained while nothing is attached, so a
	// client that attaches after the remote has already printed (a shell prompt,
	// for instance) still sees it. It is a bounded backlog, not a scrollback:
	// beyond the limit the oldest bytes are dropped and the next attachment is
	// told the backlog is incomplete.
	pumpBacklogLimit = 64 * 1024
)

// pumpChunk is one read from a PTY stream.
type pumpChunk struct {
	stream outputStream
	data   []byte
}

// pumpSubscription is one consumer's view of pump output. Ordering is
// guaranteed within a stream; chunks from different streams are not ordered
// relative to each other.
type pumpSubscription struct {
	chunks   chan pumpChunk
	overflow chan struct{}
	once     sync.Once
}

// Chunks delivers output until the pump finishes, the subscription is
// unsubscribed, or the subscriber overflows.
func (s *pumpSubscription) Chunks() <-chan pumpChunk {
	return s.chunks
}

// Overflow is closed when this subscription fell behind and output could not be
// delivered. The consumer must treat the stream as incomplete.
func (s *pumpSubscription) Overflow() <-chan struct{} {
	return s.overflow
}

// Overflowed reports whether the subscription was dropped for falling behind.
func (s *pumpSubscription) Overflowed() bool {
	select {
	case <-s.overflow:
		return true
	default:
		return false
	}
}

func (s *pumpSubscription) markOverflow() {
	s.once.Do(func() { close(s.overflow) })
}

// outputPump owns the only readers on an interactive session's output streams
// and fans their bytes out to subscribers. Exactly one pump exists per backend,
// so a re-attach never creates a second reader racing the first.
//
// Output produced while nothing is attached is retained in a bounded backlog and
// handed to the next attachment. Bytes already delivered to an attachment are
// never retained, so re-attaching never replays what the client already saw.
type outputPump struct {
	mu          sync.Mutex
	readers     []pumpReader
	subscribers map[*pumpSubscription]struct{}
	backlog     []pumpChunk
	backlogSize int
	// backlogDropped records that the backlog limit was hit, so the next
	// attachment can be told its backlog is incomplete.
	backlogDropped bool
	started        bool
	finished       bool
	done           chan struct{}
	abortOnce      sync.Once
	wg             sync.WaitGroup
}

type pumpReader struct {
	stream outputStream
	reader io.Reader
}

func newOutputPump() *outputPump {
	return &outputPump{
		subscribers: make(map[*pumpSubscription]struct{}),
		done:        make(chan struct{}),
	}
}

// AddReader registers a stream to read. It must be called before Start.
func (p *outputPump) AddReader(stream outputStream, reader io.Reader) {
	if reader == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.readers = append(p.readers, pumpReader{stream: stream, reader: reader})
}

// Start begins reading every registered stream. Must be called at most once.
func (p *outputPump) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	readers := append([]pumpReader(nil), p.readers...)
	p.mu.Unlock()

	for _, r := range readers {
		p.wg.Add(1)
		go p.run(r.stream, r.reader)
	}
	go func() {
		p.wg.Wait()
		p.finish()
	}()
}

func (p *outputPump) run(stream outputStream, reader io.Reader) {
	defer p.wg.Done()
	buf := make([]byte, pumpReadBuffer)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			p.broadcast(pumpChunk{stream: stream, data: data})
		}
		if err != nil {
			// EOF, a read error, or the owning session was closed: either way no
			// further bytes can arrive from this stream.
			return
		}
	}
}

// broadcast delivers a chunk to every subscriber. With no subscriber attached
// the chunk goes to the bounded backlog instead, so the next attachment can see
// what it missed. A subscriber whose queue is full is removed and told
// explicitly rather than having bytes dropped silently, so consumers can
// distinguish "stream ended" from "stream truncated".
func (p *outputPump) broadcast(chunk pumpChunk) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	if len(p.subscribers) == 0 {
		p.appendBacklogLocked(chunk)
		return
	}
	for sub := range p.subscribers {
		select {
		case sub.chunks <- chunk:
		default:
			delete(p.subscribers, sub)
			sub.markOverflow()
			close(sub.chunks)
		}
	}
}

// appendBacklogLocked keeps only the most recent bounded amount of output: for a
// terminal the newest bytes (the prompt that just printed) matter most.
//
// Back-to-back output of one stream is merged into a single retained unit, so
// retention stays a byte contract. A remote read one byte at a time must not
// consume the delivery budget with fragments: the limit is what bounds the
// backlog, not the number of reads that produced it.
func (p *outputPump) appendBacklogLocked(chunk pumpChunk) {
	if n := len(p.backlog); n > 0 {
		last := &p.backlog[n-1]
		if last.stream == chunk.stream && len(last.data)+len(chunk.data) <= pumpBacklogLimit {
			last.data = append(last.data, chunk.data...)
			p.backlogSize += len(chunk.data)
			p.trimBacklogLocked()
			return
		}
	}
	p.backlog = append(p.backlog, chunk)
	p.backlogSize += len(chunk.data)
	p.trimBacklogLocked()
}

// trimBacklogLocked drops the oldest bytes until the backlog is within its limit.
// The cut falls on a byte boundary rather than on a chunk boundary, so the
// retained output is exactly the newest pumpBacklogLimit bytes and the oldest
// retained unit may be a suffix of the unit that produced it.
func (p *outputPump) trimBacklogLocked() {
	dropped := false
	for p.backlogSize > pumpBacklogLimit && len(p.backlog) > 0 {
		excess := p.backlogSize - pumpBacklogLimit
		head := &p.backlog[0]
		if len(head.data) <= excess {
			p.backlogSize -= len(head.data)
			p.backlog = p.backlog[1:]
			dropped = true
			continue
		}
		head.data = head.data[excess:]
		p.backlogSize -= excess
		dropped = true
	}
	if dropped {
		p.backlogDropped = true
	}
}

// finish closes every subscriber and the done channel exactly once. done is
// closed first so that a consumer which observes its stream ending can rely on
// the pump already being finished.
func (p *outputPump) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	close(p.done)
	for sub := range p.subscribers {
		delete(p.subscribers, sub)
		close(sub.chunks)
	}
}

// Subscribe registers a consumer and hands it any output that arrived while
// nothing was attached. The second return value reports that the handed-over
// backlog is incomplete because the retained limit was exceeded. The caller must
// call Unsubscribe when done, but may rely on the channel being closed either by
// the pump finishing, by an overflow, or by Unsubscribe itself.
func (p *outputPump) Subscribe() (*pumpSubscription, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Retention is promised in bytes, so the hand-over queue is sized for the
	// retained units rather than reusing the live queue depth. A backlog that
	// arrived as many small reads must not be reduced to the first few of them.
	capacity := pumpQueueDepth
	if len(p.backlog) > capacity {
		capacity = len(p.backlog)
	}
	sub := &pumpSubscription{
		chunks:   make(chan pumpChunk, capacity),
		overflow: make(chan struct{}),
	}

	backlogIncomplete := p.backlogDropped
	for _, chunk := range p.backlog {
		sub.chunks <- chunk
	}
	p.backlog = nil
	p.backlogSize = 0
	p.backlogDropped = false

	if p.finished {
		// Output already ended: hand back an immediately complete subscription.
		close(sub.chunks)
		return sub, backlogIncomplete
	}
	p.subscribers[sub] = struct{}{}
	return sub, backlogIncomplete
}

// Unsubscribe removes a subscriber and closes its channel. It is safe to call
// after the pump already removed the subscription (overflow or completion).
func (p *outputPump) Unsubscribe(sub *pumpSubscription) {
	if sub == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.subscribers[sub]; ok {
		delete(p.subscribers, sub)
		close(sub.chunks)
	}
}

// Abort stops delivering output immediately. Consumers observe a closed stream,
// so an attach relay unblocks; readers blocked on the underlying stream are
// released by closing the owning SSH session.
func (p *outputPump) Abort() {
	p.abortOnce.Do(p.finish)
}

// Done is closed once no further output will be delivered.
func (p *outputPump) Done() <-chan struct{} {
	return p.done
}

// WaitDone waits up to timeout for the pump to finish.
func (p *outputPump) WaitDone(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// ExitOutcome is the final result of an interactive session. It is recorded on
// the session resource and published to attach observers from the same value, so
// GET and the attach exit message can never disagree.
type ExitOutcome struct {
	// Code is the remote exit status. Nil means no exit status was received.
	Code *int
	// FrameworkError describes a framework-level failure (network error, missing
	// exit status). Empty for a remote exit, even a non-zero one.
	FrameworkError string
	// DisconnectCause is the machine-readable reason the session ended.
	DisconnectCause string
}

// Disconnect causes recorded on session resources and exit outcomes.
const (
	causeExitStatusMissing  = "exit_status_missing"
	causeClientDisconnected = "client_disconnected"
	causePoolDisconnected   = "pool_disconnected"
	causeNetworkError       = "network_error"
	causeRemoteSignal       = "remote_signal"
)

// Succeeded reports whether the remote exited cleanly with status 0.
func (o ExitOutcome) Succeeded() bool {
	return o.FrameworkError == "" && o.Code != nil && *o.Code == 0
}

// Err returns nil for a clean exit and a descriptive error otherwise. A session
// that ended without an exit status or was closed locally is never reported as
// success.
func (o ExitOutcome) Err() error {
	switch {
	case o.FrameworkError != "":
		return fmt.Errorf("%s", o.FrameworkError)
	case o.Code == nil:
		if o.DisconnectCause != "" {
			return fmt.Errorf("session ended without exit status (%s)", o.DisconnectCause)
		}
		return fmt.Errorf("session ended without exit status")
	case *o.Code != 0:
		if o.DisconnectCause == causeRemoteSignal {
			return fmt.Errorf("remote process was terminated by signal")
		}
		return fmt.Errorf("exit status %d", *o.Code)
	default:
		return nil
	}
}

// exitResult holds the single terminal outcome of a session and broadcasts it to
// any number of observers through a closed channel. The first recorded outcome
// wins, so a late observer can never replace a real exit code with a
// synthesized one.
type exitResult struct {
	mu      sync.Mutex
	outcome ExitOutcome
	set     bool
	ready   chan struct{}
}

func newExitResult() *exitResult {
	return &exitResult{ready: make(chan struct{})}
}

// Set records the outcome once. Later calls are ignored.
func (e *exitResult) Set(outcome ExitOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		return
	}
	e.outcome = outcome
	e.set = true
	close(e.ready)
}

// Wait blocks until the outcome is recorded and returns it.
func (e *exitResult) Wait() ExitOutcome {
	<-e.ready
	return e.Get()
}

// Ready is closed once the outcome is recorded.
func (e *exitResult) Ready() <-chan struct{} {
	return e.ready
}

// Get returns the recorded outcome and whether it has been set.
func (e *exitResult) Get() ExitOutcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.outcome
}
