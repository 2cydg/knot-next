package http

import "sync"

// ConnTracker tracks connections that were hijacked out of net/http's control,
// such as WebSocket attachments.
//
// http.Server.Shutdown neither closes nor waits for hijacked connections, so
// without a registry of its own a service teardown leaves WebSocket handlers,
// their relays, and their session attachments running after every other
// resource has been released. Teardown closes them through CloseAll.
type ConnTracker struct {
	mu     sync.Mutex
	conns  map[*websocketConn]struct{}
	closed bool
}

func NewConnTracker() *ConnTracker {
	return &ConnTracker{conns: make(map[*websocketConn]struct{})}
}

// accepting reports whether new connections may still be registered. Handlers
// check it before the upgrade so a connection arriving after teardown began is
// refused with a normal HTTP status instead of a 101 followed by a close.
func (t *ConnTracker) accepting() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed
}

// track registers a hijacked connection. It returns false once the tracker has
// been closed, in which case the caller must close the connection instead of
// serving it: teardown has already been through and will not revisit it.
func (t *ConnTracker) track(conn *websocketConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.conns[conn] = struct{}{}
	return true
}

func (t *ConnTracker) release(conn *websocketConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, conn)
}

// Count reports how many connections are currently tracked.
func (t *ConnTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// CloseAll closes every tracked connection and refuses later ones. It is the
// explicit WebSocket shutdown that http.Server.Shutdown does not provide.
func (t *ConnTracker) CloseAll() {
	t.mu.Lock()
	pending := make([]*websocketConn, 0, len(t.conns))
	for conn := range t.conns {
		pending = append(pending, conn)
	}
	t.conns = make(map[*websocketConn]struct{})
	t.closed = true
	t.mu.Unlock()

	for _, conn := range pending {
		_ = conn.Close()
	}
}
