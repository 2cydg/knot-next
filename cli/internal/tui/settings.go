package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

type settingsSaved struct {
	language i18n.Language
	err      error
}

func (m *Model) draftLanguage() i18n.Language {
	if m.settingsDraft != "" {
		return m.settingsDraft
	}
	return m.tr.Language()
}

func (m *Model) settingsDirty() bool { return m.draftLanguage() != m.tr.Language() }

func (m *Model) persistSettings() tea.Cmd {
	if m.settingsBusy {
		return nil
	}
	m.settingsBusy = true
	language, save := m.draftLanguage(), m.saveSettings
	return func() tea.Msg {
		return settingsSaved{language: language, err: save(preferences.Settings{Language: language})}
	}
}

func (m *Model) leaveSettings() tea.Cmd {
	if !m.settingsDirty() {
		m.settingsFieldFocus = false
		m.focus = MenuFocus
		return nil
	}
	return m.OpenModal(&Modal{
		Kind: Draft, Title: m.tr.Message("ui.draft"), Body: m.tr.Message("ui.draft_hint"),
		OnConfirm: func(string) tea.Cmd { return m.persistSettings() },
		OnDiscard: func() tea.Cmd {
			m.settingsDraft = ""
			m.settingsFieldFocus = false
			m.focus = MenuFocus
			return nil
		},
	})
}

func (m *Model) settingsKey(binding string) tea.Cmd {
	if m.settingsBusy {
		return nil
	}
	switch binding {
	case "esc", "tab", "shift+tab":
		return m.leaveSettings()
	case "[", "［", "【", "]", "］", "】":
		// Accept bracket punctuation produced by both English and Chinese IMEs.
		m.settingsTab = 1 - m.settingsTab
		m.settingsFieldFocus = false
	case "ctrl+s":
		if m.settingsTab == 1 {
			return m.openCoreConnection(false)
		}
		if m.settingsTab == 0 {
			return m.persistSettings()
		}
	case "down", "enter":
		if m.settingsTab == 1 && binding == "enter" {
			return m.openCoreConnection(false)
		}
		if m.settingsTab == 0 {
			m.settingsFieldFocus = true
		}
	case "up":
		m.settingsFieldFocus = false
	case "left", "right":
		if m.settingsTab == 0 && m.settingsFieldFocus {
			m.changeSettingsLanguage()
		} else if binding == "left" {
			return m.leaveSettings()
		}
	}
	return nil
}

func (m *Model) changeSettingsLanguage() {
	if m.draftLanguage() == i18n.Chinese {
		m.settingsDraft = i18n.English
	} else {
		m.settingsDraft = i18n.Chinese
	}
}

func (m *Model) settingsContent(width int) string {
	hints := m.tr.Message("manage.tabs_hint")
	if m.settingsTab == 0 {
		hints += "  " + m.tr.Message("settings.save_hint")
	}
	head := renderHints(hints, width) + "\n" + strings.Repeat("─", width)
	var tabs []string
	for i, key := range []string{"settings.client", "settings.core"} {
		style := lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
		if i == m.settingsTab {
			style = style.Foreground(green).Background(selected)
		}
		tabs = append(tabs, style.Render(m.tr.Message(key)))
	}
	body := head + "\n\n" + wrap(strings.Join(tabs, " "), width)
	if m.settingsTab == 0 {
		label := "English"
		if m.draftLanguage() == i18n.Chinese {
			label = "中文"
		}
		choiceWidth := min(26, width)
		value := fit("‹ "+label+" ›", max(1, choiceWidth-2))
		body += "\n\n" + m.tr.Message("settings.language") + "\n" + box(value, choiceWidth, 3, m.focus == OperationsFocus && m.settingsFieldFocus)
		if m.settingsDirty() {
			body += "\n" + lipgloss.NewStyle().Foreground(muted).Render(m.tr.Message("settings.unsaved"))
		}
	}
	if m.settingsTab == 1 {
		body += "\n\n" + m.tr.Message("core.edit_hint")
		if m.coreSettings.URL != "" {
			body += "\n\n" + m.tr.Message("core.url") + "\n" + Display(m.coreSettings.URL)
		}
	}
	if m.settingsBusy {
		body += "\n" + m.tr.Message("ui.busy")
	}
	return body
}
