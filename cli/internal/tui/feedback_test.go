package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/i18n"
)

func TestMenuEscapeConfirmsExitWithoutCore(t *testing.T) {
	for _, state := range []ListState{Ready, Loading, Offline, AuthFailed, ReadError} {
		m := testModel(t)
		m.state = state
		_, cmd := m.Update(press("esc"))
		if cmd != nil || m.exitConfirm == nil {
			t.Fatal("menu Escape did not ask to exit", state)
		}
		m.Update(press("left"))
		_, cmd = m.Update(press("enter"))
		if cmd == nil {
			t.Fatal("Yes requires no core connection", state)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("Yes did not quit", state)
		}
	}
	m := testModel(t, "servers")
	m.Update(press("esc"))
	if m.exitConfirm != nil || m.focus != MenuFocus {
		t.Fatal("operations Escape must return to the menu first")
	}
}

func TestExitConfirmationKeepsDraftAndReceivesBackgroundResults(t *testing.T) {
	m := testModel(t, "servers")
	m.openEditor(coreclient.Servers, true, false)
	modal, form := m.modal, m.modal.Form
	setField(form, "alias", "keep-this-draft")
	form.Busy = true
	m.Update(press("ctrl+c"))
	for _, msg := range []tea.Msg{press("n"), tea.PasteMsg{Content: "discarded"}} {
		// Reopen after N dismisses the dialog; pasted input must be consumed.
		m.Update(msg)
		if m.exitConfirm == nil {
			m.Update(press("ctrl+c"))
		}
	}
	m.Update(FormResult{ModalID: modal.ID, InstanceID: modal.instanceID, Fields: map[string]string{"host": "required host"}})
	if form.Busy || form.Fields[form.Index].Name != "host" || m.exitConfirm == nil {
		t.Fatal("exit confirmation blocked the pending form result")
	}
	m.Update(press("esc"))
	if m.modal != modal || form.Values()["alias"] != "keep-this-draft" || form.Fields[form.Index].Error != "required host" {
		t.Fatal("canceling exit lost the draft or its completion")
	}
}

func TestFormArrowNavigationAndBracketTabs(t *testing.T) {
	for _, kind := range []coreclient.Resource{coreclient.Servers, coreclient.Keys, coreclient.Proxies, coreclient.Providers} {
		m := testModel(t, "servers")
		m.openEditor(kind, true, false)
		form := m.modal.Form
		setField(form, "alias", "unchanged")
		submits := 0
		m.modal.OnSubmit = func(map[string]string) tea.Cmd { submits++; return nil }
		m.Update(press("up"))
		if form.Index != 0 {
			t.Fatal("Up on first field moved focus", kind)
		}
		for form.next(1) >= 0 {
			next := form.next(1)
			m.Update(press("down"))
			if form.Index != next {
				t.Fatal("Down did not skip hidden fields", kind)
			}
		}
		last := form.Index
		m.Update(press("down"))
		if form.Index != last || submits != 0 || form.Busy {
			t.Fatal("Down on last field submitted", kind)
		}
		for form.next(-1) >= 0 {
			m.Update(press("up"))
		}
		if form.Index != 0 || form.Values()["alias"] != "unchanged" {
			t.Fatal("arrow navigation lost entered data", kind)
		}
		if kind == coreclient.Servers {
			for _, want := range []string{"proxy", "jumps", "forwards", "basic"} {
				m.Update(press("]"))
				if form.Section != want {
					t.Fatal("next tab", form.Section, want)
				}
			}
			m.Update(press("["))
			if form.Section != "forwards" || form.Values()["alias"] != "unchanged" {
				t.Fatal("previous tab failed to wrap or changed input")
			}
		}
	}
	m := testModel(t, "keys")
	m.openEditor(coreclient.Keys, true, false)
	form := m.modal.Form
	setField(form, "source", "inline")
	focusField(t, form, "private_key")
	m.Update(tea.PasteMsg{Content: "first\nsecond"})
	m.Update(press("down"))
	if form.Fields[form.Index].Name != "passphrase" || form.Values()["private_key"] != "first\nsecond" {
		t.Fatal("Down in multiline input did not move to the next field")
	}
	m.Update(press("up"))
	m.Update(press("["))
	if !strings.Contains(form.Values()["private_key"], "[") {
		t.Fatal("bracket input was consumed by a form without tabs")
	}
}

func TestServerPasswordDefaultsAndRemovedClearControl(t *testing.T) {
	m := testModel(t, "servers")
	m.openEditor(coreclient.Servers, true, false)
	if m.modal.Form.Values()["auth_method"] != "password" {
		t.Fatal("new server did not default to password authentication")
	}
	for _, credentials := range []bool{false, true} {
		m.showEditor(resourceDraft{Kind: coreclient.Servers, Server: coreclient.ServerWrite{AuthMethod: "password"}, CredentialsOnly: credentials})
		if _, exists := m.modal.Form.Values()["clear_password"]; exists {
			t.Fatal("server form still offers clear stored password")
		}
		if _, exists := m.modal.Form.Values()["password"]; !exists {
			t.Fatal("server password field missing")
		}
	}
}

func TestFeedbackDialogsFitBothLanguages(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			m := testModel(t)
			m.tr = i18n.New(language)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.Update(press("esc"))
			view := ansi.Strip(m.View().Content)
			checkSize(t, view, size)
			for _, key := range []string{"ui.exit_prompt", "ui.yes", "ui.no"} {
				if !strings.Contains(view, m.tr.Message(key)) {
					t.Fatal("exit dialog copy missing", language, key, view)
				}
			}
			m.Update(press("esc"))
			m.openEditor(coreclient.Servers, true, false)
			header := m.modalHeader(m.modal, 74)
			if strings.Contains(header, "Tab") || strings.Contains(header, "Enter") || !strings.Contains(header, "↑/↓") {
				t.Fatal("form shortcuts did not match arrow navigation", header)
			}
			checkSize(t, ansi.Strip(m.View().Content), size)
		}
	}
}
