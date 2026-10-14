package sshpool

import (
	"net"
	"sync/atomic"
	"time"
)

// promptConn retains original bytes read while the key-exchange callback is
// parked. The SSH handshake read loop is waiting for that callback, so this is
// its only reader until stop joins the observer. Memory is bounded to 64 KiB.
type promptConn struct {
	net.Conn
	buffered []byte
}

func (c *promptConn) Read(p []byte) (int, error) {
	if len(c.buffered) > 0 {
		n := copy(p, c.buffered)
		c.buffered = c.buffered[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
func (c *promptConn) observe() (<-chan struct{}, func()) {
	// SSH channel transports cannot interrupt reads through deadlines. They retain
	// the documented prompt deadline boundary without starting an unjoinable read.
	if err := c.Conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, func() {}
	}
	disconnected := make(chan struct{})
	joined := make(chan struct{})
	var stopping atomic.Bool
	go func() {
		defer close(joined)
		for len(c.buffered) < 64<<10 {
			buf := make([]byte, (64<<10)-len(c.buffered))
			n, err := c.Conn.Read(buf)
			c.buffered = append(c.buffered, buf[:n]...)
			if err != nil {
				timeout, ok := err.(net.Error)
				if !stopping.Load() || !ok || !timeout.Timeout() {
					close(disconnected)
				}
				return
			}
		}
		// At capacity, observing stops without claiming a disconnect. Reading
		// further would require unbounded storage or discard handshake bytes;
		// a later peer close is discovered only when the handshake resumes.
	}()
	return disconnected, func() {
		stopping.Store(true)
		_ = c.Conn.SetReadDeadline(time.Now())
		<-joined
		_ = c.Conn.SetReadDeadline(time.Time{})
	}
}
