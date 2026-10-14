package sshpool

import (
	"errors"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
	"sort"
	"time"
)

var ErrConnectionUnavailable = errors.New("connection is unavailable or belongs to a different server")

// Connection describes an authenticated transport without exposing pool keys
// or credential identity digests. Target distinguishes business connections
// from transports currently used only as intermediate jumps.
type Connection struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"server_id"`
	Alias     string    `json:"alias"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	Sequence  uint64    `json:"sequence"`
	Target    bool      `json:"target"`
	Route     []string  `json:"route,omitempty"`
}

func connectionView(e *entry) Connection {
	c := Connection{ID: e.id, ServerID: e.serverID, Alias: e.alias, Host: e.remoteHost, Port: e.port, User: e.user, CreatedAt: e.createdAt, Sequence: e.sequence, Target: e.target}
	for _, parent := range e.parents {
		c.Route = append(c.Route, parent.id)
	}
	return c
}
func (p *Pool) Connections(serverID string) []Connection {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Connection, 0, len(p.entries))
	for _, e := range p.entries {
		if serverID == "" || e.serverID == serverID {
			out = append(out, connectionView(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}
func (p *Pool) Connection(id string) (Connection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.id == id {
			return connectionView(e), nil
		}
	}
	return Connection{}, ErrConnectionUnavailable
}

// AcquireConnection retains the exact transport and every parent atomically.
// An empty id selects the newest target for serverID, by publication sequence.
func (p *Pool) AcquireConnection(id, serverID string) (*ClientLease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrConnectionUnavailable
	}
	var target *entry
	for _, e := range p.entries {
		if (id == "" && !e.target) || e.serverID != serverID || transportEnded(e) {
			continue
		}
		if id != "" {
			if e.id == id {
				target = e
				break
			}
		} else if target == nil || e.sequence > target.sequence {
			target = e
		}
	}
	if target == nil {
		return nil, ErrConnectionUnavailable
	}
	entries := append(append([]*entry(nil), target.parents...), target)
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if transportEnded(e) {
			return nil, ErrConnectionUnavailable
		}
		key := ""
		for k, current := range p.entries {
			if current == e {
				key = k
				break
			}
		}
		if key == "" {
			return nil, ErrConnectionUnavailable
		}
		keys = append(keys, key)
	}
	release := p.retainPrefixLocked(entries)
	return &ClientLease{Client: target.client, Done: target.done, ConnectionID: target.id, Keys: keys, entries: entries, releases: []func(){release}}, nil
}

// UseTargetHook installs required target-acquisition work before the pool is used.
func (p *Pool) UseTargetHook(hook func(string, string, []config.ForwardDefinition)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targetHook = hook
}

func transportEnded(e *entry) bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Recently retired IDs remain reserved so an entropy collision cannot silently
// rebind a frontend's retained resource to an immediately recreated transport.
func (p *Pool) reserveClosedIDsLocked(entries map[string]*entry) {
	if p.reservedIDs == nil {
		p.reservedIDs = map[string]time.Time{}
	}
	for _, e := range entries {
		if e.id != "" {
			p.reservedIDs[e.id] = time.Now().UTC()
		}
	}
}
func (p *Pool) pruneIDsLocked() {
	live := map[string]bool{}
	for _, e := range p.entries {
		live[e.id] = true
	}
	var closed []resourcepolicy.Item
	for id, end := range p.reservedIDs {
		if !live[id] {
			closed = append(closed, resourcepolicy.Item{ID: id, End: end})
		}
	}
	policy := resourcepolicy.Policy{History: 4096, TTL: 10 * time.Minute, Now: time.Now}
	for _, id := range policy.Expired(closed) {
		delete(p.reservedIDs, id)
	}
}
