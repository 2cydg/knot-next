package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

func testModel(t *testing.T, args ...string) *Model {
	t.Helper()
	launch, err := entry.Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), launch, i18n.New(i18n.English), "unused")
	m.state = Ready
	m.hasSnapshot = true
	m.snapshot = snapshot(t, "instance-a")
	m.ensureSelection()
	t.Cleanup(m.Close)
	return m
}
func snapshot(t *testing.T, instance string) coreclient.Snapshot {
	c, err := coreclient.New("http://127.0.0.1:1", "token")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Date(2026, 10, 19, 9, 2, 3, 0, time.Local)
	return coreclient.Snapshot{Client: c, Runtime: coreclient.Runtime{InstanceID: instance}, Version: coreclient.Version{Version: "0.1.0-dev"}, Health: coreclient.Health{Status: "ok", Authenticated: true}, Config: coreclient.Summary{Servers: []coreclient.Server{{ID: "opaque-b", Alias: "beta", Host: "db", Port: 22, User: "ops", Tags: []string{"Production"}}, {ID: "opaque-a", Alias: "alpha", Host: "other", Port: 22, User: "root", LastUsed: &recent}}, Keys: []coreclient.Key{{ID: "key", Alias: "main", Type: "ed25519", Length: 256}}, Proxies: []coreclient.Proxy{{ID: "proxy", Alias: "corp", Type: "socks5", Host: "127.0.0.1", Port: 1080}}}}
}
func press(binding string) tea.KeyPressMsg {
	k := tea.Key{}
	switch binding {
	case "enter":
		k.Code = tea.KeyEnter
	case "esc":
		k.Code = tea.KeyEscape
	case "tab":
		k.Code = tea.KeyTab
	case "shift+tab":
		k.Code = tea.KeyTab
		k.Mod = tea.ModShift
	case "ctrl+s":
		k.Code = 's'
		k.Mod = tea.ModCtrl
	case "ctrl+c":
		k.Code = 'c'
		k.Mod = tea.ModCtrl
	case "up":
		k.Code = tea.KeyUp
	case "down":
		k.Code = tea.KeyDown
	case "left":
		k.Code = tea.KeyLeft
	case "right":
		k.Code = tea.KeyRight
	case "home":
		k.Code = tea.KeyHome
	case "end":
		k.Code = tea.KeyEnd
	case "shift+enter":
		k.Code = tea.KeyEnter
		k.Mod = tea.ModShift
	default:
		k.Code = []rune(binding)[0]
		k.Text = binding
	}
	return tea.KeyPressMsg(k)
}
func TestPrototypeInitialFocusMenuAndTypingPriority(t *testing.T) {
	m := testModel(t)
	if m.focus != MenuFocus {
		t.Fatal("default focus")
	}
	for _, key := range []string{"n", "e", "d", "c", "f", "r", "/"} {
		_, cmd := m.Update(press(key))
		if cmd != nil || len(m.notices) != 0 || m.focus != MenuFocus {
			t.Fatal("menu business key escaped", key)
		}
	}
	m.Update(press("right"))
	m.Update(press("/"))
	m.Update(press("n"))
	m.Update(press("?"))
	if m.focus != SearchFocus || m.search.Value() != "n?" || m.modal != nil {
		t.Fatal("typing triggered business/help")
	}
	m.Update(press("esc"))
	if m.focus != OperationsFocus || m.search.Value() != "n?" {
		t.Fatal("search not preserved")
	}
	direct := testModel(t, "proxy")
	if direct.focus != OperationsFocus || direct.page != entry.Proxies {
		t.Fatal("page-direct focus")
	}
}
func TestTagSearchIsSubstringAndEscPreservesMode(t *testing.T) {
	m := testModel(t, "servers")
	m.Update(press("/"))
	m.Update(press("/"))
	m.Update(tea.PasteMsg{Content: "duct"})
	m.Update(press("esc"))
	if m.search.Value() != "/duct" || len(m.rows()) != 1 || m.rows()[0].ID != "opaque-b" {
		t.Fatal(m.search.Value(), m.rows())
	}
	m.search.SetValue("db")
	if len(m.rows()) != 0 {
		t.Fatal("host broadened alias search")
	}
}
func TestGenerationSelectionAndRestart(t *testing.T) {
	m := testModel(t)
	m.selectedID = "opaque-b"
	m.generation = 2
	old := snapshot(t, "stale")
	m.Update(loadResult{generation: 1, page: entry.Servers, snapshot: old})
	if m.snapshot.Runtime.InstanceID != "instance-a" {
		t.Fatal("stale request overwrote current")
	}
	fresh := snapshot(t, "instance-a")
	m.Update(loadResult{generation: 2, page: entry.Servers, snapshot: fresh})
	if m.selectedID != "opaque-b" {
		t.Fatal("refresh restored row index")
	}
	m.streamOpening = false
	m.OpenModal(&Modal{Kind: HostTrust, Title: "Trust", Body: "old fingerprint"})
	modal := m.modal
	m.Update(loadResult{generation: 2, page: entry.Servers, snapshot: snapshot(t, "instance-b")})
	if m.snapshot.Runtime.InstanceID != "instance-b" || m.modal != modal || m.selectedID == "opaque-b" {
		t.Fatal("restart failed to invalidate state or closed trust modal")
	}
}
func TestDedicatedConfirmationsConsumeAllOtherKeys(t *testing.T) {
	for _, kind := range []ModalKind{HostTrust, TransferConfirm, SFTPCloseConfirm} {
		m := testModel(t, "servers")
		calls := 0
		modal := &Modal{Kind: kind, OnConfirm: func(string) tea.Cmd { calls++; return nil }}
		m.OpenModal(modal)
		for _, key := range []string{"?", "1", "ctrl+s", "ctrl+c", "right", "tab", "n"} {
			m.Update(press(key))
			if m.modal != modal || calls != 0 {
				t.Fatal(kind, key)
			}
		}
		m.Update(press("enter"))
		if calls != 1 {
			t.Fatal("Enter not routed")
		}
		m.Update(press("esc"))
		if m.modal != nil || m.focus != OperationsFocus {
			t.Fatal("focus not restored")
		}
	}
}
func TestFormPasteAdvanceValidationAndDraft(t *testing.T) {
	m := testModel(t, "keys")
	calls := 0
	form := NewForm(MultilineField("material", "Private key", "", true), TextField("alias", "Alias", "", true, false))
	modal := &Modal{Kind: EditForm, Form: form, OnSubmit: func(values map[string]string) tea.Cmd {
		calls++
		return func() tea.Msg { return nil }
	}}
	m.OpenModal(modal)
	m.Update(tea.PasteMsg{Content: "line 1\nline 2\n"})
	if calls != 0 || !strings.Contains(form.Fields[0].Value(), "\n") || form.Index != 0 {
		t.Fatal("paste submitted/flattened")
	}
	before := strings.Count(form.Fields[0].Value(), "\n")
	m.Update(press("shift+enter"))
	if strings.Count(form.Fields[0].Value(), "\n") != before+1 || calls != 0 || form.Index != 0 {
		t.Fatal("Shift+Enter did not insert newline")
	}
	m.Update(press("enter"))
	if form.Index != 1 || calls != 0 {
		t.Fatal("Enter didn't advance")
	}
	m.Update(press("enter"))
	if calls != 0 || form.Fields[1].Error == "" {
		t.Fatal("invalid form submitted")
	}
	m.Update(press("a"))
	m.Update(press("esc"))
	if m.modal.Kind != Draft {
		t.Fatal("dirty draft missing")
	}
	m.Update(press("?"))
	if m.modal.Kind != Draft {
		t.Fatal("draft leaked help")
	}
	m.Update(press("esc"))
	if m.modal != modal || form.Index != 1 {
		t.Fatal("draft focus not restored")
	}
	m.Update(press("ctrl+s"))
	if calls != 1 || !form.Busy {
		t.Fatal("save didn't submit")
	}
	m.Update(press("ctrl+s"))
	if calls != 1 {
		t.Fatal("busy resubmitted")
	}
}
func TestModalCompletionAndPickerDoNotCrossOwners(t *testing.T) {
	m := testModel(t, "keys")
	old := &Modal{Kind: EditForm, Form: NewForm(TextField("alias", "Alias", "a", true, false))}
	m.OpenModal(old)
	current := &Modal{Kind: EditForm, Form: NewForm(TextField("alias", "Alias", "b", true, false))}
	m.OpenModal(current)
	m.Update(FormResult{ModalID: old.ID, InstanceID: "instance-a", Saved: true})
	if m.modal != current {
		t.Fatal("old completion closed current form")
	}
	choices := []Choice{{ID: "a", Label: "a"}, {ID: "b", Label: "b"}, {ID: "c", Label: "c"}, {ID: "d", Label: "d"}, {ID: "e", Label: "e"}, {ID: "f", Label: "f"}}
	selected := ""
	picker := &Modal{Kind: Picker, Choices: choices, PageSize: 5, OnConfirm: func(id string) tea.Cmd { selected = id; return nil }}
	m.OpenModal(picker)
	page, focus := m.page, m.focus
	m.Update(press("right"))
	m.Update(press("1"))
	if selected != "f" || m.page != page || m.focus != focus {
		t.Fatal("picker paging escaped")
	}
}
func TestNoticeUpdatesAndExpirationAreRevisionBound(t *testing.T) {
	m := testModel(t)
	m.notify(NoticeMsg{ID: "transfer", Text: "running", Persistent: true})
	old := m.notices[0].revision
	m.notify(NoticeMsg{ID: "transfer", Text: "finished"})
	m.Update(dismissMsg{id: "transfer", revision: old})
	if len(m.notices) != 1 || m.notices[0].Text != "finished" {
		t.Fatal("stale expiry removed updated notice")
	}
	m.Update(dismissMsg{id: "transfer", revision: m.notices[0].revision})
	if len(m.notices) != 0 {
		t.Fatal("final notice not dismissed")
	}
}
func TestBilingualLayoutsAndModalKeysFitPrototypeSizes(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			m := testModel(t, "servers")
			m.tr = i18n.New(language)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.snapshot.Config.Servers[0].Alias = "全角e\u0301 very-long-user-alias"
			m.ensureSelection()
			base := ansi.Strip(m.View().Content)
			goldenText := strings.TrimRight(base, "\n") + "\n"
			checkSize(t, base, size)
			if strings.Contains(base, "error.unknown") || !strings.Contains(base, "Knot") {
				t.Fatal(base)
			}
			if os.Getenv("KNOT_UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll("testdata", 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join("testdata", fmt.Sprintf("servers-%s-%dx%d.txt", language, size[0], size[1])), []byte(goldenText), 0644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("servers-%s-%dx%d.txt", language, size[0], size[1])))
			if err != nil {
				t.Fatal(err)
			}
			if string(golden) != goldenText {
				t.Fatal("prototype layout changed; review golden")
			}
			m.OpenModal(&Modal{Kind: HostTrust, Title: "Trust 主机", Body: "服务器 alias\n主机 host\n密钥类型 ed25519\n指纹 SHA256:very-long-e\u0301中文"})
			m.notify(NoticeMsg{ID: "error", Text: strings.Repeat("错误 error ", 80), Error: true})
			modal := ansi.Strip(m.View().Content)
			checkSize(t, modal, size)
			if !strings.HasPrefix(modal, "Knot") {
				t.Fatal("overlay ignored coordinates", modal)
			}
			if !strings.Contains(modal, "[Enter]") || !strings.Contains(modal, "[Esc]") || !strings.Contains(modal, "SHA256") {
				t.Fatal("confirmation obscured", modal)
			}
		}
	}
}
func checkSize(t *testing.T, view string, size [2]int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > size[1] {
		t.Fatal("height overflow", len(lines))
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > size[0] {
			t.Fatal("width overflow", line)
		}
	}
}

func TestCancelAndUnavailableStates(t *testing.T) {
	m := testModel(t, "servers")
	_, cmd := m.Update(press("ctrl+c"))
	if cmd == nil || m.exitCode != entry.ExitCanceled {
		t.Fatal("cancel exit policy")
	}
	for _, tc := range []struct {
		code  string
		state ListState
	}{{"OFFLINE", Offline}, {"UNAUTHORIZED", AuthFailed}, {"PROTOCOL_ERROR", ReadError}} {
		m.generation++
		m.Update(loadResult{generation: m.generation, page: m.page, err: coreclient.Failure(tc.code)})
		if m.state != tc.state || m.hasSnapshot || len(m.rows()) != 0 {
			t.Fatal("unconfirmed resources displayed", tc, m.state)
		}
	}
	m.state = Ready
	m.page = entry.Sync
	for _, key := range []string{"n", "e", "d", "p"} {
		_, cmd := m.Update(press(key))
		if cmd != nil {
			t.Fatal("sync gate leaked business key", key)
		}
	}
}

func TestRefreshDoesNotRebuildDraftAndFormFailureReturnsFocus(t *testing.T) {
	m := testModel(t, "keys")
	form := NewForm(TextField("alias", "Alias", "draft", true, false), MultilineField("key", "Material", "line 1\nline 2", true))
	modal := &Modal{Kind: EditForm, Title: "Key", Form: form}
	m.OpenModal(modal)
	form.Focus(1)
	m.generation = 1
	m.Update(loadResult{generation: 1, page: entry.Keys, snapshot: snapshot(t, "instance-a")})
	if m.modal != modal || form.Index != 1 || form.Fields[1].Value() != "line 1\nline 2" {
		t.Fatal("refresh rebuilt draft")
	}
	form.Busy = true
	m.Update(FormResult{ModalID: modal.ID, InstanceID: "instance-a", Fields: map[string]string{"alias": "required"}})
	if form.Busy || form.Index != 0 || form.Fields[0].Error == "" {
		t.Fatal("field error focus not restored")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.notify(NoticeMsg{ID: "warning", Text: "warning notification", Error: true})
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "warning notification") || !strings.Contains(view, "[Ctrl+s]") {
		t.Fatal("notice/form hints missing", view)
	}
}

func TestFormSuccessDoesNotCancelCompletedOperation(t *testing.T) {
	m := testModel(t, "keys")
	canceled := 0
	open := func() *Modal {
		modal := &Modal{Kind: EditForm, Form: NewForm(TextField("alias", "Alias", "draft", true, false)), OnCancel: func() tea.Msg {
			canceled++
			return nil
		}}
		m.OpenModal(modal)
		return modal
	}
	modal := open()
	_, cmd := m.Update(FormResult{ModalID: modal.ID, InstanceID: "instance-a", Saved: true})
	if cmd != nil {
		cmd()
	}
	if m.modal != nil || canceled != 0 || m.focus != OperationsFocus {
		t.Fatal("successful save ran cancellation cleanup or failed to restore focus")
	}
	open()
	_, cmd = m.Update(press("esc"))
	if cmd != nil {
		cmd()
	}
	if m.modal != nil || canceled != 1 {
		t.Fatal("explicit dismissal failed to cancel the operation")
	}
}
