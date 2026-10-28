package tui

import tea "charm.land/bubbletea/v2"

type ModalKind string

const (
	Information      ModalKind = "information"
	HostTrust        ModalKind = "host-trust"
	TransferConfirm  ModalKind = "transfer-confirm"
	SFTPCloseConfirm ModalKind = "sftp-close-confirm"
	EditForm         ModalKind = "form"
	Draft            ModalKind = "draft"
	Picker           ModalKind = "picker"
	ActionConfirm    ModalKind = "action-confirm"
)

type Choice struct {
	ID       string
	Label    string
	Disabled bool
}
type Modal struct {
	ID         uint64
	Scroll     int
	Kind       ModalKind
	Title      string
	Body       string
	Form       *Form
	Choices    []Choice
	Index      int
	PickerPage int
	PageSize   int
	OnConfirm  func(string) tea.Cmd
	OnSubmit   func(map[string]string) tea.Cmd
	OnCancel   tea.Cmd
	Prior      *Modal
	anchor     Focus
	resourceID string
	instanceID string
	Busy       bool
	OnKey      func(tea.KeyPressMsg) (bool, tea.Cmd)
	OnChange   func()
	Edit       *resourceDraft
	Forwards   *forwardManager
}

func (m *Modal) exclusive() bool {
	return m.Kind == HostTrust || m.Kind == TransferConfirm || m.Kind == SFTPCloseConfirm || m.Kind == ActionConfirm
}

func (m *Model) OpenModal(modal *Modal) tea.Cmd {
	var cmds []tea.Cmd
	anchor, resourceID := m.focus, m.selectedID
	for old := m.modal; old != nil; old = old.Prior {
		cmds = append(cmds, old.OnCancel)
		anchor, resourceID = old.anchor, old.resourceID
	}
	m.modalSequence++
	modal.ID = m.modalSequence
	modal.anchor = anchor
	modal.resourceID = resourceID
	modal.instanceID = m.snapshot.Runtime.InstanceID
	m.modal = modal
	if modal.Form != nil {
		modal.Form.Resize(m.formInputWidth())
		cmds = append(cmds, modal.Form.Focus(0))
	}
	m.clampModalScroll()
	return tea.Batch(cmds...)
}
func (m *Model) closeModal() tea.Cmd {
	return m.dismissModal(true)
}

// Successful completion restores focus without invoking cancellation cleanup.
func (m *Model) dismissModal(canceled bool) tea.Cmd {
	modal := m.modal
	if modal == nil {
		return nil
	}
	if modal.Prior != nil {
		m.modal = modal.Prior
		if m.modal.Form != nil {
			return m.modal.Form.Focus(m.modal.Form.Index)
		}
		return nil
	}
	m.modal = nil
	m.focus = modal.anchor
	for _, row := range m.rows() {
		if row.ID == modal.resourceID {
			m.selectedID = row.ID
			break
		}
	}
	var cancel tea.Cmd
	if canceled {
		cancel = modal.OnCancel
	}
	if m.focus == SearchFocus {
		return tea.Batch(cancel, m.search.Focus())
	}
	return cancel
}
func (m *Model) submitForm() tea.Cmd {
	form := m.modal.Form
	if form == nil || len(form.Fields) == 0 || form.Busy {
		return nil
	}
	if !m.modalCurrent() {
		return m.notify(NoticeMsg{ID: "form-error", Text: m.tr.ProtocolError("INSTANCE_CHANGED"), Error: true})
	}
	cmd := form.Validate(m.tr.Message("ui.required"))
	if form.Invalid() {
		return cmd
	}
	if m.modal.OnSubmit == nil {
		return nil
	}
	form.Busy = true
	cmd = m.modal.OnSubmit(form.Values())
	if cmd == nil {
		form.Busy = false
	}
	return cmd
}

func (m *Model) modalCurrent() bool {
	return m.state == Ready && m.modal.instanceID == m.snapshot.Runtime.InstanceID
}

func (m *Model) confirmModal(value string) tea.Cmd {
	if m.modal.Busy {
		return nil
	}
	if !m.modalCurrent() {
		return m.notify(NoticeMsg{ID: "modal-error", Text: m.tr.ProtocolError("INSTANCE_CHANGED"), Error: true})
	}
	return m.modal.OnConfirm(value)
}
func (m *Model) modalKey(key tea.KeyPressMsg) tea.Cmd {
	modal := m.modal
	binding := key.String()
	if modal.Busy {
		return nil
	}
	// These confirmations consume every key, including help and Ctrl+c/s.
	if modal.exclusive() {
		if binding == "esc" {
			return m.closeModal()
		}
		if binding == "enter" && modal.OnConfirm != nil {
			return m.confirmModal("")
		}
		return nil
	}
	if modal.Kind == Draft {
		switch binding {
		case "y", "Y":
			m.modal = modal.Prior
			return m.submitForm()
		case "n", "N":
			m.modal = modal.Prior
			m.modal.Form = nil
			return m.closeModal()
		case "esc":
			return m.closeModal()
		}
		return nil
	}
	if modal.Form != nil {
		if modal.Form.Busy {
			return nil
		}
		if modal.OnKey != nil {
			if handled, cmd := modal.OnKey(key); handled {
				return cmd
			}
		}
		switch binding {
		case "esc":
			if modal.Form.Dirty() {
				m.modal = &Modal{Kind: Draft, Title: m.tr.Message("ui.draft"), Body: m.tr.Message("ui.draft_hint"), Prior: modal}
				return nil
			}
			return m.closeModal()
		case "ctrl+s":
			return m.submitForm()
		case "enter", "tab":
			if modal.Form.next(1) == -1 {
				return m.submitForm()
			}
			return modal.Form.Focus(modal.Form.next(1))
		case "shift+tab":
			if index := modal.Form.next(-1); index >= 0 {
				return modal.Form.Focus(index)
			}
			return nil
		case "ctrl+left":
			return modal.Form.switchSection(-1)
		case "ctrl+right":
			return modal.Form.switchSection(1)
		}
		cmd := modal.Form.update(key)
		if modal.OnChange != nil {
			modal.OnChange()
		}
		return cmd
	}
	if binding == "esc" {
		return m.closeModal()
	}
	if modal.OnKey != nil {
		if handled, cmd := modal.OnKey(key); handled {
			return cmd
		}
	}
	if modal.Kind == Picker {
		size := modal.PageSize
		if size < 1 {
			size = 5
		}
		if binding == "left" || binding == "right" {
			delta := -1
			if binding == "right" {
				delta = 1
			}
			modal.PickerPage = max(0, min((len(modal.Choices)-1)/size, modal.PickerPage+delta))
			modal.Index = modal.PickerPage * size
			return nil
		}
		start := modal.PickerPage * size
		end := min(len(modal.Choices), start+size)
		if binding == "up" {
			modal.Index = max(start, modal.Index-1)
		}
		if binding == "down" {
			modal.Index = min(end-1, modal.Index+1)
		}
		if len(binding) == 1 && binding[0] >= '1' && binding[0] <= '9' {
			index := start + int(binding[0]-'1')
			if index < end {
				modal.Index = index
				binding = "enter"
			}
		}
		if binding == "enter" && modal.Index >= 0 && modal.Index < len(modal.Choices) && !modal.Choices[modal.Index].Disabled && modal.OnConfirm != nil {
			return m.confirmModal(modal.Choices[modal.Index].ID)
		}
	}
	if modal.Kind == Information {
		if binding == "enter" {
			return m.closeModal()
		}
		if binding == "down" {
			modal.Scroll = min(m.informationScrollLimit(), modal.Scroll+1)
		}
		if binding == "up" {
			modal.Scroll = max(0, modal.Scroll-1)
		}
		if binding == "home" {
			modal.Scroll = 0
		}
	}
	return nil
}
