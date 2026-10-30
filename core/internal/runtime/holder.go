package runtime

import "sync"

// Holder carries instance identity and listener metadata from the lifecycle
// runner to the HTTP runtime endpoint and health checks. It never writes files.
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

// Set publishes instance information. It is safe to call from any goroutine.
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

// Clear drops published information so a stopped instance never serves stale addresses.
func (h *Holder) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.info = Info{}
	h.present = false
}
