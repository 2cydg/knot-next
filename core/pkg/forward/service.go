// Package forward owns forwarding instances independently of SSH sessions.
package forward

import (
	"context"
	"errors"
	"fmt"
	"knot-core/internal/resourceid"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
	"net"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("forward not found")
var ErrConflict = errors.New("forward conflict")
var ErrValidation = errors.New("forward validation failed")

type Request struct {
	ServerID     string `json:"server_id"`
	ConnectionID string `json:"connection_id,omitempty"`
	Type         string `json:"type"`
	Bind         string `json:"bind"`
	Target       string `json:"target,omitempty"`
}
type View struct {
	ID           string     `json:"id"`
	ServerID     string     `json:"server_id"`
	ConnectionID string     `json:"connection_id"`
	Type         string     `json:"type"`
	Bind         string     `json:"bind"`
	Target       string     `json:"target,omitempty"`
	BoundAddress string     `json:"bound_address,omitempty"`
	Origin       string     `json:"origin"`
	DefinitionID string     `json:"definition_id,omitempty"`
	State        string     `json:"state"`
	FailureCode  string     `json:"failure_code,omitempty"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EventsURL    string     `json:"events_url"`
}
type Event struct {
	Type        string       `json:"type"`
	Forward     View         `json:"forward,omitzero"`
	AutoFailure *AutoFailure `json:"auto_failure,omitempty"`
	FailureCode string       `json:"failure_code,omitempty"`
	Error       string       `json:"error,omitempty"`
	Time        time.Time    `json:"time"`
}

// AutoFailure identifies a definition that could not receive a runtime ID.
// It is an advisory notification, not a queryable forwarding resource.
type AutoFailure struct {
	ServerID     string `json:"server_id"`
	ConnectionID string `json:"connection_id"`
	DefinitionID string `json:"definition_id"`
}
type resource struct {
	View
	cancel   context.CancelFunc
	done     chan struct{}
	listener net.Listener
	streams  map[net.Conn]struct{}
	subs     map[chan Event]struct{}
}
type Service struct {
	mu          sync.Mutex
	pool        *sshpool.Pool
	entries     map[string]*resource
	auto        map[string]bool
	ctx         context.Context
	cancel      context.CancelFunc
	stopped     bool
	policy      resourcepolicy.Policy
	workers     resourcepolicy.Group
	callbacks   resourcepolicy.Callbacks
	onEvent     func(Event)
	dialSlots   chan struct{}
	streamSlots chan struct{}
	idGenerator resourceid.Generator
}

func NewService(pool *sshpool.Pool) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{pool: pool, entries: map[string]*resource{}, auto: map[string]bool{}, ctx: ctx, cancel: cancel, policy: resourcepolicy.Sessions(), dialSlots: make(chan struct{}, 32), streamSlots: make(chan struct{}, 512)}
}
func (s *Service) OnEvent(fn func(Event)) { s.mu.Lock(); defer s.mu.Unlock(); s.onEvent = fn }
func (s *Service) emitLocked(r *resource, typ string) {
	event := Event{Type: typ, Forward: r.View, Time: s.policy.Now().UTC()}
	if typ == "forward.flow_failed" {
		event.FailureCode = "FORWARD_TARGET_FAILED"
		event.Error = "forward target could not be reached"
	}
	for ch := range r.subs {
		select {
		case ch <- event:
		default:
			close(ch)
			delete(r.subs, ch)
		}
	}
	s.publishAdvisoryLocked(event)
}

func (s *Service) publishAdvisoryLocked(event Event) {
	if fn := s.onEvent; fn != nil {
		s.callbacks.Send(func() { fn(event) })
	}
}
func (s *Service) pruneLocked() {
	var items []resourcepolicy.Item
	for id, r := range s.entries {
		if r.EndedAt != nil && isDone(r.done) {
			items = append(items, resourcepolicy.Item{ID: id, End: *r.EndedAt})
		}
	}
	for _, id := range s.policy.Expired(items) {
		for ch := range s.entries[id].subs {
			close(ch)
			delete(s.entries[id].subs, ch)
		}
		delete(s.entries, id)
	}
	for id := range s.auto {
		if _, err := s.pool.Connection(id); err != nil {
			delete(s.auto, id)
		}
	}
}
func isDone(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
func (s *Service) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.entries {
		if r.State == "active" {
			n++
		}
	}
	return n
}
func (s *Service) List(serverID, connectionID string) []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	out := []View{}
	for _, r := range s.entries {
		if (serverID == "" || r.ServerID == serverID) && (connectionID == "" || r.ConnectionID == connectionID) {
			out = append(out, r.View)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
func (s *Service) Get(id string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	r := s.entries[id]
	if r == nil {
		return View{}, ErrNotFound
	}
	return r.View, nil
}
func (s *Service) Create(req Request) (View, error) { return s.create(req, "temporary", "") }
func (s *Service) create(req Request, origin, definitionID string) (View, error) {
	if req.ServerID == "" {
		return View{}, fmt.Errorf("%w: server_id is required", ErrValidation)
	}
	if err := config.ValidateForward(config.ForwardDefinition{Type: req.Type, Bind: req.Bind, Target: req.Target}); err != nil {
		return View{}, fmt.Errorf("%w: %s", ErrValidation, err)
	}
	lease, err := s.pool.AcquireConnection(req.ConnectionID, req.ServerID)
	if err != nil {
		return View{}, fmt.Errorf("%w: %s", ErrConflict, err)
	}
	s.mu.Lock()
	s.pruneLocked()
	n := 0
	for _, r := range s.entries {
		if !isDone(r.done) {
			n++
		}
	}
	if s.stopped || n >= s.policy.Active {
		s.mu.Unlock()
		lease.Release()
		return View{}, fmt.Errorf("%w: forwarding capacity reached", ErrConflict)
	}
	id, err := resourceid.Allocate("forward", s.idGenerator, func(id string) bool { return s.entries[id] != nil })
	if err != nil {
		s.mu.Unlock()
		lease.Release()
		return View{}, err
	}
	now := s.policy.Now().UTC()
	r := &resource{View: View{ID: id, ServerID: req.ServerID, ConnectionID: lease.ConnectionID, Type: req.Type, Bind: req.Bind, Target: req.Target, Origin: origin, DefinitionID: definitionID, State: "starting", CreatedAt: now, UpdatedAt: now, EventsURL: "/v1/forwards/" + id + "/events"}, subs: map[chan Event]struct{}{}}
	s.entries[id] = r
	s.startLocked(r, lease)
	out := r.View
	s.mu.Unlock()
	return out, nil
}
func (s *Service) startLocked(r *resource, lease *sshpool.ClientLease) {
	ctx, cancel := context.WithCancel(s.ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	r.streams = map[net.Conn]struct{}{}
	r.State = "starting"
	r.Error = ""
	r.FailureCode = ""
	r.EndedAt = nil
	r.UpdatedAt = s.policy.Now().UTC()
	s.workers.Add()
	s.emitLocked(r, "forward.starting")
	go func() { defer s.workers.Done(); s.run(ctx, r, lease) }()
}
func (s *Service) Start(id string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.entries[id]
	if r == nil {
		return View{}, ErrNotFound
	}
	if s.stopped || r.State == "deleted" || !isDone(r.done) {
		return View{}, ErrConflict
	}
	n := 0
	for _, entry := range s.entries {
		if !isDone(entry.done) {
			n++
		}
	}
	if n >= s.policy.Active {
		return View{}, ErrConflict
	}
	lease, err := s.pool.AcquireConnection(r.ConnectionID, r.ServerID)
	if err != nil {
		return View{}, fmt.Errorf("%w: connection unavailable", ErrConflict)
	}
	s.startLocked(r, lease)
	return r.View, nil
}
func (s *Service) Stop(ctx context.Context, id string) (View, error) {
	s.mu.Lock()
	r := s.entries[id]
	if r == nil {
		s.mu.Unlock()
		return View{}, ErrNotFound
	}
	done := r.done
	if !isDone(done) {
		r.State = "stopping"
		r.UpdatedAt = s.policy.Now().UTC()
		r.cancel()
		s.emitLocked(r, "forward.stopping")
	}
	s.mu.Unlock()
	select {
	case <-done:
		return s.Get(id)
	case <-ctx.Done():
		view, _ := s.Get(id)
		return view, ctx.Err()
	}
}
func (s *Service) Delete(ctx context.Context, id string) (View, error) {
	if _, err := s.Stop(ctx, id); err != nil {
		return View{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.entries[id]
	if r == nil {
		return View{}, ErrNotFound
	}
	if r.State == "deleted" {
		return r.View, nil
	}
	r.State = "deleted"
	now := s.policy.Now().UTC()
	r.EndedAt = &now
	r.UpdatedAt = now
	s.emitLocked(r, "forward.deleted")
	return r.View, nil
}
func (s *Service) Subscribe(id string) (<-chan Event, func(), View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	r := s.entries[id]
	if r == nil {
		return nil, nil, View{}, ErrNotFound
	}
	if len(r.subs) >= 16 {
		return nil, nil, View{}, ErrConflict
	}
	ch := make(chan Event, 32)
	r.subs[ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
	return ch, cancel, r.View, nil
}

// EnsureAutoForwards is a required, synchronous registration hook. The first
// target acquisition fixes this transport's definition snapshot; startup itself
// is owned asynchronously. It never relies on advisory callback delivery.
func (s *Service) EnsureAutoForwards(connectionID, serverID string, defs []config.ForwardDefinition) {
	s.mu.Lock()
	if s.stopped || s.auto[connectionID] {
		s.mu.Unlock()
		return
	}
	s.auto[connectionID] = true
	s.workers.Add()
	s.mu.Unlock()
	defer s.workers.Done()
	for _, d := range defs {
		if !d.Enabled {
			continue
		}
		_, err := s.create(Request{ServerID: serverID, ConnectionID: connectionID, Type: d.Type, Bind: d.Bind, Target: d.Target}, "persistent", d.ID)
		if err != nil {
			s.recordAutoFailure(connectionID, serverID, d)
		}
	}
}
func (s *Service) recordAutoFailure(connectionID, serverID string, d config.ForwardDefinition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.pruneLocked()

	id, err := resourceid.Allocate("forward", s.idGenerator, func(id string) bool { return s.entries[id] != nil })
	now := s.policy.Now().UTC()
	if err != nil {
		// Without an allocated ID there is no runtime resource or events URL.
		s.publishAdvisoryLocked(Event{
			Type:        "forward.auto_failed",
			AutoFailure: &AutoFailure{ServerID: serverID, ConnectionID: connectionID, DefinitionID: d.ID},
			FailureCode: "AUTO_FORWARD_START_FAILED",
			Error:       "automatic forwarding could not allocate a resource identity",
			Time:        now,
		})
		return
	}
	done := make(chan struct{})
	close(done)
	r := &resource{View: View{ID: id, ServerID: serverID, ConnectionID: connectionID, Type: d.Type, Bind: d.Bind, Target: d.Target, Origin: "persistent", DefinitionID: d.ID, State: "failed", FailureCode: "AUTO_FORWARD_START_FAILED", Error: "automatic forwarding could not start", CreatedAt: now, UpdatedAt: now, EndedAt: &now, EventsURL: "/v1/forwards/" + id + "/events"}, done: done, subs: map[chan Event]struct{}{}}
	s.entries[id] = r
	s.emitLocked(r, "forward.failed")
	s.pruneLocked()
}
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	for _, r := range s.entries {
		if r.cancel != nil {
			if !isDone(r.done) {
				r.State = "stopping"
				r.UpdatedAt = s.policy.Now().UTC()
			}
			r.cancel()
		}
		for ch := range r.subs {
			close(ch)
			delete(r.subs, ch)
		}
	}
	s.callbacks.Close()
	s.mu.Unlock()
	return s.workers.Wait(ctx)
}
