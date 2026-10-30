package directssh

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

type fixture struct {
	mu         sync.Mutex
	snapshot   coreclient.Snapshot
	path       string
	state      string
	failure    string
	instance   string
	agentFails bool
	requests   []coreclient.SessionRequest
	answers    []coreclient.ChallengeAnswer
	closed     int
	polls      int
	challenge  coreclient.Challenge
	attach     http.HandlerFunc
}

func newFixture(t *testing.T, state string) *fixture {
	t.Helper()
	f := &fixture{state: state, instance: "instance-a", challenge: coreclient.Challenge{ID: "displayed-fingerprint", SessionID: "session-a", Type: "host_key", Pending: true, Host: "example:22", ServerAlias: "production", KeyType: "ssh-ed25519", Fingerprint: "SHA256:test", ExpiresAt: time.Now().Add(time.Minute)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		attach := f.attach
		f.mu.Unlock()
		if r.URL.Path == "/v1/sessions/session-a/attach" && attach != nil {
			attach(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		switch r.URL.Path {
		case "/v1/runtime":
			data(coreclient.Runtime{InstanceID: f.instance, APIVersion: "v1"})
		case "/v1/version":
			data(coreclient.Version{Version: "test", APIVersion: "v1"})
		case "/v1/health":
			data(coreclient.Health{Status: "ok", Authenticated: true})
		case "/v1/capabilities":
			data([]coreclient.Capability{{Name: "config", Available: true}, {Name: "agent_forwarding", Available: true}})
		case "/v1/config":
			data(coreclient.Summary{})
		case "/v1/config/servers/resolve":
			if r.URL.Query().Get("ref") == "missing" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND"}}`)
				return
			}
			data(coreclient.Server{ID: "server-persistent", Alias: "production"})
		case "/v1/sessions":
			var request coreclient.SessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			f.requests = append(f.requests, request)
			if f.agentFails {
				f.state, f.failure = "connected", ""
				if request.ForwardAgent {
					f.state, f.failure = "failed", "agent_forwarding_unavailable"
				}
			}
			// Even a ready-looking creation response must be checked with GET.
			data(coreclient.Session{ID: "session-a", Alias: "production", State: "connected"})
		case "/v1/sessions/session-a":
			if r.Method == http.MethodDelete {
				f.closed++
				data(map[string]bool{"deleted": true})
				return
			}
			f.polls++
			data(coreclient.Session{ID: "session-a", Alias: "production", State: f.state, FrameworkCode: f.failure})
		case "/v1/sessions/session-a/challenges/host-key":
			if r.Method == http.MethodPost {
				var answer coreclient.ChallengeAnswer
				if err := json.NewDecoder(r.Body).Decode(&answer); err != nil {
					t.Error(err)
				}
				f.answers = append(f.answers, answer)
				f.state = "connected"
				data(map[string]bool{"accepted": true})
				return
			}
			data(f.challenge)
		case "/v1/sessions/session-a/challenges/auth":
			data(coreclient.Challenge{ID: "auth", SessionID: "session-a", Type: "auth", Pending: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := coreclient.New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	f.snapshot = coreclient.Snapshot{Client: client, Runtime: coreclient.Runtime{InstanceID: "instance-a"}, Capabilities: []coreclient.Capability{{Name: "agent_forwarding", Available: true}}}
	setTestHome(t, t.TempDir())
	f.path = server.URL
	if err := preferences.Save(preferences.Settings{Core: &preferences.Core{URL: server.URL, Secret: "token"}}); err != nil {
		t.Fatal(err)
	}

	return f
}

func TestConnectionFailuresExitWithoutConsoleAuthOrTUI(t *testing.T) {
	for _, tc := range []struct{ state, failure, alias, message string }{
		{"failed", "", "production", "SSH connection failed"},
		{"failed", "AUTHENTICATION_FAILED", "production", "SSH authentication failed"},
		{"auth_pending", "", "production", "SSH authentication failed"},
		{"unexpected", "", "production", "unsupported SSH session state"},
		{"failed", "", "missing", "Resource not found"},
	} {
		t.Run(tc.state+tc.failure+tc.alias, func(t *testing.T) {
			f := newFixture(t, tc.state)
			f.mu.Lock()
			f.failure = tc.failure
			f.mu.Unlock()
			t.Setenv("KNOT_RUNTIME_FILE", f.path)
			var out, diagnostic bytes.Buffer
			if code := Run(context.Background(), tc.alias, i18n.New(i18n.English), nil, &out, &diagnostic); code != entry.ExitFailure {
				t.Fatal("failure exit code", code)
			}
			if !strings.Contains(diagnostic.String(), tc.message) {
				t.Fatal("failure diagnostic missing", diagnostic.String())
			}
			if strings.ContainsAny(out.String()+diagnostic.String(), "┌┐└┘") || strings.Contains(out.String(), "\x1b[?1049") {
				t.Fatal("direct failure started a TUI")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if tc.alias != "missing" && (f.closed != 1 || f.polls != 1 || len(f.requests) != 1 || f.requests[0].AllowAuthRetry || f.requests[0].ServerRef != "server-persistent") {
				t.Fatal("failure retried, leaked its session or used an unresolved alias", f.closed, f.polls, f.requests)
			}
		})
	}
}

func TestHostKeyAnswerBindsDisplayedChallengeAndRejectsWithoutRetry(t *testing.T) {
	for _, accept := range []bool{false, true} {
		f := newFixture(t, "host_key_pending")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		attempt, session, err := connect(ctx, f.snapshot, coreclient.SessionRequest{ServerRef: "server-persistent", HostKeyPolicy: "ask"}, func(c coreclient.Challenge) (bool, error) {
			if c.ID != f.challenge.ID || c.Fingerprint != f.challenge.Fingerprint {
				t.Fatal("confirmation did not use the polled fingerprint")
			}
			return accept, nil
		})
		if accept {
			if err != nil || !session.Ready() {
				t.Fatal("accepted host did not connect", err)
			}
			attempt.Finish()
		} else if coreclient.Code(err) != "HOST_KEY_VERIFICATION_FAILED" {
			t.Fatal("rejection was not reported", err)
		}
		f.mu.Lock()
		if len(f.answers) != 1 || f.answers[0].ChallengeID != f.challenge.ID || f.answers[0].Accept != accept || f.answers[0].Remember != accept || len(f.requests) != 1 || (!accept && f.closed != 1) {
			t.Fatal("host answer identity, memory or cleanup", f.answers, f.requests, f.closed)
		}
		f.mu.Unlock()
	}
}

func TestAgentForwardingFallsBackOnceWithoutPrompt(t *testing.T) {
	f := newFixture(t, "connecting")
	f.mu.Lock()
	f.agentFails = true
	f.mu.Unlock()
	attempt, _, err := connect(context.Background(), f.snapshot, coreclient.SessionRequest{ServerRef: "server-persistent", ForwardAgent: true}, func(coreclient.Challenge) (bool, error) {
		t.Fatal("optional Agent fallback requested confirmation")
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt.Finish()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || !f.requests[0].ForwardAgent || f.requests[1].ForwardAgent || f.closed != 1 {
		t.Fatal("Agent fallback did not retry once with forwarding off", f.requests, f.closed)
	}
}

func TestConsoleHostConfirmationIsExplicitLocalizedAndEscaped(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, risk := range []string{"REMOTE_TRUST", "HIGH_RISK"} {
			tr := i18n.New(language)
			c := coreclient.Challenge{Risk: risk, Host: "host\x1b[2J", ServerAlias: "production", KeyType: "ssh-ed25519", Fingerprint: "SHA256:displayed"}
			for _, tc := range []struct {
				input  string
				accept bool
			}{{"yes\n", true}, {"y\nyes\n", true}, {"no\n", false}, {"\n", false}, {"", false}, {"yes", false}} {
				var out bytes.Buffer
				accepted, err := confirmHost(context.Background(), c, tr, strings.NewReader(tc.input), &out)
				if err != nil || accepted != tc.accept {
					t.Fatal("confirmation must require explicit yes", tc.input, accepted, err)
				}
				if !strings.Contains(out.String(), "SHA256:displayed") || !strings.Contains(out.String(), "ssh-ed25519") || !strings.Contains(out.String(), "yes/no") || strings.Contains(out.String(), "\x1b") {
					t.Fatal("console fingerprint omitted details or injected controls", out.String())
				}
				warning := "WARNING"
				if language == i18n.Chinese {
					warning = "警告"
				}
				if strings.Contains(out.String(), warning) != (risk == "HIGH_RISK") {
					t.Fatal("changed host key warning missing", out.String())
				}
			}
		}
	}
}

func TestLateConfirmationCannotAnswerANewCoreInstance(t *testing.T) {
	f := newFixture(t, "host_key_pending")
	_, _, err := connect(context.Background(), f.snapshot, coreclient.SessionRequest{ServerRef: "server-persistent"}, func(coreclient.Challenge) (bool, error) {
		f.mu.Lock()
		f.instance = "instance-b"
		f.mu.Unlock()
		return true, nil
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if coreclient.Code(err) != "INSTANCE_CHANGED" || len(f.answers) != 0 {
		t.Fatal("old fingerprint was accepted on a new core", err)
	}
}
