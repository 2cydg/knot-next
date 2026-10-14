package forward

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
)

func TestAutoFailureWithoutIDPublishesWarningWithoutRuntimeResource(t *testing.T) {
	for _, mode := range []string{"entropy", "collisions"} {
		t.Run(mode, func(t *testing.T) {
			pool := sshpool.NewPool()
			defer pool.Shutdown(context.Background())
			s := NewService(pool)
			defer s.Shutdown(context.Background())
			if mode == "entropy" {
				s.idGenerator = func() (string, error) { return "", errors.New("artificial entropy failure") }
			} else {
				s.idGenerator = func() (string, error) { return "collision", nil }
				s.entries["forward_collision"] = &resource{View: View{ID: "forward_collision", State: "active"}, done: make(chan struct{}), subs: map[chan Event]struct{}{}}
			}
			before := len(s.entries)
			events := make(chan Event, 1)
			s.OnEvent(func(event Event) { events <- event })
			s.recordAutoFailure("conn_authenticated", "srv", config.ForwardDefinition{ID: "rule_saved", Type: "dynamic", Bind: "127.0.0.1:0"})
			var event Event
			select {
			case event = <-events:
			case <-time.After(time.Second):
				t.Fatal("ID failure silently dropped its warning")
			}
			if event.Type != "forward.auto_failed" || event.AutoFailure == nil || event.AutoFailure.ConnectionID != "conn_authenticated" || event.AutoFailure.ServerID != "srv" || event.AutoFailure.DefinitionID != "rule_saved" || event.FailureCode != "AUTO_FORWARD_START_FAILED" {
				t.Fatal("warning lost its existing connection/definition identity", event)
			}
			if len(s.entries) != before || event.Forward.ID != "" || event.Forward.EventsURL != "" {
				t.Fatal("ID failure fabricated a runtime resource", event)
			}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			if _, exists := document["forward"]; exists {
				t.Fatal("warning exposes an invalid runtime View", string(raw))
			}
			if _, exists := document["auto_failure"]; !exists {
				t.Fatal("warning lacks definition identity", string(raw))
			}
		})
	}
}
