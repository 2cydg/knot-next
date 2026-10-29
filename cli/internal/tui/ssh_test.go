package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"knot-cli/internal/connection"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/terminal"
)

func TestSSHAuthAlwaysRemembersSuccessfulCredentials(t *testing.T) {
	for _, method := range []string{"password", "key", "agent"} {
		t.Run(method, func(t *testing.T) {
			answers := make(chan coreclient.ChallengeAnswer, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/runtime":
					_, _ = w.Write([]byte(`{"data":{"instance_id":"instance-a"}}`))
				case "/v1/sessions":
					_, _ = w.Write([]byte(`{"data":{"id":"session-a","state":"auth_pending"}}`))
				case "/v1/sessions/session-a/challenges/auth":
					var answer coreclient.ChallengeAnswer
					if err := json.NewDecoder(r.Body).Decode(&answer); err != nil {
						t.Error(err)
					}
					answers <- answer
					_, _ = w.Write([]byte(`{"data":{}}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			m := testModel(t, "servers")
			client, err := coreclient.New(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			m.snapshot.Client.Close()
			m.snapshot.Client = client
			attempt := connection.New(m.ctx, client, "instance-a")
			defer attempt.Finish()
			started := attempt.Start(coreclient.SessionRequest{ServerRef: "opaque-a"})
			if started.Err != nil {
				t.Fatal(started.Err)
			}
			m.ssh = &sshAttempt{Attempt: attempt, Session: started.Session}
			// Avoid a cleanup DELETE after the HTTP fixture has closed.
			defer func() { m.ssh = nil }()
			m.snapshot.Config.Keys[0].PrivateKeySet = true
			challenge := coreclient.Challenge{ID: "displayed-auth", Type: "auth", AllowedMethods: []string{method}, ServerAlias: "alpha"}
			m.ssh.Challenge = &challenge
			m.openSSHChallenge(challenge)
			if _, exists := m.modal.Form.Values()["remember"]; exists {
				t.Fatal("remember credentials control is still present")
			}
			setField(m.modal.Form, "password", "secret")
			setField(m.modal.Form, "passphrase", "temporary")
			cmd := m.submitForm()
			if cmd == nil {
				t.Fatal("authentication did not submit")
			}
			result, ok := cmd().(sshResult)
			if !ok || result.Result.Err != nil {
				t.Fatal("authentication request failed", result.Result.Err)
			}
			select {
			case answer := <-answers:
				if !answer.Remember || answer.ChallengeID != challenge.ID {
					t.Fatal("authentication did not remember the displayed challenge", answer)
				}
				if method == "password" && (answer.Password != "secret" || answer.Passphrase != "" || answer.KeyID != "") {
					t.Fatal("password answer sent unrelated key credentials")
				}
				if method == "key" && (answer.KeyID != "key" || answer.Passphrase != "temporary" || answer.Password != "") {
					t.Fatal("key answer sent incorrect credentials")
				}
				if method == "agent" && (answer.Password != "" || answer.KeyID != "" || answer.Passphrase != "") {
					t.Fatal("Agent answer sent unrelated secrets")
				}
			default:
				t.Fatal("no authentication request received")
			}
		})
	}
}

func TestSSHWaitsForExitDecisionBeforeHandoff(t *testing.T) {
	m, attempt := sshModel(t)
	m.Update(press("ctrl+c"))
	result := sshResult{Attempt: attempt, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "connected"}}}
	if cmd := m.handleSSH(result); cmd == nil || m.handoff != nil || m.exitConfirm == nil {
		t.Fatal("SSH bypassed the pending exit decision")
	}
	m.Update(press("esc"))
	m.handleSSH(result)
	if m.handoff == nil {
		t.Fatal("declining exit did not allow SSH handoff")
	}
}

func sshModel(t *testing.T) (*Model, *connection.Attempt) {
	t.Helper()
	m := testModel(t, "servers")
	a := connection.New(context.Background(), m.snapshot.Client, m.snapshot.Runtime.InstanceID)
	m.ssh = &sshAttempt{Attempt: a, Session: coreclient.Session{ID: "opaque", State: "connecting"}}
	t.Cleanup(func() { a.Finish(); m.ssh = nil; m.handoff = nil })
	return m, a
}

func TestSSHChallengePreservesSnapshotUntilUserAnswers(t *testing.T) {
	m, a := sshModel(t)
	c := &coreclient.Challenge{ID: "round1", SessionID: "opaque", Type: "host_key", Pending: true, ServerAlias: "alpha", Host: "example", KeyType: "ssh-ed25519", Fingerprint: "SHA256:old"}
	m.handleSSH(sshResult{Attempt: a, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "host_key_pending"}, Challenge: c}})
	if m.modal == nil || m.modal.Kind != HostTrust || !strings.Contains(m.modal.Body, "SHA256:old") {
		t.Fatal("missing explicit trust")
	}
	id := m.modal.ID
	for _, key := range []string{"?", "1", "ctrl+s", "ctrl+c"} {
		m.Update(press(key))
		if m.modal == nil || m.modal.ID != id || m.ssh == nil {
			t.Fatal("key escaped trust", key)
		}
	}
	m.handleSSH(sshResult{Attempt: a, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "failed", FailureCode: "HOST_KEY_CHALLENGE_EXPIRED"}}})
	if m.modal == nil || m.modal.ID != id || !strings.Contains(m.modal.Body, "SHA256:old") {
		t.Fatal("background expiry dismissed/replaced fingerprint")
	}
	m.Update(press("enter"))
	if m.modal != nil || m.ssh != nil || m.handoff != nil {
		t.Fatal("late acceptance reconnected or entered terminal")
	}
	if len(m.notices) == 0 || !strings.Contains(m.notices[len(m.notices)-1].Text, "expired") {
		t.Fatal("late acceptance was not precise expiry", m.notices)
	}
}

func TestSSHReadyHandoffAndLateResultsDoNotChangeNewAttempt(t *testing.T) {
	m, a := sshModel(t)
	m.handleSSH(sshResult{Attempt: a, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "connecting"}}})
	if m.handoff != nil {
		t.Fatal("connecting handed off")
	}
	other := connection.New(context.Background(), m.snapshot.Client, "i")
	defer other.Finish()
	m.handleSSH(sshResult{Attempt: other, Result: connection.Result{Session: coreclient.Session{ID: "other", State: "connected"}}})
	if m.handoff != nil {
		t.Fatal("stale result handed off")
	}
	m.Update(sshCancelMsg{Attempt: other})
	if m.ssh == nil || m.ssh.Attempt != a {
		t.Fatal("stale modal cancellation closed new attempt")
	}
	m.handleSSH(sshResult{Attempt: a, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "connected"}}})
	if m.handoff == nil || m.handoff.Session.ID != "opaque" {
		t.Fatal("real ready did not hand off")
	}
	// Process cancellation between renderer exit and bridge startup must still
	// cancel the owned attempt; pause(), rather than Close(), is used to hand off.
	m.Close()
	if coreclient.Code(a.Poll().Err) != "CANCELED" {
		t.Fatal("handoff bypassed process cleanup")
	}
}

func TestSSHAnswerInvalidatesPollFromPreviousChallenge(t *testing.T) {
	m, a := sshModel(t)
	c := &coreclient.Challenge{ID: "round1", Type: "host_key", Pending: true}
	m.ssh.Challenge = c
	m.openSSHChallenge(*c)
	_ = m.answerSSH("host-key", coreclient.ChallengeAnswer{ChallengeID: c.ID, Accept: true})
	revision := m.ssh.Revision
	if revision == 0 {
		t.Fatal("answer did not supersede pending poll")
	}
	m.handleSSH(sshResult{Attempt: a, Revision: revision, Answered: true})
	m.handleSSH(sshResult{Attempt: a, Revision: revision - 1, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "host_key_pending"}, Challenge: c}})
	if m.modal != nil || m.ssh.Challenge != nil {
		t.Fatal("old pending poll reopened answered challenge")
	}
}

func TestSSHAgentForwardingFollowsCoreAvailability(t *testing.T) {
	for _, available := range []bool{false, true} {
		m := testModel(t, "servers")
		m.snapshot.Capabilities = []coreclient.Capability{{Name: "agent_forwarding", Available: available}}
		m.startSSH("opaque", false)
		if m.ssh.ForwardAgent != available || m.modal != nil {
			t.Fatal("Agent availability did not control forwarding without a prompt")
		}
	}
}

func TestSSHAgentFailureRetriesOnceWithoutConfirmation(t *testing.T) {
	m, a := sshModel(t)
	m.snapshot.Capabilities = []coreclient.Capability{{Name: "agent_forwarding", Available: true}}
	m.ssh.ForwardAgent = true
	m.ssh.Ref, m.ssh.Resolve = "alias", true
	failure := sshResult{Attempt: a, Result: connection.Result{Session: coreclient.Session{ID: "opaque", ServerID: "server-id", State: "failed", FrameworkCode: "agent_forwarding_unavailable"}}}
	cmd := m.handleSSH(failure)
	if cmd == nil || m.modal != nil || m.handoff != nil || !m.ssh.Submitting || len(m.notices) != 0 {
		t.Fatal("Agent failure did not schedule a silent retry")
	}
	if m.ssh.Revision == 0 {
		t.Fatal("fallback did not invalidate outstanding polls")
	}
	if m.handleSSH(failure) != nil {
		t.Fatal("late failure scheduled a duplicate retry")
	}
	// A late completion from another attempt cannot replace the current one.
	other := connection.New(context.Background(), m.snapshot.Client, "other")
	defer other.Finish()
	m.Update(sshRetryMsg{Ref: "other", Attempt: other})
	if m.ssh.Attempt != a {
		t.Fatal("stale retry replaced current attempt")
	}
	retry, ok := cmd().(sshRetryMsg)
	if !ok || retry.Ref != "server-id" || retry.Resolve {
		t.Fatal("retry lost the resolved server identity")
	}
	m.Update(retry)
	if m.ssh == nil || m.ssh.Attempt == a || m.ssh.ForwardAgent || m.modal != nil {
		t.Fatal("retry did not disable forwarding")
	}
	// A failure even with forwarding disabled must terminate, not loop.
	next := m.ssh.Attempt
	t.Cleanup(func() { next.Finish() })
	m.handleSSH(sshResult{Attempt: next, Result: connection.Result{Session: failure.Result.Session}})
	if m.ssh != nil {
		t.Fatal("fallback retried indefinitely")
	}
}

func TestSSHAuthConditionalSecretAndEscapeCancelsImmediately(t *testing.T) {
	m, a := sshModel(t)
	c := coreclient.Challenge{ID: "auth1", SessionID: "opaque", Type: "auth", Pending: true, ServerAlias: "alpha", PassphraseRequired: true, AllowedMethods: []string{"password", "key", "agent"}}
	m.ssh.Challenge = &c
	m.openSSHChallenge(c)
	if m.modal.Form.Values()["method"] != "key" {
		t.Fatal("passphrase challenge did not default to key")
	}
	setField(m.modal.Form, "passphrase", "do-not-show")
	if strings.Contains(ansi.Strip(m.View().Content), "do-not-show") {
		t.Fatal("passphrase visible")
	}
	for i, f := range m.modal.Form.Fields {
		if f.Name == "password" && m.modal.Form.visible(i) {
			t.Fatal("unrelated password required")
		}
	}
	m.Update(press("esc"))
	if m.modal != nil || m.ssh != nil || m.handoff != nil {
		t.Fatal("auth escape entered draft flow")
	}
	_ = a
}

func TestAliasViewHasNoManagementAndExitResultRetainedInTUI(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		t.Run(string(language), func(t *testing.T) {
			m, a := sshModel(t)
			_ = a
			launch, _ := entry.Parse([]string{"ssh", "example"})
			m.launch = launch
			m.tr = i18n.New(language)
			m.directMessage = m.tr.Message("ssh.connecting")
			view := ansi.Strip(m.View().Content)
			if !strings.Contains(view, "example") || strings.Contains(view, m.tr.Message("ui.region_hints")) {
				t.Fatal("alias entered management", view)
			}
			code := 7
			m.showSSHResult(terminal.Result{ExitCode: &code}, nil, "opaque")
			if m.exitCode != 0 || !strings.Contains(m.notices[len(m.notices)-1].Text, "7") {
				t.Fatal("remote nonzero exit terminated management")
			}
		})
	}
}
