package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"knot-cli/internal/i18n"
)

func (m *Model) settingsKey(binding string) tea.Cmd {
	switch binding {
	case "esc", "tab", "shift+tab":
		m.focus = MenuFocus
	case "[", "]":
		m.settingsTab = 1 - m.settingsTab
	case "left", "right", " ", "space", "enter":
		if m.settingsTab == 0 {
			language := i18n.Chinese
			if m.tr.Language() == i18n.Chinese {
				language = i18n.English
			}
			m.tr = i18n.New(language)
		}
	}
	return nil
}

func (m *Model) settingsContent(width int) string {
	hints := m.tr.Message("manage.tabs_hint")
	if m.settingsTab == 0 {
		hints += "  " + m.tr.Message("settings.language_hint")
	}
	head := lipgloss.NewStyle().Foreground(green).Render(m.pageName()) + "\n" + renderHints(hints, width) + "\n" + strings.Repeat("─", width)
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
		var languages []string
		for _, choice := range []struct {
			language i18n.Language
			label    string
		}{{i18n.English, "English"}, {i18n.Chinese, "中文"}} {
			style := lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
			if m.tr.Language() == choice.language {
				style = style.Foreground(green).Background(selected)
			}
			languages = append(languages, style.Render(choice.label))
		}
		body += "\n\n" + m.tr.Message("settings.language") + "\n" + box(wrap(strings.Join(languages, "  "), width-2), width, 3, m.focus == OperationsFocus)
	}
	return body
}
