package http

import (
	"context"
	"sync"
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
// into the middle of a JSON event). Producers wait for space in the bounded
// queue while the connection's write deadline limits a stalled client.
type wsFrameWriter struct {
	conn    *websocketConn
	queue   chan wsFrame
	closing chan struct{}
	done    chan struct{}

	// mu guards sends against the queue being closed, so enqueue can never send
	// on a closed channel.
	mu   sync.RWMutex
	once sync.Once
}

func newWSFrameWriter(conn *websocketConn, depth int) *wsFrameWriter {
	w := &wsFrameWriter{
		conn:    conn,
		queue:   make(chan wsFrame, depth),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *wsFrameWriter) run() {
	defer close(w.done)
	for frame := range w.queue {
		if err := w.write(frame); err != nil {
			// The client is gone or too slow. Close the connection so the reader
			// loop returns promptly. The done signal wakes waiting producers.
			_ = w.conn.Close()
			return
		}
	}
}

func (w *wsFrameWriter) write(frame wsFrame) error {
	return w.conn.WriteFrame(frame.opcode, frame.payload)
}

// enqueue applies backpressure until the writer can accept the frame.
func (w *wsFrameWriter) enqueue(ctx context.Context, opcode int, payload []byte) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if isClosed(w.closing) || isClosed(w.done) {
		return false
	}
	select {
	case w.queue <- wsFrame{opcode: opcode, payload: payload}:
		return true
	case <-ctx.Done():
		return false
	case <-w.closing:
		return false
	case <-w.done:
		return false
	}
}

// tryEnqueue hands off an abort diagnostic without delaying teardown.
func (w *wsFrameWriter) tryEnqueue(opcode int, payload []byte) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if isClosed(w.closing) || isClosed(w.done) {
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
		close(w.closing)
		w.mu.Lock()
		defer w.mu.Unlock()
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
