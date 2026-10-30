package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func TestSettingsSaveLanguageWithoutCore(t *testing.T) {
	for _, state := range []ListState{Ready, Loading, Offline, AuthFailed, ReadError} {
		t.Run(string(state), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			m := testModel(t)
			m.state = state
			m.Update(press("end"))
			if m.page != entry.Settings || pages[len(pages)-2] != entry.Sync {
				t.Fatal("settings must follow Sync in the menu")
			}
			if state != Ready {
				m.hasSnapshot = false
				m.snapshot = coreclient.Snapshot{}
			}
			m.Update(press("enter"))
			generation := m.generation
			m.Update(press("down"))
			m.Update(press("right"))
			if m.tr.Language() != i18n.English || m.draftLanguage() != i18n.Chinese || !m.settingsDirty() {
				t.Fatal("selection must remain a draft until saved")
			}
			path, err := preferences.Path()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("changing a selection wrote client settings", err)
			}
			_, cmd := m.Update(press("ctrl+s"))
			if cmd == nil || !m.settingsBusy || m.tr.Language() != i18n.English {
				t.Fatal("save must await the file write before applying language")
			}
			m.Update(cmd())
			if m.settingsBusy || m.settingsDirty() || m.tr.Language() != i18n.Chinese || m.generation != generation || m.focus != MenuFocus || m.settingsFieldFocus {
				t.Fatal("successful local save did not apply without core requests")
			}
			saved, err := preferences.Load()
			if err != nil || saved.Language != i18n.Chinese {
				t.Fatal("saved language did not survive reloading", saved, err)
			}
			// Inspect the underlying page after its temporary save notice expires.
			m.removeNotice("settings")
			view := ansi.Strip(m.View().Content)
			for _, want := range []string{"客户端设置", "核心设置", "语言", "中文", "[] 切换页签", "Ctrl+s 保存", "‹ 中文 ›"} {
				if !strings.Contains(view, want) {
					t.Fatal("settings did not update its language", want, view)
				}
			}
			if strings.Contains(view, "English") || strings.Contains(view, "core 离线") || strings.Contains(view, "← 菜单") || strings.Contains(view, "空格/Enter") || strings.Contains(view, "▾") {
				t.Fatal("settings must show a single local choice", view)
			}
			m.Update(press("home"))
			m.showEditor(resourceDraft{Kind: coreclient.Servers, Create: true})
			if m.modal.Title != "新增服务器" {
				t.Fatal("new forms did not use the saved language")
			}
		})
	}
}

func TestSettingsTabsAndLanguageKeys(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			m := testModel(t)
			m.page, m.focus, m.tr = entry.Settings, OperationsFocus, i18n.New(language)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.Update(press("right"))
			if m.settingsDirty() || m.focus != OperationsFocus || m.settingsFieldFocus {
				t.Fatal("right arrow outside the control must not change language")
			}
			m.Update(press("left"))
			if m.focus != MenuFocus || m.settingsDirty() {
				t.Fatal("left arrow outside the control must restore the menu")
			}
			m.Update(press("right"))
			m.Update(press("down"))
			if !m.settingsFieldFocus || m.focus != OperationsFocus {
				t.Fatal("down must focus the language control")
			}
			for _, key := range []string{" ", "enter"} {
				m.Update(press(key))
				if m.settingsDirty() {
					t.Fatal("Space and Enter must not change language", key)
				}
			}
			for _, key := range []string{"left", "right"} {
				before := m.draftLanguage()
				m.Update(press(key))
				if m.draftLanguage() == before || m.tr.Language() != language || m.focus != OperationsFocus || !m.settingsFieldFocus {
					t.Fatal("arrows on the control must only change the draft", key)
				}
			}
			m.Update(press("up"))
			m.Update(press("left"))
			if m.focus != MenuFocus || m.settingsFieldFocus || m.settingsDirty() {
				t.Fatal("up must leave the control so left can restore the menu")
			}
			m.Update(press("right"))
			m.Update(press("enter"))
			if !m.settingsFieldFocus || m.settingsDirty() {
				t.Fatal("Enter must focus the control without changing its value")
			}
			for _, key := range []string{"]", "[", "】", "【", "］", "［"} {
				before := m.settingsTab
				m.Update(press(key))
				if m.settingsTab == before || m.focus != OperationsFocus || m.settingsFieldFocus {
					t.Fatal("bracket must switch tabs and leave the control", key)
				}
			}
			m.Update(press("]"))
			for _, key := range []string{"right", "down", " ", "n", "e", "s", "/"} {
				_, cmd := m.Update(press(key))
				if cmd != nil {
					t.Fatal("core settings triggered an operation", key)
				}
			}
			view := ansi.Strip(m.View().Content)
			if m.settingsDirty() || m.modal != nil || m.settingsFieldFocus || strings.Contains(view, "‹") {
				t.Fatal("core settings must not change language", view)
			}
			m.Update(press("["))
			view = ansi.Strip(m.View().Content)
			if m.settingsTab != 0 || !strings.Contains(view, "‹") || strings.Contains(view, "▾") {
				t.Fatal("client settings must show a choice control", view)
			}
			for _, removed := range []string{"← Menu", "← 菜单", "Space/Enter", "空格/Enter"} {
				if strings.Contains(view, removed) {
					t.Fatal("removed shortcut is still shown", removed, view)
				}
			}
			checkSize(t, view, size)
			m.Update(press("down"))
			m.Update(press("tab"))
			if m.focus != MenuFocus || m.settingsFieldFocus {
				t.Fatal("Tab must leave the control and restore the menu")
			}
		}
	}
}

func TestSettingsDraftConfirmationAndSaveFailure(t *testing.T) {
	for _, leaveKey := range []string{"esc", "left", "tab", "shift+tab"} {
		m := testModel(t)
		m.page, m.focus = entry.Settings, OperationsFocus
		m.Update(press("down"))
		m.Update(press("right"))
		if leaveKey == "left" {
			m.Update(press("up"))
		}
		m.Update(press(leaveKey))
		if m.modal == nil || m.modal.Kind != Draft || m.modal.Form != nil {
			t.Fatal("changed settings need the shared draft confirmation", leaveKey)
		}
		m.Update(press("esc"))
		if m.modal != nil || !m.settingsDirty() || m.focus != OperationsFocus {
			t.Fatal("Esc must return to the unchanged draft")
		}
		m.Update(press(leaveKey))
		m.Update(press("n"))
		if m.modal != nil || m.settingsDirty() || m.focus != MenuFocus || m.tr.Language() != i18n.English {
			t.Fatal("N must discard the draft and restore the menu")
		}
	}
	m := testModel(t)
	m.page, m.focus, m.state = entry.Settings, OperationsFocus, Offline
	writes := 0
	m.saveSettings = func(settings preferences.Settings) error {
		writes++
		if settings.Language != i18n.Chinese {
			t.Fatal("wrong language was submitted")
		}
		return errors.New("disk unavailable")
	}
	m.Update(press("down"))
	m.Update(press("right"))
	m.Update(press("esc"))
	_, cmd := m.Update(press("y"))
	if cmd == nil || !m.settingsBusy || m.modal != nil {
		t.Fatal("Y must start the local save without a live core")
	}
	for _, key := range []string{"enter", "ctrl+s", "]", "left", "esc"} {
		m.Update(press(key))
	}
	m.Update(cmd())
	if writes != 1 || m.settingsBusy || !m.settingsDirty() || m.tr.Language() != i18n.English || m.focus != OperationsFocus {
		t.Fatal("save failure must preserve the selected draft and old language")
	}
	m.saveSettings = func(preferences.Settings) error { writes++; return nil }
	m.Update(press("esc"))
	_, cmd = m.Update(press("y"))
	m.Update(cmd())
	if writes != 2 || m.modal != nil || m.settingsDirty() || m.focus != MenuFocus || m.tr.Language() != i18n.Chinese {
		t.Fatal("Y must apply a successful save and restore the menu")
	}
}

func TestServerDetailsAndUnselectedMarker(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		m := testModel(t, "servers")
		m.tr = i18n.New(language)
		populateAllPages(m)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 35})
		rows := m.rows()
		for _, row := range rows {
			for _, field := range row.Details {
				switch field[0] {
				case "auth_method", "proxy", "jumps":
				default:
					t.Fatal("obsolete server detail", field[0])
				}
			}
		}
		view := ansi.Strip(m.View().Content)
		if strings.Contains(view, "· ") || strings.Count(view, "alpha") != 1 {
			t.Fatal("server list marker or repeated detail alias remains", view)
		}
		if !strings.Contains(view, "┌─ "+m.pageName()+" ─") || !strings.Contains(view, "┌─ "+m.tr.Message("ui.detail")+" ─") {
			t.Fatal("page and details titles must appear on the left of their top borders", view)
		}
	}
}
