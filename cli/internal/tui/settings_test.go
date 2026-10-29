package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

func TestSettingsMenuAndImmediateLanguageChangeWithoutCore(t *testing.T) {
	for _, state := range []ListState{Ready, Loading, Offline, AuthFailed, ReadError} {
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
		_, cmd := m.Update(press("right"))
		if cmd != nil || m.generation != generation || m.tr.Language() != i18n.Chinese || m.modal != nil {
			t.Fatal("language selection must apply immediately without requests or a form")
		}
		view := ansi.Strip(m.View().Content)
		for _, want := range []string{"客户端设置", "核心设置", "语言", "English", "中文", "[] 切换页签"} {
			if !strings.Contains(view, want) {
				t.Fatal("settings did not update its language", want, view)
			}
		}
		if strings.Contains(view, "Ctrl+s") || strings.Contains(view, "core 离线") {
			t.Fatal("local settings must be available without saving or core")
		}
		m.Update(press("esc"))
		m.Update(press("home"))
		if m.page != entry.Servers || m.pageName() != "服务器" || m.tr.ProtocolError("OFFLINE") != i18n.New(i18n.Chinese).ProtocolError("OFFLINE") {
			t.Fatal("language did not carry into other pages and errors")
		}
		m.showEditor(resourceDraft{Kind: coreclient.Servers, Create: true})
		if m.modal.Title != "新增服务器" {
			t.Fatal("new forms did not use the selected language")
		}
	}
}

func TestSettingsTabsAndLanguageKeys(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			m := testModel(t)
			m.page, m.focus, m.tr = entry.Settings, OperationsFocus, i18n.New(language)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			for _, key := range []string{"left", "right", " ", "enter"} {
				before := m.tr.Language()
				m.Update(press(key))
				if m.tr.Language() == before {
					t.Fatal("language choice ignored", key)
				}
			}
			m.Update(press("]"))
			if m.settingsTab != 1 {
				t.Fatal("next tab did not select core settings")
			}
			before := m.tr.Language()
			for _, key := range []string{"left", "right", " ", "enter", "n", "e", "s", "/"} {
				m.Update(press(key))
			}
			view := ansi.Strip(m.View().Content)
			if m.tr.Language() != before || m.modal != nil || m.focus != OperationsFocus || strings.Contains(view, "English") {
				t.Fatal("core settings must remain empty", view)
			}
			checkSize(t, view, size)
			m.Update(press("["))
			if m.settingsTab != 0 || !strings.Contains(ansi.Strip(m.View().Content), "English") {
				t.Fatal("previous tab did not restore client settings")
			}
			checkSize(t, ansi.Strip(m.View().Content), size)
			m.Update(press("tab"))
			if m.focus != MenuFocus {
				t.Fatal("Tab did not restore menu focus")
			}
		}
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
		detailLines := strings.Split(view, "\n")
		for i, line := range detailLines {
			if strings.Contains(line, m.tr.Message("ui.detail")) && i+1 < len(detailLines) && strings.Contains(detailLines[i+1], m.tr.Message("ui.auth_method")) {
				t.Fatal("detail heading needs a separator before its fields")
			}
		}
	}
}
