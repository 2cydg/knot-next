package coreclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
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
	for _, code := range []string{"OFFLINE", "UNAUTHORIZED"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == "UNAUTHORIZED" {
				w.WriteHeader(401)
				return
			}
			fixtureHandler(t, func() string { return "restarted" })(w, r)
		}))
		s, err := Load(context.Background(), runtimeFixture(t, server, "original"))
		server.Close()
		if err == nil || s.Client != nil || s.Runtime.InstanceID != "" || len(s.Config.Servers) != 0 {
			t.Fatal("failed load exposed a partial or closed snapshot", s, err)
		}
	}
}

func TestRuntimeFileUsesCoreHomeFallbackAndRelativeXDG(t *testing.T) {
	home := t.TempDir()
	getenv := func(string) string { return "" }
	path, err := runtimeFile(getenv, func() (string, error) { return "", errors.New("no home environment") }, func() (*user.User, error) { return &user.User{HomeDir: home}, nil })
	if err != nil || path != filepath.Join(home, ".local", "state", "knot", "runtime", "core.json") {
		t.Fatal("user.Current fallback differs from core", path, err)
	}
	t.Setenv("KNOT_RUNTIME_FILE", "")
	t.Setenv("XDG_STATE_HOME", "relative-state")
	path, err = RuntimeFile()
	want, _ := filepath.Abs(filepath.Join("relative-state", "knot", "runtime", "core.json"))
	if err != nil || path != want {
		t.Fatal("relative XDG layout differs from core", path, err)
	}
}

func TestRelativeRuntimeTokenIsAnchoredToDiscoveredFile(t *testing.T) {
	server := httptest.NewServer(fixtureHandler(t, func() string { return "instance" }))
	defer server.Close()
	path := runtimeFixture(t, server, "instance")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var runtime Runtime
	if err := json.Unmarshal(data, &runtime); err != nil {
		t.Fatal(err)
	}
	runtime.RuntimePath = filepath.Join("other-working-directory", "state", "knot", "runtime", "core.json")
	runtime.TokenPath = filepath.Join(filepath.Dir(runtime.RuntimePath), "token")
	write := func() {
		data, _ := json.Marshal(runtime)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	s, err := Load(context.Background(), path)
	if err != nil {
		t.Fatal("relative core paths not resolved", err)
	}
	s.Client.Close()
	runtime.TokenPath = filepath.Join("elsewhere", "token")
	write()
	if _, _, err := Discover(path); Code(err) != "INVALID_RUNTIME" {
		t.Fatal("unrelated relative token path accepted", err)
	}
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
