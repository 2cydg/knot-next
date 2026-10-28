package connection

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"knot-cli/internal/coreclient"
)

func TestAttemptWaitsBindsChallengeAndDoesNotRememberLocally(t *testing.T) {
	var mu sync.Mutex
	state, instance := "connecting", "i"
	var answer coreclient.ChallengeAnswer
	var created coreclient.SessionRequest
	var writes []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/events":
			w.WriteHeader(404)
		case r.URL.Path == "/v1/runtime":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"instance_id": instance}})
		case r.URL.Path == "/v1/sessions" && r.Method == "POST":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(201)
			io.WriteString(w, `{"data":{"id":"opaque","state":"connecting"}}`)
		case r.URL.Path == "/v1/sessions/opaque" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": coreclient.Session{ID: "opaque", State: state}})
		case r.URL.Path == "/v1/sessions/opaque/challenges/host-key" && r.Method == "GET":
			io.WriteString(w, `{"data":{"session_id":"opaque","challenge_id":"round1","type":"host_key","pending":true,"fingerprint":"SHA256:test"}}`)
		case r.URL.Path == "/v1/sessions/opaque/challenges/host-key" && r.Method == "POST":
			writes = append(writes, r.URL.Path)
			_ = json.NewDecoder(r.Body).Decode(&answer)
			state = "connecting"
			io.WriteString(w, `{"data":{}}`)
		default:
			writes = append(writes, r.Method+" "+r.URL.Path)
			io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer s.Close()
	c, _ := coreclient.New(s.URL, "token")
	defer c.Close()
	a := New(context.Background(), c, "i")
	defer a.Finish()
	r := a.Start(coreclient.SessionRequest{ServerRef: "persistent-id", ForwardAgent: true, AllowAuthRetry: true, HostKeyPolicy: "ask"})
	if r.Err != nil || r.Session.Ready() || a.ID() != "opaque" {
		t.Fatal("201 guessed ready", r)
	}
	mu.Lock()
	state = "host_key_pending"
	mu.Unlock()
	r = a.Poll()
	if r.Err != nil || r.Challenge == nil || r.Challenge.ID != "round1" {
		t.Fatal(r)
	}
	if err := a.Answer("host-key", coreclient.ChallengeAnswer{ChallengeID: r.Challenge.ID, Accept: true, Remember: true}); err != nil {
		t.Fatal(err)
	}
	r = a.Poll()
	if r.Session.Ready() {
		t.Fatal("answer guessed connected")
	}
	mu.Lock()
	state = "connected"
	mu.Unlock()
	r = a.Poll()
	if !r.Session.Ready() {
		t.Fatal(r)
	}
	mu.Lock()
	defer mu.Unlock()
	if answer.ChallengeID != "round1" || !answer.Remember || created.ServerRef != "persistent-id" || !created.ForwardAgent || len(writes) != 1 {
		t.Fatal("challenge or secret contract", answer, created, writes)
	}
}

func TestAttemptClosesAcceptedSessionAndGuardsNewInstance(t *testing.T) {
	var mu sync.Mutex
	instance := "i"
	deleted, answered := false, false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/events":
			w.WriteHeader(404)
		case r.URL.Path == "/v1/runtime":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"instance_id": instance}})
		case r.URL.Path == "/v1/sessions":
			io.WriteString(w, `{"data":{"id":"opaque","state":"connecting"}}`)
		case r.Method == "DELETE":
			deleted = true
			io.WriteString(w, `{"data":{}}`)
		default:
			answered = true
			io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer s.Close()
	c, _ := coreclient.New(s.URL, "token")
	defer c.Close()
	a := New(context.Background(), c, "i")
	if r := a.Start(coreclient.SessionRequest{}); r.Err != nil {
		t.Fatal(r.Err)
	}
	a.Close()
	mu.Lock()
	if !deleted {
		t.Fatal("accepted resource not deleted")
	}
	deleted = false
	mu.Unlock()
	a = New(context.Background(), c, "i")
	defer a.Finish()
	if r := a.Start(coreclient.SessionRequest{}); r.Err != nil {
		t.Fatal(r.Err)
	}
	mu.Lock()
	instance = "new-instance"
	mu.Unlock()
	if err := a.Answer("auth", coreclient.ChallengeAnswer{ChallengeID: "old"}); coreclient.Code(err) != "INSTANCE_CHANGED" {
		t.Fatal(err)
	}
	a.Close()
	mu.Lock()
	defer mu.Unlock()
	if deleted || answered {
		t.Fatal("old IDs sent to new instance")
	}
}

func TestAttemptBoundsWarningQueueAndJoinsObserver(t *testing.T) {
	warningsSent := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime" {
			io.WriteString(w, `{"data":{"instance_id":"i"}}`)
			return
		}
		if r.URL.Path == "/v1/sessions" {
			<-warningsSent
			io.WriteString(w, `{"data":{"id":"opaque","state":"connected"}}`)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"core.snapshot","api_version":"v1"}`))
		// These arrive before the POST response identifies the attempt. Failed
		// Agent attempts and other sessions must not become its exit warnings.
		for _, event := range []string{
			`{"type":"session.error","resource_id":"opaque","level":"warn","data":{"error":"agent forwarding unavailable: agent_unavailable"}}`,
			`{"type":"session.warning","resource_id":"other","level":"warn","data":{"warning":{"kind":"unrelated"}}}`,
			`{"type":"session.warning","level":"warn","data":{"warning":{"kind":"unscoped"}}}`,
			`{"type":"forward.failed","resource_id":"forward","level":"error"}`,
		} {
			_ = ws.Write(r.Context(), websocket.MessageText, []byte(event))
		}
		for range 40 {
			_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.warning","resource_id":"opaque","level":"warn","data":{"warning":{"kind":"remember_failed","message":"must-not-be-displayed"}}}`))
		}
		// Also send them after the retained warnings so queue eviction cannot
		// hide a failure to filter them.
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.error","resource_id":"opaque","level":"warn"}`))
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.warning","resource_id":"other","level":"warn","data":{"warning":{"kind":"unrelated"}}}`))
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.warning","level":"warn","data":{"session_id":"opaque","warning":{"kind":"last_warning"}}}`))
		close(warningsSent)
		_, _, _ = ws.Read(r.Context())
	}))
	defer s.Close()
	c, _ := coreclient.New(s.URL, "token")
	defer c.Close()
	a := New(context.Background(), c, "i")
	if r := a.Start(coreclient.SessionRequest{}); r.Err != nil {
		t.Fatal(r.Err)
	}
	<-warningsSent
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		drained := len(a.warnings) > 0 && a.warnings[len(a.warnings)-1] == "last_warning"
		a.mu.Unlock()
		if drained {
			break
		}
		time.Sleep(time.Millisecond)
	}
	warnings := a.Finish()
	if len(warnings) != 16 {
		t.Fatal("unbounded/lost warnings", len(warnings))
	}
	for i, code := range warnings {
		want := "remember_failed"
		if i == len(warnings)-1 {
			want = "last_warning"
		}
		if code != want {
			t.Fatal("backend text escaped into application queue")
		}
	}
}
