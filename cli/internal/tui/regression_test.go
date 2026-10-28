package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

func execute(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, child := range batch {
			execute(child)
		}
	}
}

func TestWarningsAndHandshakeFollowCoreContract(t *testing.T) {
	m := testModel(t, "servers")
	// No reader command is executed: exercise event routing independently of I/O.
	m.stream = &coreclient.Stream{Items: make(chan coreclient.StreamItem)}
	t.Cleanup(func() { m.stream = nil })
	send := func(event coreclient.Event) {
		m.Update(eventMsg{epoch: m.streamEpoch, item: coreclient.StreamItem{Event: event}})
	}
	generation := m.generation
	send(coreclient.Event{Type: "core.snapshot", APIVersion: "v1"})
	if m.loading || m.generation != generation {
		t.Fatal("handshake caused redundant config reload")
	}
	for _, level := range []string{"warn", "warning", "error"} {
		send(coreclient.Event{Type: "session.error", ResourceID: "failed-agent-attempt", Level: level, Data: json.RawMessage(`{"state":"failed","error":"agent forwarding unavailable: agent_unavailable"}`)})
		if len(m.notices) != 0 {
			t.Fatal("session failure duplicated as a generic core warning", level)
		}
	}
	for _, level := range []string{"warn", "warning", "error"} {
		send(coreclient.Event{Type: "session.warning", ResourceID: level, Level: level, Data: json.RawMessage(`{"warning":{"kind":"credential_save_failed"}}`)})
		if got := m.notices[len(m.notices)-1]; !got.Error || !strings.Contains(got.Text, "credential_save_failed") {
			t.Fatal("warning lost", level, got)
		}
	}
	if len(m.notices) != 3 {
		t.Fatal("core warn was discarded")
	}
	send(coreclient.Event{Type: "session.connected"})
	if !m.loading || m.generation != generation+1 {
		t.Fatal("real recent-use change did not refresh config")
	}
}

func TestReenteringTUIRearmsRetainedNotificationTimers(t *testing.T) {
	m := testModel(t)
	// Simulate handing off to native SSH before the first program's timer
	// could dismiss a warning. The same model is then used by a new program.
	m.notify(NoticeMsg{ID: "warning:previous", Text: "old warning", Error: true})
	m.notify(NoticeMsg{ID: "active", Text: "still running", Persistent: true})
	cmds, ok := m.Init()().(tea.BatchMsg)
	if !ok || len(cmds) != 3 {
		t.Fatal("expected discovery, polling and one retained notice timer")
	}
	// Leave discovery and polling unexecuted; only the resumed timer matters.
	message, ok := cmds[2]().(dismissMsg)
	if !ok || message.id != "warning:previous" {
		t.Fatal("retained notification has no dismissal command")
	}
	m.Update(message)
	if len(m.notices) != 1 || m.notices[0].ID != "active" {
		t.Fatal("warning stuck after returning from SSH, or active notice expired")
	}
}

func TestCtrlCExitsSearchAndNonexclusiveModals(t *testing.T) {
	for _, kind := range []ModalKind{"", Information, EditForm, Picker, Draft} {
		t.Run(string(kind), func(t *testing.T) {
			m := testModel(t, "servers")
			if kind == "" {
				m.Update(press("/"))
			} else {
				m.OpenModal(&Modal{Kind: kind})
			}
			_, cmd := m.Update(press("ctrl+c"))
			if cmd == nil || m.exitCode != entry.ExitCanceled {
				t.Fatal("Ctrl+c was swallowed")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("Ctrl+c did not quit")
			}
		})
	}
}

func TestBackgroundRetryPreservesFailureState(t *testing.T) {
	for _, code := range []string{"OFFLINE", "UNAUTHORIZED", "PROTOCOL_ERROR"} {
		m := testModel(t, "servers")
		m.Update(loadResult{generation: m.generation, page: m.page, err: coreclient.Failure(code)})
		state := m.state
		m.Update(pollMsg{})
		if m.state != state || !m.loading || m.hasSnapshot || m.lastError == nil {
			t.Fatal("background retry obscured failure", code)
		}
		m.Update(press("r"))
		if m.state != state {
			t.Fatal("removed refresh shortcut changed failure state")
		}
	}
}

func TestHealthNoticesOnlyOnTransitions(t *testing.T) {
	m := testModel(t)
	load := func(status string) {
		fresh := snapshot(t, "instance-a")
		fresh.Health.Status = status
		m.Update(loadResult{generation: m.generation, page: m.page, snapshot: fresh})
	}
	load("degraded")
	if len(m.notices) != 1 {
		t.Fatal("initial degradation missing")
	}
	m.removeNotice("health")
	load("degraded")
	if len(m.notices) != 0 {
		t.Fatal("unchanged health repeated its notification")
	}
	load("ok")
	load("degraded")
	if len(m.notices) != 1 {
		t.Fatal("new degradation not reported")
	}
	load("ok")
	if len(m.notices) != 0 {
		t.Fatal("recovery retained stale degradation")
	}
}

func TestFailedStreamOpenCancelsItsContext(t *testing.T) {
	m := testModel(t)
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelStream, m.streamOpening = cancel, true
	epoch := m.streamEpoch
	m.Update(streamResult{epoch: epoch, err: coreclient.Failure("EVENTS_UNAVAILABLE")})
	if ctx.Err() == nil || m.cancelStream != nil || m.streamOpening || m.streamEpoch == epoch {
		t.Fatal("failed open retained context or accepted late results")
	}
}

func TestActiveNoticesSurviveCapacityAndCanFinish(t *testing.T) {
	m := testModel(t)
	for i := 0; i < 20; i++ {
		m.notify(NoticeMsg{ID: fmt.Sprint(i), Text: "active", Persistent: true})
	}
	m.notify(NoticeMsg{ID: "transient", Text: "final"})
	if len(m.notices) != 21 {
		t.Fatal("active operation evicted or warning suppressed")
	}
	m.notify(NoticeMsg{ID: "0", Text: "done"})
	if m.notices[0].Persistent || m.notices[0].Text != "done" {
		t.Fatal("active notification could not complete")
	}
	m.Update(dismissMsg{id: "0", revision: m.notices[0].revision})
	if len(m.notices) != 20 {
		t.Fatal("completed notification did not expire")
	}
	for i := 0; i < 30; i++ {
		m.notify(NoticeMsg{ID: fmt.Sprintf("final-%d", i), Text: "done"})
	}
	if len(m.notices) != 19+16 {
		t.Fatal("final notice history is not independently bounded")
	}
	for i := 1; i < 20; i++ {
		if m.notices[i-1].ID != fmt.Sprint(i) || !m.notices[i-1].Persistent {
			t.Fatal("active notice lost while pruning final history")
		}
	}
}

func TestEmptyFormAndNilSubmissionStayUsable(t *testing.T) {
	m := testModel(t, "keys")
	form := NewForm()
	m.OpenModal(&Modal{Kind: EditForm, Form: form, OnSubmit: func(map[string]string) tea.Cmd { t.Fatal("empty form submitted"); return nil }})
	for _, msg := range []tea.Msg{press("a"), press("enter"), press("tab"), press("shift+tab"), press("ctrl+s"), tea.PasteMsg{Content: "pasted"}, struct{}{}} {
		m.Update(msg)
		if form.Index != -1 || form.Busy {
			t.Fatal("empty form gained an invalid field or became busy")
		}
		_ = m.View()
	}
	m.Update(press("esc"))
	if m.modal != nil {
		t.Fatal("empty form cannot close")
	}
	form = NewForm(TextField("alias", "Alias", "draft", true, false))
	m.OpenModal(&Modal{Kind: EditForm, Form: form, OnSubmit: func(map[string]string) tea.Cmd { return nil }})
	m.Update(press("ctrl+s"))
	if form.Busy {
		t.Fatal("nil command left form permanently busy")
	}
}

func TestReplacingDraftCancelsOldOwnerAndPreservesAnchor(t *testing.T) {
	m := testModel(t, "keys")
	canceled := 0
	old := &Modal{Kind: EditForm, Form: NewForm(TextField("alias", "Alias", "a", true, false)), OnCancel: func() tea.Msg { canceled++; return nil }}
	m.OpenModal(old)
	m.Update(press("b"))
	m.Update(press("esc"))
	if m.modal.Kind != Draft {
		t.Fatal("expected draft prompt")
	}
	execute(m.OpenModal(&Modal{Kind: Information, Body: "replacement"}))
	if canceled != 1 {
		t.Fatal("old modal owner was not canceled exactly once")
	}
	m.Update(press("esc"))
	if m.modal != nil || m.focus != OperationsFocus {
		t.Fatal("replacement lost focus anchor")
	}
}

func TestConfirmationsRejectStaleInstancesAndOfflineState(t *testing.T) {
	for _, kind := range []ModalKind{HostTrust, TransferConfirm, SFTPCloseConfirm, Picker} {
		for _, state := range []ListState{Ready, Offline} {
			m := testModel(t, "servers")
			calls := 0
			modal := &Modal{Kind: kind, Choices: []Choice{{ID: "old-id"}}, OnConfirm: func(string) tea.Cmd { calls++; return nil }}
			m.OpenModal(modal)
			m.state = state
			m.snapshot.Runtime.InstanceID = "new-instance"
			m.Update(press("enter"))
			if kind == Picker {
				m.Update(press("1"))
			}
			if calls != 0 || m.modal != modal || len(m.notices) == 0 {
				t.Fatal("stale confirmation escaped owner guard", kind, state)
			}
			m.Update(press("esc"))
			if m.modal != nil {
				t.Fatal("stale confirmation cannot cancel")
			}
		}
	}
}

func TestInformationScrollClampsAtEndAndOnResize(t *testing.T) {
	m := testModel(t)
	m.OpenModal(&Modal{Kind: Information, Title: "Details", Body: strings.Repeat("line\n", 50)})
	for i := 0; i < 100; i++ {
		m.Update(press("down"))
	}
	end := m.modal.Scroll
	if end != m.informationScrollLimit() || end == 0 {
		t.Fatal("scroll exceeded body")
	}
	m.Update(press("up"))
	if m.modal.Scroll != end-1 {
		t.Fatal("up did not immediately scroll back from end")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 100})
	if m.modal.Scroll != 0 {
		t.Fatal("resize retained out-of-range scroll")
	}
}

func TestDisplayEscapesFormatCharactersWithoutChangingInput(t *testing.T) {
	value := "alias\u202ehidden\u200b\ufeff"
	if got := Display(value); got != `alias\u202ehidden\u200b\ufeff` {
		t.Fatal("format characters escaped incorrectly", got)
	}
	m := testModel(t, "servers")
	m.Update(press("/"))
	m.Update(tea.PasteMsg{Content: value})
	if m.search.Value() != value || strings.ContainsAny(m.View().Content, "\u202e\u200b\ufeff") {
		t.Fatal("input changed or format character reached the terminal")
	}
	field := TextField("alias", "Alias", value, true, false)
	m.OpenModal(&Modal{Kind: EditForm, Form: NewForm(field)})
	if m.modal.Form.Values()["alias"] != value || strings.ContainsAny(m.View().Content, "\u202e\u200b\ufeff") {
		t.Fatal("form changed literal data or rendered format controls")
	}
}

func populateAllPages(m *Model) {
	m.snapshot.Config.Settings.SyncPasswordSet = true
	m.snapshot.Config.Servers[0].AuthMethod = "key"
	m.snapshot.Config.Servers[0].KeyID = "key"
	m.snapshot.Config.Servers[1].AuthMethod = "password"
	m.snapshot.Config.SyncProviders = []coreclient.SyncProvider{
		{ID: "dav", Alias: "备份e\u0301", Type: "webdav", URL: "https://backup.example/config", Username: "user", PasswordSet: true},
		{ID: "s3", Alias: "s3-main", Type: "s3", Bucket: "backup", Key: "config.toml.enc", Region: "auto", AccessKeyIDSet: true, SecretAccessKeySet: true, Default: true},
	}
}

func TestDynamicCatalogKeysCoverActualPagesAndActions(t *testing.T) {
	m := testModel(t)
	populateAllPages(m)
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		tr := i18n.New(language)
		check := func(key string) {
			if _, err := tr.Render(i18n.Key(key), nil); err != nil {
				t.Error(language, key, err)
			}
		}
		for _, page := range pages {
			m.page, m.tr = page, tr
			check("menu." + string(page))
			for _, action := range primaryActions[page] {
				check("ui.action." + action)
			}
			for _, row := range m.allRows() {
				for _, detail := range row.Details {
					check("ui." + detail[0])
				}
			}
			if strings.Contains(m.View().Content, "Operation failed") || strings.Contains(m.View().Content, "操作失败") {
				t.Fatal("dynamic key silently fell back", page)
			}
		}
		for _, method := range []string{"agent", "key", "password"} {
			check("ui.auth." + method)
		}
	}
}

func TestOtherPagesBilingualGolden(t *testing.T) {
	for _, page := range []entry.Page{entry.Keys, entry.Proxies, entry.Sync} {
		for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
			for _, size := range [][2]int{{80, 24}, {120, 35}} {
				m := testModel(t, string(page))
				populateAllPages(m)
				m.tr = i18n.New(language)
				m.ensureSelection()
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				raw := ansi.Strip(m.View().Content)
				checkSize(t, raw, size)
				view := strings.TrimRight(raw, "\n") + "\n"
				path := filepath.Join("testdata", fmt.Sprintf("%s-%s-%dx%d.txt", page, language, size[0], size[1]))
				if os.Getenv("KNOT_UPDATE_GOLDEN") == "1" {
					if err := os.WriteFile(path, []byte(view), 0644); err != nil {
						t.Fatal(err)
					}
				}
				golden, err := os.ReadFile(path)
				if err != nil || string(golden) != view {
					t.Fatal("page layout changed", path, err)
				}
			}
		}
	}
}
