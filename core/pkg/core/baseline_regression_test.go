package core

import (
	"testing"
	"time"

	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
)

func TestRegressionGlobalCredentialWarning(t *testing.T) {
	svc := New("audit", time.Now())
	ch, cancel, err := svc.SubscribeEvents()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer svc.events.Close()
	svc.handleSessionEvent(session.Event{Type: "session.warning", SessionID: "session", Warning: &session.Warning{Kind: session.WarningCredentialSaveFailed, Message: "password was not saved"}})
	got := <-ch
	if warning, ok := got.Data["warning"].(session.Warning); !ok || warning.Kind != session.WarningCredentialSaveFailed || got.Level != "warn" {
		t.Errorf("global session.warning lost warning detail: %+v", got.Data)
	}
}

func TestGlobalSFTPWarningRetainsDetails(t *testing.T) {
	svc := New("test", time.Now())
	ch, cancel, err := svc.SubscribeEvents()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer svc.events.Close()
	svc.handleSFTPEvent(sftp.Event{Type: "sftp.warning", SessionID: "closed-resource", Warning: &session.Warning{Kind: session.WarningCredentialSaveFailed, Message: "key was not saved"}})
	event := <-ch
	warning, ok := event.Data["warning"].(session.Warning)
	if !ok || warning.Message != "key was not saved" || event.Level != "warn" {
		t.Fatalf("global warning lost: %+v", event)
	}
}
