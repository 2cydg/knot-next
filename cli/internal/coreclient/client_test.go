package coreclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func runtimeFixture(t *testing.T, server *httptest.Server, instance string) string {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	path := filepath.Join(dir, "core.json")
	if err := os.WriteFile(token, []byte("private-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	b, _ := json.Marshal(Runtime{Version: "core-real", APIVersion: "v1", InstanceID: instance, Port: port, ListenAddresses: []string{u.Host}, TokenPath: token})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func jsonData(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func fixtureHandler(t *testing.T, instance func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing token header")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v1/runtime":
			jsonData(w, Runtime{APIVersion: "v1", InstanceID: instance()})
		case "/v1/version":
			jsonData(w, Version{APIVersion: "v1", Version: "actual-version"})
		case "/v1/health":
			jsonData(w, Health{Status: "degraded", Authenticated: true, Checks: []HealthCheck{{Name: "crypto", Status: "unavailable"}}})
		case "/v1/capabilities":
			jsonData(w, []Capability{{Name: "config", Available: true, Supported: true}, {Name: "agent", Available: false, Supported: true}})
		case "/v1/config":
			jsonData(w, Summary{Servers: []Server{{ID: "opaque:one", Alias: "sample"}}})
		default:
			http.NotFound(w, r)
		}
	}
}
func TestLoadAuthenticatesAndPreservesActualAvailability(t *testing.T) {
	server := httptest.NewServer(fixtureHandler(t, func() string { return "instance-a" }))
	defer server.Close()
	s, err := Load(context.Background(), runtimeFixture(t, server, "instance-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Client.Close()
	if s.Version.Version != "actual-version" || s.Health.Status != "degraded" || s.Available("agent") || !s.Available("config") || s.Config.Servers[0].ID != "opaque:one" {
		t.Fatalf("incorrect snapshot: %+v", s)
	}
}
func TestRestartDuringLoadRejectsMixedSnapshot(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(fixtureHandler(t, func() string {
		if calls.Add(1) > 1 {
			return "instance-b"
		}
		return "instance-a"
	}))
	defer server.Close()
	_, err := Load(context.Background(), runtimeFixture(t, server, "instance-a"))
	if Code(err) != "INSTANCE_CHANGED" {
		t.Fatalf("mixed snapshot accepted: %v", err)
	}
}
func TestDiscoveryLayoutAndInvalidRuntime(t *testing.T) {
	t.Setenv("KNOT_RUNTIME_FILE", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := RuntimeFile()
	if err != nil || path != filepath.Join(os.Getenv("XDG_STATE_HOME"), "knot", "runtime", "core.json") {
		t.Fatal(path, err)
	}
	t.Setenv("KNOT_RUNTIME_FILE", "relative")
	if _, err := RuntimeFile(); Code(err) != "INVALID_RUNTIME" {
		t.Fatal(err)
	}
	for _, base := range []string{"https://127.0.0.1:1", "http://localhost:1", "http://192.0.2.1:1", "http://127.0.0.1:1/path", "http://127.0.0.1:1?token=secret", "http://secret@127.0.0.1:1"} {
		if _, err := New(base, "token"); Code(err) != "INVALID_RUNTIME" {
			t.Fatal(base, err)
		}
	}
}
func TestProtocolErrorsAndFreshDTOReplacement(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			if calls.Add(1) == 1 {
				jsonData(w, Summary{Servers: []Server{{ID: "id", Alias: "a", ProxyID: "old", JumpHostIDs: []string{"jump"}}}})
			} else {
				jsonData(w, Summary{Servers: []Server{{ID: "id", Alias: "a"}}})
			}
		case "/v1/version":
			w.WriteHeader(401)
			_, _ = w.Write([]byte("not json"))
		case "/v1/health":
			_, _ = w.Write([]byte(`{"data":null}`))
		case "/v1/runtime":
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":{"code":"FUTURE_CODE","message":"private-token","retryable":true,"details":{"fields":["host"]}}}`))
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "private-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first, err := c.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Servers[0].ProxyID != "old" || second.Servers[0].ProxyID != "" || len(second.Servers[0].JumpHostIDs) != 0 {
		t.Fatal("omitted fields retained")
	}
	if _, err := c.Version(context.Background()); Code(err) != "UNAUTHORIZED" {
		t.Fatal(err)
	}
	if _, err := c.Health(context.Background()); Code(err) != "PROTOCOL_ERROR" {
		t.Fatal(err)
	}
	_, err = c.Runtime(context.Background())
	if Code(err) != "FUTURE_CODE" || strings.Contains(err.Error(), "private-token") {
		t.Fatal(err)
	}
	e := err.(*Error)
	if !e.Retryable || e.Status != 409 || len(e.Details) == 0 {
		t.Fatal("machine fields lost")
	}
}
func TestRedirectNeverForwardsCredentials(t *testing.T) {
	var seen atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Store(true) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer server.Close()
	c, _ := New(server.URL, "private-token")
	defer c.Close()
	if _, err := c.Version(context.Background()); err == nil || seen.Load() {
		t.Fatal("redirect followed")
	}
}
func TestResolveEscapesLiteralReference(t *testing.T) {
	ref := "alias []/?&+中文"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != ref {
			t.Errorf("reference changed: %q", r.URL.RawQuery)
		}
		jsonData(w, Server{ID: "persistent-id", Alias: ref})
	}))
	defer server.Close()
	c, _ := New(server.URL, "private-token")
	defer c.Close()
	s, err := c.Resolve(context.Background(), ref)
	if err != nil || s.ID != "persistent-id" {
		t.Fatal(s, err)
	}
}
func TestBoundedEventsCancelAndOpcodePreservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("WS token missing")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if r.URL.Path == "/v1/events" {
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"core.snapshot","api_version":"v1"}`))
			for i := 0; i < 100; i++ {
				if conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.cwd"}`)) != nil {
					return
				}
			}
			_, _, _ = conn.Read(r.Context())
			return
		}
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if conn.Write(r.Context(), kind, data) != nil {
				return
			}
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, "private-token")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	item := <-stream.Items
	if item.Err != nil || item.Event.Type != "core.snapshot" {
		t.Fatal(item)
	}
	closed := make(chan struct{})
	go func() { stream.Close(); close(closed) }()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("bounded queue prevented cancellation")
	}
	socket, err := c.dial(ctx, "/echo")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	for _, kind := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		payload := []byte{0, 13, 10, 27, 255}
		if err := socket.Write(ctx, kind, payload); err != nil {
			t.Fatal(err)
		}
		gotKind, got, err := socket.Read(ctx)
		if err != nil || gotKind != kind || string(got) != string(payload) {
			t.Fatalf("opcode/bytes changed: %v %v %v", gotKind, got, err)
		}
	}
}
