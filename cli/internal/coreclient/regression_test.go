package coreclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type rejectingTransport struct{}

func (rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("ambient transport must not be used")
}

func TestClientDoesNotDependOnGlobalTransport(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = rejectingTransport{}
	t.Cleanup(func() { http.DefaultTransport = old })
	server := httptest.NewServer(fixtureHandler(t, func() string { return "instance" }))
	defer server.Close()
	c, err := New(server.URL, "private-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatal("client used ambient transport", err)
	}
}

func TestFailedLoadReturnsNoUsableSnapshot(t *testing.T) {
	for _, code := range []string{"PROTOCOL_ERROR", "UNAUTHORIZED"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == "UNAUTHORIZED" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"data":{}}`))
		}))
		s, err := Load(context.Background(), server.URL, "private-token")
		server.Close()
		if err == nil || s.Client != nil || s.Runtime.InstanceID != "" || len(s.Config.Servers) != 0 {
			t.Fatal("failed load exposed a partial or closed snapshot", s, err)
		}
	}
}

func TestOldRuntimeFilesAreIgnored(t *testing.T) {
	t.Setenv("KNOT_RUNTIME_FILE", filepath.Join(t.TempDir(), "core.json"))
	t.Setenv("XDG_STATE_HOME", "unusable-state")
	server := httptest.NewServer(fixtureHandler(t, func() string { return "instance" }))
	defer server.Close()
	snapshot, err := Load(context.Background(), server.URL, "private-token")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Client.Close()
}

func TestIdleEventStreamCancellationClosesReaderAndSocket(t *testing.T) {
	peerClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"core.snapshot","api_version":"v1"}`))
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.warning","level":"warn","data":{"warning":{"kind":"credential_save_failed"}}}`))
		_, _, _ = conn.Read(r.Context())
		close(peerClosed)
	}))
	defer server.Close()
	c, _ := New(server.URL, "token")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	select {
	case item := <-stream.Items:
		if item.Err != nil || item.Event.Type != "core.snapshot" {
			t.Fatal(item)
		}
	case <-ctx.Done():
		t.Fatal("missing handshake")
	}
	select {
	case item := <-stream.Items:
		if item.Err != nil || item.Event.Level != "warn" || item.Event.Type != "session.warning" {
			t.Fatal("core warning frame was lost", item)
		}
	case <-ctx.Done():
		t.Fatal("missing core warning frame")
	}
	cancel()
	select {
	case _, ok := <-stream.Items:
		if ok {
			t.Fatal("cancellation should close queue without a protocol error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle event reader did not stop")
	}
	select {
	case <-peerClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("idle socket was not closed")
	}
}
