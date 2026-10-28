package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"knot-cli/internal/coreclient"
)

type proxyResult struct {
	ModalID  uint64
	Instance string
	Revision uint64
	Result   coreclient.ProxyTestResult
	Err      error
}

func (m *Model) testSavedProxy() tea.Cmd {
	return m.runProxyTest(nil, coreclient.ProxyTestRequest{ProxyID: m.selectedID})
}
func (m *Model) testDraftProxy(modal *Modal) tea.Cmd {
	if !m.modalCurrent() {
		return m.notify(NoticeMsg{ID: "proxy-test", Text: m.tr.ProtocolError("INSTANCE_CHANGED"), Error: true})
	}
	v := modal.Form.Values()
	port, err := strconv.Atoi(v["port"])
	fields := map[string]string{}
	if err != nil || port < 1 || port > 65535 {
		fields["port"] = m.tr.Message("manage.invalid_port")
	}
	if strings.TrimSpace(v["host"]) == "" {
		fields["host"] = m.tr.Message("ui.required")
	}
	if len(fields) > 0 {
		return m.formErrors(modal, fields)
	}
	draft := &coreclient.ProxyDraft{Type: v["type"], Host: strings.TrimSpace(v["host"]), Port: port, Username: v["username"]}
	if v["password"] != "" || v["clear_password"] == "true" {
		password := v["password"]
		if v["clear_password"] == "true" {
			password = ""
		}
		draft.Password = &password
	}
	id := ""
	if !modal.Edit.Create {
		id = modal.Edit.ID
	}
	return m.runProxyTest(modal, coreclient.ProxyTestRequest{ProxyID: id, Draft: draft, Target: strings.TrimSpace(v["test_target"])})
}

func (m *Model) runProxyTest(modal *Modal, request coreclient.ProxyTestRequest) tea.Cmd {
	if m.cancelProxyTest != nil {
		m.cancelProxyTest()
	}
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	m.cancelProxyTest = cancel
	m.proxyTestRevision++
	revision, instance, client := m.proxyTestRevision, m.snapshot.Runtime.InstanceID, m.snapshot.Client
	modalID := uint64(0)
	if modal != nil {
		modalID = modal.ID
		modal.OnCancel = func() tea.Msg { cancel(); return nil }
	}
	return tea.Batch(m.notify(NoticeMsg{ID: "proxy-test", Text: m.tr.Message("manage.testing"), Persistent: true}), func() tea.Msg {
		defer cancel()
		err := client.CheckInstance(ctx, instance)
		var result coreclient.ProxyTestResult
		if err == nil {
			result, err = client.TestProxy(ctx, request)
		}
		return proxyResult{ModalID: modalID, Instance: instance, Revision: revision, Result: result, Err: err}
	})
}
func (m *Model) handleProxyResult(msg proxyResult) tea.Cmd {
	if msg.Revision != m.proxyTestRevision {
		return nil
	}
	m.removeNotice("proxy-test")
	if msg.Instance != m.snapshot.Runtime.InstanceID || (msg.ModalID != 0 && (m.modal == nil || m.modal.ID != msg.ModalID)) {
		return nil
	}
	if msg.Err != nil {
		return m.notify(NoticeMsg{ID: "proxy-test", Text: m.tr.ProtocolError(coreclient.Code(msg.Err)), Error: true})
	}
	r := msg.Result
	var lines []string
	lines = append(lines, m.tr.Message("manage.scope."+r.Scope))
	for _, stage := range r.Stages {
		status := m.tr.Message("manage.failed")
		if stage.Passed {
			status = m.tr.Message("manage.passed")
		}
		lines = append(lines, fmt.Sprintf("%s: %s (%d ms)", m.tr.Message("manage.stage."+stage.Name), status, stage.DurationMS))
	}
	if r.FailureCode != "" {
		lines = append(lines, m.tr.ProtocolError(r.FailureCode))
	}
	if msg.ModalID != 0 {
		m.modal.Body = m.tr.Message("manage.proxy_form_hint") + "\n" + strings.Join(lines, "\n")
	}
	return m.notify(NoticeMsg{ID: "proxy-test", Text: strings.Join(lines, "; "), Error: !r.Passed})
}
