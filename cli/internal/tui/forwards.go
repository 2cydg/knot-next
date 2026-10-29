package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"knot-cli/internal/coreclient"
)

type forwardManager struct {
	ServerID    string
	Saved       bool
	Definitions []coreclient.ForwardDefinition
	Runtime     []coreclient.Forward
	Connections []coreclient.Connection
	Generation  uint64
	Loading     bool
}
type forwardsResult struct {
	ModalID     uint64
	Instance    string
	Generation  uint64
	Definitions []coreclient.ForwardDefinition
	Runtime     []coreclient.Forward
	Connections []coreclient.Connection
	Err         error
}
type forwardActionResult struct {
	manageResult
	Forward coreclient.Forward
}

func (m *Model) openForwards(serverID string) tea.Cmd {
	modal := &Modal{Kind: Picker, Title: m.tr.Message("manage.forwards"), Body: m.tr.Message("ui.loading"), Forwards: &forwardManager{ServerID: serverID}}
	modal.OnKey = func(k tea.KeyPressMsg) (bool, tea.Cmd) { return m.forwardKey(modal, k) }
	cmd := m.OpenModal(modal)
	return tea.Batch(cmd, m.refreshForwards(modal))
}
func (m *Model) refreshForwards(modal *Modal) tea.Cmd {
	f := modal.Forwards
	if f == nil || f.Loading || !m.modalCurrent() {
		return nil
	}
	f.Loading = true
	f.Generation++
	generation, id, instance, serverID, client := f.Generation, modal.ID, modal.instanceID, f.ServerID, m.snapshot.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
		defer cancel()
		out := forwardsResult{ModalID: id, Instance: instance, Generation: generation}
		err := client.CheckInstance(ctx, instance)
		if err == nil {
			out.Definitions, err = client.ForwardDefinitions(ctx, serverID)
		}
		if err == nil {
			out.Runtime, err = client.Forwards(ctx, serverID)
		}
		if err == nil {
			out.Connections, err = client.Connections(ctx, serverID)
		}
		out.Err = err
		return out
	}
}
func (m *Model) handleForwards(msg forwardsResult) tea.Cmd {
	modal := m.modal
	if modal == nil || modal.ID != msg.ModalID || modal.Forwards == nil || modal.Forwards.Generation != msg.Generation || msg.Instance != m.snapshot.Runtime.InstanceID {
		return nil
	}
	f := modal.Forwards
	f.Loading = false
	if msg.Err != nil {
		modal.Choices = nil
		modal.Body = m.tr.ProtocolError(coreclient.Code(msg.Err)) + "\n" + m.tr.Message("manage.forward_keys")
		return m.notify(NoticeMsg{ID: "forwards", Text: m.tr.ProtocolError(coreclient.Code(msg.Err)), Error: true})
	}
	selected := ""
	if modal.Index >= 0 && modal.Index < len(modal.Choices) {
		selected = modal.Choices[modal.Index].ID
	}
	f.Definitions, f.Runtime, f.Connections = msg.Definitions, msg.Runtime, msg.Connections
	m.forwardChoices(modal)
	for i, c := range modal.Choices {
		if c.ID == selected {
			modal.Index = i
			modal.PickerPage = i / 5
		}
	}
	return nil
}
func (m *Model) forwardChoices(modal *Modal) {
	f := modal.Forwards
	modal.Choices = nil
	if f.Saved {
		modal.Body = m.tr.Message("manage.saved_rules") + "\n" + m.tr.Message("manage.saved_scope") + "\n" + m.tr.Message("manage.saved_keys")
		for _, d := range f.Definitions {
			modal.Choices = append(modal.Choices, Choice{ID: d.ID, Label: fmt.Sprintf("%s %s → %s · %s", m.tr.Message("manage.forward."+d.Type), d.Bind, m.none(d.Target), m.boolean(d.Enabled))})
		}
	} else {
		modal.Body = m.tr.Message("manage.running_rules") + "\n" + m.tr.Message("manage.forward_keys")
		for _, r := range f.Runtime {
			bind := r.BoundAddress
			if bind == "" {
				bind = r.Bind
			}
			modal.Choices = append(modal.Choices, Choice{ID: r.ID, Label: fmt.Sprintf("%s %s → %s · %s", m.tr.Message("manage.forward."+r.Type), bind, m.none(r.Target), m.tr.Message("manage.state."+r.State))})
		}
	}
	modal.Index = max(0, min(modal.Index, len(modal.Choices)-1))
	modal.PickerPage = modal.Index / 5
	if len(modal.Choices) == 0 {
		modal.Body += "\n" + m.tr.Message("ui.empty")
	}
}
func (m *Model) forwardKey(modal *Modal, k tea.KeyPressMsg) (bool, tea.Cmd) {
	f := modal.Forwards
	switch k.String() {
	case "[", "]":
		f.Saved = !f.Saved
		modal.Index = 0
		modal.PickerPage = 0
		m.forwardChoices(modal)
		return true, nil
	case "r":
		return true, m.refreshForwards(modal)
	case "n":
		if !f.Saved {
			return true, m.openTemporaryForward(f)
		}
		fallthrough
	case "e":
		if !f.Saved {
			return true, nil
		}
		m.selectedID = f.ServerID
		return true, m.openEditor(coreclient.Servers, false, false, "forwards")
	case "s", "x", "d", "delete":
		if f.Saved {
			if (k.String() == "d" || k.String() == "delete") && modal.Index >= 0 && modal.Index < len(f.Definitions) {
				return true, m.confirmDefinitionDelete(f, f.Definitions[modal.Index])
			}
			return true, nil
		}
		if modal.Index < 0 || modal.Index >= len(modal.Choices) {
			return true, nil
		}
		id := modal.Choices[modal.Index].ID
		for _, r := range f.Runtime {
			if r.ID == id {
				op := "start"
				if k.String() == "x" {
					op = "stop"
				}
				if k.String() == "d" || k.String() == "delete" {
					op = "delete"
				}
				return true, m.confirmForwardControl(f.ServerID, r, op)
			}
		}
	}
	return false, nil
}
func (m *Model) confirmDefinitionDelete(manager *forwardManager, definition coreclient.ForwardDefinition) tea.Cmd {
	modal := &Modal{Kind: ActionConfirm, Title: m.tr.Message("manage.definition_delete"), Body: definition.Bind + " → " + m.none(definition.Target) + "\n" + m.tr.Message("manage.definition_delete_scope")}
	var definitions []coreclient.ForwardDefinition
	for _, d := range manager.Definitions {
		if d.ID != definition.ID {
			definitions = append(definitions, d)
		}
	}
	serverID := manager.ServerID
	modal.OnConfirm = func(string) tea.Cmd {
		modal.Busy = true
		id, instance, client := modal.ID, modal.instanceID, m.snapshot.Client
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
			defer cancel()
			err := client.CheckInstance(ctx, instance)
			if err == nil {
				err = client.PutForwardDefinitions(ctx, serverID, definitions)
			}
			return forwardActionResult{manageResult: manageResult{ModalID: id, Instance: instance, Err: err}, Forward: coreclient.Forward{ServerID: serverID}}
		}
	}
	return m.OpenModal(modal)
}

func (m *Model) forwardDetail(modal *Modal) string {
	if modal.Index < 0 || modal.Index >= len(modal.Choices) || modal.Forwards.Saved {
		return ""
	}
	id := modal.Choices[modal.Index].ID
	for _, r := range modal.Forwards.Runtime {
		if r.ID != id {
			continue
		}
		created := ""
		for _, c := range modal.Forwards.Connections {
			if c.ID == r.ConnectionID {
				created = c.CreatedAt.Local().Format("01-02 15:04:05")
			}
		}
		failure := ""
		if r.FailureCode != "" {
			failure = m.tr.ProtocolError(r.FailureCode)
		}
		return strings.Join([]string{m.tr.Message("manage.origin."+r.Origin) + " · " + m.tr.Message("manage.state."+r.State), r.Bind + " → " + m.none(r.Target), m.tr.Message("manage.connection") + ": " + r.ConnectionID + " " + created, r.ID, failure}, "\n")
	}
	return ""
}
func (m *Model) openTemporaryForward(manager *forwardManager) tea.Cmd {
	choices := []Choice{{ID: "", Label: m.tr.Message("manage.latest_connection")}}
	for _, c := range manager.Connections {
		choices = append(choices, Choice{ID: c.ID, Label: c.CreatedAt.Local().Format("2006-01-02 15:04:05") + " · " + c.ID})
	}
	form := NewForm(m.choice("type", "ui.type", "local", m.forwardTypes()...), m.text("bind", "manage.bind", "127.0.0.1:0", true, false), m.text("target", "manage.forward_target", "", false, false), m.choice("connection_id", "manage.connection", "", choices...))
	modal := &Modal{Kind: EditForm, Title: m.tr.Message("manage.temporary_new"), Body: m.tr.Message("manage.temporary_scope"), Form: form}
	serverID := manager.ServerID
	modal.OnSubmit = func(v map[string]string) tea.Cmd {
		fields := map[string]string{}
		for name, key := range validateForward(v["type"], v["bind"], v["target"]) {
			fields[name] = m.tr.Message(key)
		}
		if len(fields) > 0 {
			return m.formErrors(modal, fields)
		}
		id, instance, client := modal.ID, modal.instanceID, m.snapshot.Client
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
			defer cancel()
			err := client.CheckInstance(ctx, instance)
			var out coreclient.Forward
			if err == nil {
				out, err = client.CreateForward(ctx, coreclient.ForwardRequest{ServerID: serverID, ConnectionID: v["connection_id"], Type: v["type"], Bind: v["bind"], Target: v["target"]})
			}
			return forwardActionResult{manageResult: manageResult{ModalID: id, Instance: instance, Err: err}, Forward: out}
		}
	}
	return m.OpenModal(modal)
}
func (m *Model) confirmForwardControl(serverID string, r coreclient.Forward, op string) tea.Cmd {
	modal := &Modal{Kind: ActionConfirm, Title: m.tr.Message("manage." + op), Body: r.Bind + " → " + m.none(r.Target) + "\n" + m.tr.Message("manage.runtime_only")}
	modal.OnConfirm = func(string) tea.Cmd {
		modal.Busy = true
		id, instance, client := modal.ID, modal.instanceID, m.snapshot.Client
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 25*time.Second)
			defer cancel()
			err := client.CheckInstance(ctx, instance)
			var out coreclient.Forward
			if err == nil {
				out, err = client.ControlForward(ctx, r.ID, op)
			}
			return forwardActionResult{manageResult: manageResult{ModalID: id, Instance: instance, Err: err}, Forward: out}
		}
	}
	return m.OpenModal(modal)
}
func (m *Model) handleForwardAction(msg forwardActionResult) tea.Cmd {
	if m.modal == nil || m.modal.ID != msg.ModalID || msg.Instance != m.snapshot.Runtime.InstanceID {
		return nil
	}
	if msg.Err != nil {
		return m.handleManageResult(msg.manageResult)
	}
	// Creation is acceptance. The manager GET, rather than HTTP 202, decides
	// whether a listener really became active and retains its actual connection.
	message := m.tr.Message("manage.saved")
	if msg.Forward.State != "" {
		message = m.tr.Message("manage.state." + msg.Forward.State)
	}
	return tea.Batch(m.openForwards(msg.Forward.ServerID), m.notify(NoticeMsg{ID: "forward", Text: message}))
}
