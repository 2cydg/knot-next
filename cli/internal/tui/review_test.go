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

func TestMenuEntryShortcutsExecuteAndRestoreOperationsFocus(t *testing.T) {
	for _, page := range pages {
		for _, passwordSet := range []bool{false, true} {
			for _, binding := range []string{"/", "n", "s"} {
				t.Run(string(page)+"/"+binding+"/"+map[bool]string{false: "unset", true: "set"}[passwordSet], func(t *testing.T) {
					m := testModel(t)
					m.page = page
					m.snapshot.Config.Settings.SyncPasswordSet = passwordSet
					gated := page == entry.Sync && !passwordSet
					allowed := page != entry.Settings && ((!gated && (binding == "/" || binding == "n")) || (gated && binding == "s"))
					m.Update(press(binding))
					if !allowed {
						if m.focus != MenuFocus || m.modal != nil {
							t.Fatal("menu consumed an unrelated business key")
						}
						return
					}
					if binding == "/" {
						if m.focus != SearchFocus || m.search.Value() != "" {
							t.Fatal("search shortcut did not focus an empty input")
						}
						m.Update(press("n"))
						if m.modal != nil || m.search.Value() != "n" {
							t.Fatal("search typing escaped into a business action")
						}
						return
					}
					if m.modal == nil || m.modal.Form == nil || m.focus != OperationsFocus {
						t.Fatal("entry shortcut did not open its form")
					}
					if binding == "n" && (m.modal.Edit == nil || !m.modal.Edit.Create) {
						t.Fatal("new shortcut did not create a draft")
					}
					if binding == "s" && m.modal.Title != "Sync password" {
						t.Fatal("password shortcut opened the wrong form")
					}
					m.Update(press("esc"))
					if m.modal != nil || m.focus != OperationsFocus {
						t.Fatal("closing an entry form did not restore operations focus")
					}
				})
			}
		}
	}
}

func TestPageHintsStayFixedAcrossFocusAndRemovedKeysDoNothing(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, page := range pages {
			for _, passwordSet := range []bool{false, true} {
				m := testModel(t)
				m.page, m.tr = page, i18n.New(language)
				m.snapshot.Config.Settings.SyncPasswordSet = passwordSet
				header := func() string {
					return strings.SplitN(ansi.Strip(m.mainContent(72, 22)), "─", 2)[0]
				}
				want := header()
				for _, focus := range []Focus{MenuFocus, OperationsFocus, SearchFocus} {
					m.focus = focus
					if got := header(); got != want {
						t.Fatalf("hints changed with focus: %q != %q", got, want)
					}
				}
				for _, removed := range []string{"? Help", "r Refresh", "i Details", "←/Esc", "↑↓ Menu", "↑↓ 菜单"} {
					if strings.Contains(want, removed) {
						t.Fatal("removed hint still visible", want)
					}
				}
				if page != entry.Settings && (page != entry.Sync || passwordSet) {
					if !strings.Contains(want, "/ ") {
						t.Fatal("search hint missing", want)
					}
				}
				m.focus = OperationsFocus
				generation := m.generation
				for _, binding := range []string{"?", "r", "i"} {
					_, cmd := m.Update(press(binding))
					if cmd != nil || m.modal != nil || m.generation != generation {
						t.Fatal("removed shortcut executed", binding)
					}
				}
				if page == entry.Servers {
					if strings.Contains(want, "  t ") {
						t.Fatal("forwarding hint still visible")
					}
					_, cmd := m.Update(press("t"))
					if cmd != nil || m.modal != nil {
						t.Fatal("removed forwarding shortcut executed")
					}
				}
			}
		}
	}
}

func TestEmptyAndFilteredListsKeepTableHeaders(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, page := range []entry.Page{entry.Servers, entry.Keys, entry.Proxies, entry.Sync} {
			for _, size := range [][2]int{{80, 24}, {120, 35}} {
				m := testModel(t)
				m.page, m.tr = page, i18n.New(language)
				populateAllPages(m)
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				for _, filtered := range []bool{true, false} {
					if filtered {
						m.search.SetValue("no-matching-resource")
					} else {
						m.search.SetValue("")
						m.snapshot.Config = coreclient.Summary{}
						m.snapshot.Config.Settings.SyncPasswordSet = true
					}
					m.ensureSelection()
					view := ansi.Strip(m.View().Content)
					checkSize(t, view, size)
					if !strings.Contains(view, m.tr.Message("ui.alias")) || !strings.Contains(view, "┌") || !strings.Contains(view, "┘") {
						t.Fatal("empty table or headers missing", view)
					}
					for _, key := range []string{"ui.list", "ui.empty", "ui.empty_hint"} {
						if strings.Contains(view, m.tr.Message(key)) {
							t.Fatal("obsolete empty copy visible", key)
						}
					}
				}
			}
		}
	}
}

func TestFormNavigationPreservesVisibleValuesAndScrollsToFocusedField(t *testing.T) {
	for _, binding := range []string{"tab", "enter"} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			m := testModel(t, "servers")
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.openEditor(coreclient.Servers, true, false)
			m.Update(tea.PasteMsg{Content: "saved-alias"})
			m.Update(press(binding))
			view := ansi.Strip(m.modalView())
			if !strings.Contains(view, "saved-alias") || !strings.Contains(view, "Host *") || strings.Contains(view, "[Basic]") {
				t.Fatal("advancing hid a field that still fits, or tabs still use brackets", view)
			}
			if !strings.Contains(view, "└") || !strings.Contains(view, "┐") {
				t.Fatal("input borders missing", view)
			}
			m.Update(tea.PasteMsg{Content: "saved-host"})
			form := m.modal.Form
			for form.next(1) >= 0 && form.Fields[form.next(1)].Section == "basic" {
				m.Update(press(binding))
				view = ansi.Strip(m.modalView())
				if !strings.Contains(view, form.Fields[form.Index].Label) {
					t.Fatal("focused field scrolled out of view", view)
				}
				checkSize(t, ansi.Strip(m.View().Content), size)
			}
			for form.next(-1) >= 0 {
				m.Update(press("shift+tab"))
			}
			view = ansi.Strip(m.modalView())
			if !strings.Contains(view, "saved-alias") || !strings.Contains(view, "saved-host") {
				t.Fatal("scrolling back lost entered values", view)
			}
		}
	}
}

func TestResourceEditorTitlesNameResourceInBothLanguages(t *testing.T) {
	for _, tc := range []struct {
		kind   coreclient.Resource
		en, zh [2]string
	}{
		{coreclient.Servers, [2]string{"New server", "Edit server"}, [2]string{"新增服务器", "编辑服务器"}},
		{coreclient.Keys, [2]string{"New key", "Edit key"}, [2]string{"新增密钥", "编辑密钥"}},
		{coreclient.Proxies, [2]string{"New proxy", "Edit proxy"}, [2]string{"新增代理", "编辑代理"}},
		{coreclient.Providers, [2]string{"New sync service", "Edit sync service"}, [2]string{"新增同步服务", "编辑同步服务"}},
	} {
		for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
			for i, create := range []bool{true, false} {
				m := testModel(t)
				m.tr = i18n.New(language)
				m.showEditor(resourceDraft{Kind: tc.kind, Create: create})
				want := tc.en[i]
				if language == i18n.Chinese {
					want = tc.zh[i]
				}
				if m.modal.Title != want {
					t.Fatalf("title = %q, want %q", m.modal.Title, want)
				}
			}
		}
	}
}
