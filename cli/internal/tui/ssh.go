package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"knot-cli/internal/connection"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/terminal"
)

type sshAttempt struct {
	Attempt      *connection.Attempt
	Session      coreclient.Session
	Challenge    *coreclient.Challenge
	Failure      error
	Submitting   bool
	Revision     uint64
	Ref          string
	Resolve      bool
	ForwardAgent bool
}
type sshResult struct {
	Attempt  *connection.Attempt
	Result   connection.Result
	Answered bool
	Revision uint64
}
type sshPollMsg struct {
	Attempt  *connection.Attempt
	Revision uint64
}
type sshCanceled struct{ Warnings []string }
type aliasFinishedMsg struct{}
type sshRetryMsg struct {
	Ref     string
	Resolve bool
	Attempt *connection.Attempt
}

func (m *Model) startSSH(ref string, resolve bool, agent ...bool) tea.Cmd {
	if m.ssh != nil {
		return nil
	}
	client, instance := m.snapshot.Client, m.snapshot.Runtime.InstanceID
	a := connection.New(m.ctx, client, instance)
	forwardAgent := (len(agent) == 0 || agent[0]) && m.snapshot.Available("agent_forwarding")
	m.ssh = &sshAttempt{Attempt: a, Ref: ref, Resolve: resolve, ForwardAgent: forwardAgent}
	m.directMessage = m.tr.Message("ssh.connecting")
	rows, cols := min(max(1, m.height), 1000), min(max(1, m.width), 1000)
	request := coreclient.SessionRequest{ServerRef: ref, Term: os.Getenv("TERM"), Rows: rows, Cols: cols, ForwardAgent: forwardAgent, HostKeyPolicy: "ask", AllowAuthRetry: true}
	if request.Term == "" || request.Term == "dumb" {
		request.Term = "xterm-256color"
	}
	return tea.Batch(m.notify(NoticeMsg{ID: "ssh", Text: m.tr.Message("ssh.connecting"), Persistent: true}), func() tea.Msg {
		if resolve {
			ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
			defer cancel()
			server, err := client.Resolve(ctx, ref)
			if err != nil {
				return sshResult{Attempt: a, Result: connection.Result{Err: err}}
			}
			request.ServerRef = server.ID
		}
		return sshResult{Attempt: a, Result: a.Start(request)}
	})
}

func sshPoll(a *connection.Attempt, revision uint64) tea.Cmd {
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return sshPollMsg{Attempt: a, Revision: revision} })
}

func (m *Model) handleSSH(msg sshResult) tea.Cmd {
	current := m.ssh
	if current == nil || current.Attempt != msg.Attempt || current.Revision != msg.Revision {
		return nil
	}
	if msg.Answered {
		current.Submitting = false
		if msg.Result.Err != nil {
			return m.finishSSH(msg.Result.Err)
		}
		current.Challenge = nil
		m.dismissModal(false)
		return sshPoll(current.Attempt, current.Revision)
	}
	r := msg.Result
	if r.Session.ID != "" {
		current.Session = r.Session
	}
	if r.Err != nil {
		current.Failure = r.Err
		// A displayed challenge belongs to the user until Enter/Esc. Background
		// failure or expiry never silently dismisses its fingerprint snapshot.
		if current.Challenge != nil {
			return nil
		}
		return m.finishSSH(r.Err)
	}
	if current.Session.Done() {
		current.Failure = sessionFailure(current.Session)
		if current.Challenge != nil {
			return nil
		}
		if current.ForwardAgent && coreclient.Code(current.Failure) == "agent_forwarding_unavailable" {
			return m.retryWithoutAgent()
		}
		return m.finishSSH(current.Failure)
	}
	if current.Submitting {
		return nil
	}
	if current.Challenge != nil {
		return sshPoll(current.Attempt, current.Revision)
	}
	if r.Challenge != nil {
		current.Challenge = r.Challenge
		return tea.Batch(m.openSSHChallenge(*r.Challenge), sshPoll(current.Attempt, current.Revision))
	}
	if current.Session.Ready() {
		if m.exitConfirm != nil {
			return sshPoll(current.Attempt, current.Revision)
		}
		m.handoff = current
		m.removeNotice("ssh")
		return tea.Quit
	}
	if current.Session.State != "connecting" && current.Session.State != "host_key_pending" && current.Session.State != "auth_pending" {
		return m.finishSSH(coreclient.Failure("SSH_STATE_UNKNOWN"))
	}
	return sshPoll(current.Attempt, current.Revision)
}

func sessionFailure(s coreclient.Session) error {
	code := s.FailureCode
	if code == "" {
		code = s.FrameworkCode
	}
	if code == "" {
		code = s.DisconnectCause
	}
	if code == "" {
		code = "CONNECTION_FAILED"
	}
	return coreclient.Failure(code)
}

func (m *Model) retryWithoutAgent() tea.Cmd {
	current := m.ssh
	// The capability snapshot can become stale, or the remote can refuse
	// forwarding. Retry once without it and invalidate already scheduled polls.
	current.Submitting = true
	current.Revision++
	ref, resolve := current.Ref, current.Resolve
	if current.Session.ServerID != "" {
		ref, resolve = current.Session.ServerID, false
	}
	attempt := current.Attempt
	return func() tea.Msg {
		attempt.Close()
		return sshRetryMsg{Ref: ref, Resolve: resolve, Attempt: attempt}
	}
}

func (m *Model) openSSHChallenge(c coreclient.Challenge) tea.Cmd {
	a := m.ssh.Attempt
	modal := &Modal{Kind: HostTrust, Title: m.tr.Message("ssh.trust"), Body: fmt.Sprintf("%s\n%s\n%s\n%s", Display(c.ServerAlias), Display(c.Host), Display(c.KeyType), Display(c.Fingerprint))}
	modal.OnCancel = func() tea.Msg { return sshCancelMsg{Attempt: a} }
	if c.Type == "host_key" {
		modal.OnConfirm = func(string) tea.Cmd {
			return m.answerSSH("host-key", coreclient.ChallengeAnswer{ChallengeID: c.ID, Accept: true, Remember: true})
		}
		return m.OpenModal(modal)
	}
	modal.Kind, modal.Title = EditForm, m.tr.Message("ssh.auth")
	modal.Body = Display(c.ServerAlias)
	method := "password"
	if c.FailedMethod != "" {
		method = c.FailedMethod
	}
	if c.PassphraseRequired {
		method = "key"
	}
	methods := []Choice{}
	for _, value := range c.AllowedMethods {
		methods = append(methods, Choice{ID: value, Label: m.tr.Message("ui.auth." + value)})
	}
	if len(methods) == 0 {
		methods = []Choice{{ID: "password", Label: m.tr.Message("ui.auth.password")}, {ID: "key", Label: m.tr.Message("ui.auth.key")}, {ID: "agent", Label: m.tr.Message("ui.auth.agent")}}
	}
	found := false
	for _, choice := range methods {
		if choice.ID == method {
			found = true
		}
	}
	if !found {
		method = methods[0].ID
	}
	keys := []Choice{}
	for _, key := range m.snapshot.Config.Keys {
		if key.PrivateKeySet {
			keys = append(keys, Choice{ID: key.ID, Label: key.Alias})
		}
	}
	keyID := ""
	// Auth challenges can refer to a jump host. Its key, rather than the final
	// destination's key, is the default selection for passphrase retries.
	for _, s := range m.snapshot.Config.Servers {
		if s.Alias == c.ServerAlias {
			keyID = s.KeyID
		}
	}
	if keyID == "" && len(keys) > 0 {
		keyID = keys[0].ID
	}
	modal.Form = NewForm(
		m.choice("method", "ui.auth_method", method, methods...),
		conditional(m.text("password", "ssh.password", "", true, true), "method", "password"),
		conditional(m.choice("key_id", "ssh.key", keyID, keys...), "method", "key"),
		conditional(m.text("passphrase", "ssh.passphrase", "", c.PassphraseRequired, true), "method", "key"),
	)
	modal.OnKey = func(key tea.KeyPressMsg) (bool, tea.Cmd) {
		if key.String() == "esc" {
			return true, m.cancelSSH()
		}
		return false, nil
	}
	modal.OnSubmit = func(values map[string]string) tea.Cmd {
		answer := coreclient.ChallengeAnswer{ChallengeID: c.ID, Remember: true}
		switch values["method"] {
		case "password":
			answer.Password = values["password"]
		case "key":
			if values["key_id"] == "" {
				return func() tea.Msg {
					return FormResult{ModalID: modal.ID, InstanceID: modal.instanceID, Fields: map[string]string{"key_id": m.tr.Message("ui.required")}}
				}
			}
			answer.KeyID, answer.Passphrase = values["key_id"], values["passphrase"]
		}
		return m.answerSSH("auth", answer)
	}
	return m.OpenModal(modal)
}

func (m *Model) answerSSH(kind string, answer coreclient.ChallengeAnswer) tea.Cmd {
	current := m.ssh
	if current == nil || current.Submitting {
		return nil
	}
	if current.Failure != nil {
		return m.finishSSH(current.Failure)
	}
	current.Submitting = true
	current.Revision++
	revision := current.Revision
	m.modal.Busy = true
	a := current.Attempt
	return func() tea.Msg {
		return sshResult{Attempt: a, Answered: true, Revision: revision, Result: connection.Result{Err: a.Answer(kind, answer)}}
	}
}

type sshCancelMsg struct{ Attempt *connection.Attempt }

func (m *Model) cancelSSH() tea.Cmd {
	if m.ssh == nil {
		return nil
	}
	a := m.ssh.Attempt
	m.ssh, m.modal = nil, nil
	m.removeNotice("ssh")
	cleanup := func() tea.Msg { a.Close(); return sshCanceled{Warnings: a.Finish()} }
	if m.launch.ReturnPolicy() == entry.ExitProgram {
		m.exitCode = entry.ExitCanceled
		return tea.Sequence(cleanup, tea.Quit)
	}
	return tea.Batch(cleanup, m.notify(NoticeMsg{ID: "ssh", Text: m.tr.Message("ssh.canceled")}))
}

func (m *Model) finishSSH(err error) tea.Cmd {
	a := m.ssh.Attempt
	m.ssh, m.modal = nil, nil
	m.directMessage = m.tr.ProtocolError(coreclient.Code(err))
	cleanup := func() tea.Msg { a.Close(); return sshCanceled{Warnings: a.Finish()} }
	notice := m.notify(NoticeMsg{ID: "ssh", Text: m.directMessage, Error: true})
	if m.launch.ReturnPolicy() == entry.ExitProgram {
		m.exitCode = entry.ExitFailure
		return tea.Sequence(cleanup, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return aliasFinishedMsg{} }))
	}
	return tea.Batch(cleanup, notice, m.beginLoad(false))
}

func (m *Model) showSSHResult(result terminal.Result, warnings []string, id string) tea.Cmd {
	text := m.tr.Message("ssh.ended")
	isError := result.Err != nil || result.Code() != 0
	if result.ExitCode != nil {
		text = m.tr.Text("ssh.exit", map[string]any{"Code": strconv.Itoa(*result.ExitCode)})
	}
	if result.Err != nil {
		text = m.tr.ProtocolError(coreclient.Code(result.Err))
	}
	if result.Detached {
		text = m.tr.Text("ssh.detached", map[string]any{"ID": id})
	}
	cmds := []tea.Cmd{m.notify(NoticeMsg{ID: "ssh", Text: text, Error: isError})}
	if result.Truncated {
		cmds = append(cmds, m.notify(NoticeMsg{ID: "ssh-truncated", Text: m.tr.Message("ssh.truncated"), Error: true}))
	}
	for _, code := range warnings {
		cmds = append(cmds, m.notify(NoticeMsg{ID: "ssh-warning:" + code, Text: m.tr.Text("ui.warning", map[string]any{"Code": code}), Error: true}))
	}
	return tea.Batch(cmds...)
}
