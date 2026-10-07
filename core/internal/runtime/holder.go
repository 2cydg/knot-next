package runtime

import "sync"

// Holder carries the published runtime discovery information from the lifecycle
// runner to every reader that must agree with it: the HTTP runtime endpoint, the
// health checks, and the discovery file written to disk.
//
// Startup publishes the real values once the listeners exist, so the API never
// reports the zero-value placeholder it was constructed with.
type Holder struct {
	mu      sync.RWMutex
	info    Info
	present bool
}

func NewHolder() *Holder {
	return &Holder{}
}

// Set publishes discovery information. It is safe to call from any goroutine.
func (h *Holder) Set(info Info) {
	h.mu.Lock()
	defer h.mu.Unlock()
	info.ListenAddresses = append([]string(nil), info.ListenAddresses...)
	h.info = info
	h.present = true
}

// Get returns the published information and whether it has been published yet.
func (h *Holder) Get() (Info, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	info := h.info
	info.ListenAddresses = append([]string(nil), h.info.ListenAddresses...)
	return info, h.present
}

// Clear drops the published information. Teardown calls it once the discovery
// file is removed so a stopped instance never serves stale addresses.
func (h *Holder) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.info = Info{}
	h.present = false
}
