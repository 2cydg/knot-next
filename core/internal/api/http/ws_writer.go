package http

import (
	"sync"
	"sync/atomic"
	"time"
)

// wsFrame is one outbound WebSocket frame.
type wsFrame struct {
	opcode  int
	payload []byte
}

// wsFrameWriter serializes every frame sent on one WebSocket connection through
// a single goroutine and a bounded queue.
//
// Serializing keeps frames from interleaving (a PTY stream must not be spliced
// into the middle of a JSON event), and bounding the queue keeps a client that
// stopped reading from growing server memory without limit: enqueue reports the
// overflow so the caller can end the attachment explicitly instead of dropping
// bytes silently.
type wsFrameWriter struct {
	conn   *websocketConn
	queue  chan wsFrame
	done   chan struct{}
	failed atomic.Bool

	// mu guards sends against the queue being closed, so enqueue can never send
	// on a closed channel.
	mu      sync.Mutex
	stopped bool
	once    sync.Once
}

func newWSFrameWriter(conn *websocketConn, depth int) *wsFrameWriter {
	w := &wsFrameWriter{
		conn:  conn,
		queue: make(chan wsFrame, depth),
		done:  make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *wsFrameWriter) run() {
	defer close(w.done)
	for frame := range w.queue {
		if err := w.write(frame); err != nil {
			// The client is gone or too slow. Close the connection so the reader
			// loop and any in-flight write return promptly, and mark the writer
			// failed so producers stop queueing frames nobody will receive.
			w.failed.Store(true)
			_ = w.conn.Close()
			return
		}
	}
}

func (w *wsFrameWriter) write(frame wsFrame) error {
	return w.conn.WriteFrame(frame.opcode, frame.payload)
}

// enqueue queues a frame, returning false when the client is not keeping up, the
// writer already failed, or the queue is closed.
func (w *wsFrameWriter) enqueue(opcode int, payload []byte) bool {
	if w.failed.Load() {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	select {
	case w.queue <- wsFrame{opcode: opcode, payload: payload}:
		return true
	default:
		return false
	}
}

// close ends the queue so the writer flushes what is already queued and exits.
func (w *wsFrameWriter) close() {
	w.once.Do(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.stopped = true
		close(w.queue)
	})
}

// wait blocks until the writer has flushed and exited, or the timeout elapses.
func (w *wsFrameWriter) wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		return false
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
