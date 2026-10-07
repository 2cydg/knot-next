package resourcepolicy

import "sync"

// Callbacks serializes advisory notifications outside business locks. A slow
// observer owns at most one goroutine and 256 pending notifications. Shutdown
// drops pending notifications; it does not wait for the observer, which may
// itself call Shutdown. Resource workers are waited independently.
type Callbacks struct {
	mu      sync.Mutex
	queue   []func()
	running bool
	closed  bool
}

func (c *Callbacks) Send(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.queue) >= 256 {
		return
	}
	c.queue = append(c.queue, fn)
	if !c.running {
		c.running = true
		go c.run()
	}
}
func (c *Callbacks) run() {
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		fn := c.queue[0]
		c.queue[0] = nil
		c.queue = c.queue[1:]
		c.mu.Unlock()
		func() { defer func() { _ = recover() }(); fn() }()
	}
}
func (c *Callbacks) Close() { c.mu.Lock(); c.closed = true; c.queue = nil; c.mu.Unlock() }
