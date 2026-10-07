package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrAttachmentClosed is returned by an attachment's input once the attachment
// no longer owns the session. It is a normal end of input, not a failure.
var ErrAttachmentClosed = errors.New("attachment is no longer active")

const (
	// attachOutputQueueDepth bounds how far one attachment's relay may fall
	// behind the pump. The pump itself owns the overflow signal.
	attachOutputQueueDepth = 16
	// attachInputQueueDepth bounds the input accepted from one attachment before
	// Write blocks. Backpressure stays with the client, which is the producer,
	// so a remote that stopped reading parks this attachment's worker alone.
	attachInputQueueDepth = 64
)

// attachmentGrace bounds how long revoking an attachment waits for its workers
// before the next attachment starts. Claim workers return on cancellation;
// the backend owns the one actual write that may still be stuck in transport.
// It is a variable so tests can shorten it.
var attachmentGrace = 2 * time.Second

// attachmentClaim is the single owner of a session's interactive streams: the
// output subscription, the exit waiter and the input worker. A session has at
// most one claim, and a claim that lost ownership retires before a new one
// starts, so two clients can never read the same pump or write the same stdin.
type attachmentClaim struct {
	gen     int64
	backend *interactiveBackend
	sub     *pumpSubscription
	input   *attachInput

	// backlogTruncated reports that the output handed to this claim is missing
	// its oldest bytes because the retained backlog overflowed.
	backlogTruncated bool

	ctx    context.Context
	cancel context.CancelFunc

	stdout chan []byte
	stderr chan []byte
	exit   chan ExitOutcome

	// revoked is closed when the claim loses ownership without the session
	// ending: an explicit detach, or a newer attachment. A session that ends is
	// reported through exit instead, so a normal completion is never mistaken for
	// a revocation.
	revoked    chan struct{}
	revokeOnce sync.Once

	// inputErr carries the first failure writing to the remote's stdin, so the
	// transport knows its input stopped rather than silently discarding it.
	inputErr     chan error
	inputErrOnce sync.Once

	workers sync.WaitGroup
	done    chan struct{}
}

// newAttachmentClaim takes the claim's output subscription and builds its
// channels. The previous claim must already be revoked: the pump hands the
// backlog to the subscriber that is registered when it is created, so the
// previous owner has to be gone first.
func newAttachmentClaim(gen int64, backend *interactiveBackend) *attachmentClaim {
	ctx, cancel := context.WithCancel(context.Background())
	sub, backlogTruncated := backend.pump.Subscribe()
	return &attachmentClaim{
		gen:              gen,
		backend:          backend,
		sub:              sub,
		input:            newAttachInput(ctx),
		backlogTruncated: backlogTruncated,
		ctx:              ctx,
		cancel:           cancel,
		stdout:           make(chan []byte, attachOutputQueueDepth),
		stderr:           make(chan []byte, attachOutputQueueDepth),
		exit:             make(chan ExitOutcome, 1),
		revoked:          make(chan struct{}),
		inputErr:         make(chan error, 1),
		done:             make(chan struct{}),
	}
}

// start begins the claim's workers. It is safe to call on a claim that was
// revoked before it started: the workers then return immediately.
func (c *attachmentClaim) start() {
	c.workers.Add(3)
	go c.relayOutput()
	go c.waitForExit()
	go c.runInput()
	go func() {
		c.workers.Wait()
		close(c.done)
	}()
}

// revoke ends the claim's ownership of the session. It is idempotent and never
// blocks: the output subscription is dropped here, so the pump has exactly one
// owner again as soon as this returns, and the workers return on their own.
func (c *attachmentClaim) revoke() {
	c.revokeOnce.Do(func() {
		close(c.revoked)
		c.backend.pump.Unsubscribe(c.sub)
		c.input.Close()
		c.cancel()
	})
}

// wait blocks until every worker has returned, or the grace expires. A false
// result means claim workers did not finish within the grace. Backend write
// serialization still prevents the next claim from starting an overlapping write.
func (c *attachmentClaim) wait(grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-c.done:
		return true
	case <-timer.C:
		return false
	}
}

// relayOutput moves pump output to this attachment's streams. It is the only
// reader the claim has: the pump owns the actual stream readers, so a re-attach
// never creates a second one.
func (c *attachmentClaim) relayOutput() {
	defer c.workers.Done()
	defer close(c.stdout)
	defer close(c.stderr)
	for {
		select {
		case <-c.ctx.Done():
			return
		case chunk, ok := <-c.sub.Chunks():
			if !ok {
				return
			}
			target := c.stdout
			if chunk.stream == streamStderr {
				target = c.stderr
			}
			select {
			case target <- chunk.data:
			case <-c.ctx.Done():
				return
			}
		}
	}
}

// waitForExit publishes the session's single terminal outcome, then closes so
// consumers see the end of the attachment. It selects on the outcome being
// recorded and on cancellation: a session whose shell lives for hours must not
// leave one waiter behind per cancelled attachment.
func (c *attachmentClaim) waitForExit() {
	defer c.workers.Done()
	defer close(c.exit)
	select {
	case <-c.backend.exitResult.Ready():
	case <-c.ctx.Done():
		return
	}
	outcome := c.backend.exitResult.Get()
	select {
	case c.exit <- outcome:
	case <-c.ctx.Done():
	}
}

// runInput delivers this attachment's queued input to the session's stdin. The
// client's own goroutine never performs the write, so a remote that stopped
// reading cannot pin it, and cancelling the attachment drops whatever is still
// queued instead of delivering it to the next owner.
//
// The backend serializes actual writes across attachments. Revoking a claim
// ends its wait, but a write already inside the transport retains the backend's
// slot until it returns. New input waits for that slot without spawning another
// writer. Bytes already handed to the transport may still arrive after detach;
// queued input from the revoked claim is discarded.
func (c *attachmentClaim) runInput() {
	defer c.workers.Done()
	for {
		select {
		case <-c.input.ctx.Done():
			return
		case <-c.backend.pump.Done():
			// The session's streams ended; there is nothing left to write to.
			return
		case chunk := <-c.input.queue:
			// A chunk that was still queued when the attachment lost ownership is
			// dropped rather than delivered to the next owner's session.
			if c.input.ctx.Err() != nil {
				return
			}
			slot := c.backend.stdinSlot()
			select {
			case slot <- struct{}{}:
			case <-c.input.ctx.Done():
				return
			case <-c.backend.pump.Done():
				return
			}
			if c.input.ctx.Err() != nil {
				<-slot
				return
			}
			stdin := c.backend.stdinWriter()
			if stdin == nil {
				<-slot
				return
			}
			written := make(chan error, 1)
			go writeStdin(stdin, chunk, written, slot)
			select {
			case err := <-written:
				if err != nil {
					c.reportInputError(err)
					return
				}
			case <-c.input.ctx.Done():
				return
			case <-c.backend.pump.Done():
				return
			}
		}
	}
}

// writeStdin performs one write on the helper goroutine and reports the result.
// It is a named function so the write can be told apart from the worker that
// owns it when both appear in a stack dump: a write still parked on a stalled
// remote is not an input owner.
func writeStdin(stdin io.Writer, chunk []byte, done chan<- error, slot chan struct{}) {
	defer func() { <-slot }()
	n, err := stdin.Write(chunk)
	if err == nil && n != len(chunk) {
		err = io.ErrShortWrite
	}
	done <- err
}

func (b *interactiveBackend) stdinSlot() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inputSlot == nil {
		b.inputSlot = make(chan struct{}, 1)
	}
	return b.inputSlot
}

func (c *attachmentClaim) reportInputError(err error) {
	c.inputErrOnce.Do(func() {
		c.inputErr <- err
		close(c.inputErr)
	})
}

// attachInput is the input side of one attachment. It queues writes for the
// attachment's worker and refuses them once the attachment is gone.
type attachInput struct {
	queue  chan []byte
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu     sync.RWMutex
	closed bool
}

func newAttachInput(parent context.Context) *attachInput {
	ctx, cancel := context.WithCancel(parent)
	return &attachInput{
		queue:  make(chan []byte, attachInputQueueDepth),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Write queues stdin bytes, blocking while the queue is full so backpressure
// reaches the client. It returns ErrAttachmentClosed once the attachment no
// longer owns the session.
func (in *attachInput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	in.mu.RLock()
	closed := in.closed
	in.mu.RUnlock()
	if closed {
		return 0, ErrAttachmentClosed
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	select {
	case in.queue <- chunk:
		return len(p), nil
	case <-in.ctx.Done():
		return 0, ErrAttachmentClosed
	}
}

// Close ends this attachment's input. It never closes the session's stdin: the
// shell keeps running and stays usable for the next attachment. It does not wait
// for a Write already parked on a full queue, so revoking an attachment cannot
// be pinned by a client that stopped sending.
func (in *attachInput) Close() error {
	in.once.Do(func() {
		in.mu.Lock()
		in.closed = true
		in.mu.Unlock()
		in.cancel()
	})
	return nil
}
