package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"knot-cli/internal/connection"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/terminal"
)

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

func TestSSHAgentFailureRequiresExplicitRetry(t *testing.T) {
	m, a := sshModel(t)
	m.ssh.ForwardAgent = true
	m.handleSSH(sshResult{Attempt: a, Result: connection.Result{Session: coreclient.Session{ID: "opaque", State: "failed", FrameworkCode: "agent_forwarding_unavailable"}}})
	if m.modal == nil || m.modal.Kind != ActionConfirm || m.handoff != nil || m.ssh.Attempt != a {
		t.Fatal("agent forwarding silently downgraded")
	}
	for _, key := range []string{"?", "c", "ctrl+s", "ctrl+c"} {
		m.Update(press(key))
		if m.ssh.Attempt != a || m.modal == nil {
			t.Fatal("retry confirmation leaked key")
		}
	}
	// A late completion from another attempt cannot replace the current one.
	other := connection.New(context.Background(), m.snapshot.Client, "other")
	defer other.Finish()
	m.Update(sshRetryMsg{Ref: "other", Attempt: other})
	if m.ssh.Attempt != a {
		t.Fatal("stale retry replaced current attempt")
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
