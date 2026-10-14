package forward

import (
	"context"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
	"net"
	"testing"
	"time"
)

func TestPruneSubscriberCancelAndDeleteIdempotency(t *testing.T) {
	pool := sshpool.NewPool()
	defer pool.Shutdown(context.Background())
	s := NewService(pool)
	now := time.Now()
	s.policy.Now = func() time.Time { return now }
	done := make(chan struct{})
	close(done)
	r := &resource{View: View{ID: "ended", State: "stopped", EndedAt: &now}, done: done, subs: map[chan Event]struct{}{}}
	s.entries[r.ID] = r
	ch, cancel, _, err := s.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Delete(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	now = now.Add(time.Second)
	second, err := s.Delete(context.Background(), r.ID)
	if err != nil || !first.EndedAt.Equal(*second.EndedAt) {
		t.Fatal("duplicate delete changed timestamp", err)
	}
	select {
	case e := <-ch:
		t.Fatal("duplicate deletion event", e)
	default:
	}
	now = now.Add(11 * time.Minute)
	s.List("", "")
	if _, ok := <-ch; ok {
		t.Fatal("history subscription not closed")
	}
	cancel()
	cancel() // Must never close the already-pruned channel twice.
}
func TestFlowFailureIsEventOnly(t *testing.T) {
	s := NewService(nil)
	r := &resource{View: View{ID: "active", State: "active"}, subs: map[chan Event]struct{}{}}
	ch := make(chan Event, 1)
	r.subs[ch] = struct{}{}
	s.flowFailure(r)
	event := <-ch
	if r.FailureCode != "" || r.Error != "" || event.FailureCode != "FORWARD_TARGET_FAILED" || event.Forward.State != "active" {
		t.Fatal(event, r.View)
	}
}
func TestAutoFailureRetainedAtCapacity(t *testing.T) {
	pool := sshpool.NewPool()
	defer pool.Shutdown(context.Background())
	s := NewService(pool)
	s.policy = resourcepolicy.Policy{Active: 1, History: 1, TTL: time.Hour, Now: time.Now}
	done := make(chan struct{})
	s.entries["active"] = &resource{View: View{ID: "active", State: "active"}, done: done, subs: map[chan Event]struct{}{}}
	now := time.Now()
	ended := make(chan struct{})
	close(ended)
	s.entries["old"] = &resource{View: View{ID: "old", State: "failed", EndedAt: &now}, done: ended, subs: map[chan Event]struct{}{}}
	s.recordAutoFailure("conn", "srv", config.ForwardDefinition{ID: "rule", Type: "dynamic", Bind: "127.0.0.1:0"})
	found := false
	for _, r := range s.List("", "") {
		if r.DefinitionID == "rule" && r.FailureCode == "AUTO_FORWARD_START_FAILED" {
			found = true
		}
	}
	if !found || len(s.entries) > 2 {
		t.Fatal("capacity hid automatic failure", s.List("", ""))
	}
}
func TestZeroMethodGreetingAndDialCodes(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = b.Write([]byte{5, 0}) }()
	available, err := readSocks5Greeting(a)
	if available || err != nil {
		t.Fatal(available, err)
	}
	if dialFailureCode(context.DeadlineExceeded) != 6 || dialFailureCode(&sshpool.AuthError{}) != 1 {
		t.Fatal("incorrect dial failure mapping")
	}
}
