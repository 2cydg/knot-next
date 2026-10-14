package core

import (
	"testing"
	"time"

	"knot-core/pkg/forward"
	"knot-core/pkg/session"
)

func TestAutoFailureGlobalEventUsesExistingConnectionIdentity(t *testing.T) {
	s := New("fixture", time.Now())
	ch, cancel, err := s.events.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	s.handleForwardEvent(forward.Event{
		Type:        "forward.auto_failed",
		AutoFailure: &forward.AutoFailure{ServerID: "srv", ConnectionID: "conn_authenticated", DefinitionID: "rule_saved"},
		FailureCode: "AUTO_FORWARD_START_FAILED", Error: "automatic forwarding could not allocate a resource identity",
	})
	select {
	case event := <-ch:
		if event.Resource != "connection" || event.ResourceID != "conn_authenticated" || event.Level != "warn" || event.Data["definition_id"] != "rule_saved" || event.Data["failure_code"] != "AUTO_FORWARD_START_FAILED" {
			t.Fatal("global warning fabricated a forward identity", event)
		}
		warning, ok := event.Data["warning"].(session.Warning)
		if !ok || warning.Kind != "auto_forward_failed" {
			t.Fatal("global warning missing", event)
		}
		if _, exists := event.Data["events_url"]; exists {
			t.Fatal("unqueryable warning has events URL", event)
		}
	case <-time.After(time.Second):
		t.Fatal("global warning not delivered")
	}
}
