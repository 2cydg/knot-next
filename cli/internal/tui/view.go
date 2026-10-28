package tui

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/buildinfo"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
)

var (
	background = lipgloss.Color("#101313")
	text       = lipgloss.Color("#d3d6d4")
	muted      = lipgloss.Color("#8b9390")
	line       = lipgloss.Color("#46504b")
	green      = lipgloss.Color("#a7ccb2")
	red        = lipgloss.Color("#d6a19b")
	selected   = lipgloss.Color("#283a31")
)

// Display escapes control bytes in user data without changing the stored value
// used by the protocol. Untrusted aliases/paths must not inject terminal ANSI.
func Display(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Widgets already sanitize terminal controls, but preserve format characters.
// Escape those only in their rendered output; retain literal input for the API.
func displayWidget(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func displayMultiline(value string) string {
	parts := strings.Split(value, "\n")
	for i := range parts {
		parts[i] = Display(parts[i])
	}
	return strings.Join(parts, "\n")
}
func fit(value string, width int) string {
	width = max(0, width)
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}
func wrap(value string, width int) string { return ansi.Wrap(value, max(1, width), "") }
func box(content string, width, height int, focus bool) string {
	color := line
	if focus {
		color = green
	}
	return lipgloss.NewStyle().Foreground(text).Background(background).Border(lipgloss.NormalBorder()).BorderForeground(color).Width(max(1, width)).Height(max(1, height)).MaxWidth(width).MaxHeight(height).Render(content)
}
func (m *Model) sidebarWidth() int {
	if m.width < 100 {
		return 6
	}
	return 19
}
func (m *Model) mainWidth() int   { return max(1, m.width-m.sidebarWidth()-1) }
func (m *Model) pageName() string { return m.tr.Message("menu." + string(m.page)) }

func (m *Model) View() tea.View {
	if m.width < 24 || m.height < 10 {
		v := tea.NewView(wrap(m.tr.Message("ui.small"), m.width))
		v.AltScreen = true
		return v
	}
	w, h := m.width, m.height
	versions := "cli " + Display(buildinfo.Version) + "    core " + m.tr.Message("ui.unknown")
	if m.hasSnapshot {
		versions = "cli " + Display(buildinfo.Version) + "    core " + Display(m.snapshot.Version.Version)
	}
	versions = ansi.Truncate(versions, max(1, w-7), "…")
	top := lipgloss.NewStyle().Foreground(green).Render("Knot") + strings.Repeat(" ", max(1, w-4-ansi.StringWidth(versions))) + lipgloss.NewStyle().Foreground(muted).Render(versions)
	var menu []string
	icons := []string{"S", "K", "P", "Y"}
	for i, p := range pages {
		value := "  " + icons[i]
		if p == m.page {
			value = "> " + icons[i]
		}
		if w >= 100 {
			value += " " + m.tr.Message("menu."+string(p))
		}
		value = fit(value, m.sidebarWidth()-2)
		style := lipgloss.NewStyle().Foreground(muted)
		if p == m.page {
			style = style.Foreground(green)
			if m.focus == MenuFocus && m.modal == nil {
				style = style.Background(selected)
			}
		}
		menu = append(menu, style.Render(value))
	}
	sidebar := box(strings.Join(menu, "\n\n"), m.sidebarWidth(), h-2, m.focus == MenuFocus && m.modal == nil)
	main := box(lipgloss.NewStyle().Padding(0, 1).Render(m.mainContent(m.mainWidth()-4, h-4)), m.mainWidth(), h-2, m.focus != MenuFocus && m.modal == nil)
	base := top + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " ", main)
	if m.launch.Mode() == entry.AliasDirect {
		message := m.directMessage
		if message == "" {
			message = m.tr.Message("ui.loading")
		}
		base = top + "\n\n" + wrap(Display(m.launch.ServerRef())+"\n"+message+"\n"+m.tr.Message("ssh.cancel_hint"), w)
	}
	canvas := lipgloss.NewCanvas(w, h).Compose(lipgloss.NewCompositor(lipgloss.NewLayer(base)))
	// Notifications use available top-right space; a modal always renders above
	// them so its body and confirmation keys cannot be obscured.
	if len(m.notices) > 0 {
		nw := min(38, w-4)
		available := min(h-3, 9)
		if m.modal != nil {
			available = 1
		}
		var parts []string
		for i := len(m.notices) - 1; i >= 0; i-- {
			n := m.notices[i]
			color := green
			if n.Error {
				color = red
			}
			content := wrap(Display(n.Text), nw-4)
			if m.modal != nil {
				content = ansi.Truncate(strings.ReplaceAll(content, "\n", " "), nw-4, "…")
			}
			part := lipgloss.NewStyle().Foreground(color).Background(background).Border(lipgloss.NormalBorder()).BorderForeground(color).Width(nw).MaxHeight(available).Render(content)
			if m.modal != nil {
				part = lipgloss.NewStyle().Foreground(color).Background(background).Render(fit(content, nw))
			}
			if lipgloss.Height(strings.Join(append(parts, part), "\n")) > available {
				break
			}
			parts = append(parts, part)
		}
		if len(parts) > 0 {
			canvas.Compose(lipgloss.NewCompositor(lipgloss.NewLayer(strings.Join(parts, "\n")).X(w - nw - 1).Y(1)))
		}
	}
	if m.modal != nil {
		modal := m.modalView()
		mw, mh := lipgloss.Width(modal), lipgloss.Height(modal)
		canvas.Compose(lipgloss.NewCompositor(lipgloss.NewLayer(modal).X(max(0, (w-mw)/2)).Y(max(3, (h-mh)/2))))
	}
	v := tea.NewView(canvas.Render())
	v.AltScreen = true
	return v
}

var primaryActions = map[entry.Page][]string{
	entry.Servers: {"n", "e", "Delete", "c/Enter", "f", "t"},
	entry.Keys:    {"n", "Delete", "x"},
	entry.Proxies: {"n", "e", "Delete", "t"},
	entry.Sync:    {"n", "e", "d", "p", "a", "s"},
}

func (m *Model) mainContent(width, height int) string {
	hints := m.tr.Message("ui.region_hints")
	if m.focus == MenuFocus {
		hints = m.tr.Message("ui.menu_hints")
	}
	if m.focus == SearchFocus {
		hints = m.tr.Message("ui.search_hints")
	}
	if m.focus == OperationsFocus && m.selectedID != "" {
		hints += "  " + m.tr.Message("ui.detail_hint")
	}
	if m.focus != SearchFocus && m.state == Ready {
		for _, key := range primaryActions[m.page] {
			label := m.tr.Message("ui.action." + key)
			if key == "t" && m.page == entry.Servers {
				label = m.tr.Message("manage.forwards")
			}
			if key == "s" && m.page == entry.Sync {
				label = m.tr.Message("manage.sync_password")
			}
			hints += "  [" + key + "] " + label
		}
	}
	if m.page == entry.Sync && m.hasSnapshot && !m.snapshot.Config.Settings.SyncPasswordSet {
		hints = m.tr.Message("ui.sync_hint") + "  " + m.tr.Message("ui.menu_hints")
	}
	head := lipgloss.NewStyle().Foreground(green).Render(m.pageName()) + "\n" + lipgloss.NewStyle().Foreground(green).Render(wrap(hints, width)) + "\n" + strings.Repeat("─", width)
	remaining := max(1, height-lipgloss.Height(head)-1)
	if m.state != Ready {
		key, hint := "ui.loading", "ui.loading_hint"
		switch m.state {
		case Offline:
			key, hint = "ui.offline", "ui.offline_hint"
		case AuthFailed:
			key, hint = "ui.auth", "ui.auth_hint"
		case ReadError:
			key, hint = "ui.error", ""
		}
		style := lipgloss.NewStyle().Foreground(muted)
		if m.state != Loading {
			style = style.Foreground(red)
		}
		body := style.Render(m.tr.Message(key)) + "\n\n"
		if hint != "" {
			body += m.tr.Message(hint)
		}
		if m.lastError != nil {
			body += "\n\n" + m.tr.ProtocolError(coreclient.Code(m.lastError))
		}
		return head + "\n" + wrap(body, width)
	}
	if m.page == entry.Sync && !m.snapshot.Config.Settings.SyncPasswordSet {
		return head + "\n\n" + wrap(m.tr.Message("ui.sync_gate")+"\n\n"+m.tr.Message("ui.sync_gate_hint"), width)
	}
	placeholder := m.tr.Message("ui.search_alias")
	if m.page == entry.Servers {
		placeholder = m.tr.Message("ui.search_server")
	}
	if strings.HasPrefix(m.search.Value(), "/") && m.page == entry.Servers {
		placeholder = m.tr.Message("ui.search_tags")
	}
	input := m.search
	input.Placeholder = placeholder
	search := displayWidget(input.View())
	if m.focus != SearchFocus {
		value := Display(m.search.Value())
		if value == "" {
			value = placeholder
		}
		search = lipgloss.NewStyle().Foreground(muted).Render(fit(value, width))
	}
	head += "\n" + fit(search, width) + "\n"
	remaining -= 2
	rows := m.rows()
	if len(rows) == 0 {
		key, hint := "ui.empty", "ui.empty_hint"
		if m.search.Value() != "" {
			key, hint = "ui.no_results", "ui.no_results_hint"
		}
		return head + wrap(m.tr.Message("ui.list")+"\n\n"+m.tr.Message(key)+"\n\n"+m.tr.Message(hint), width)
	}
	tableWidth := width
	detailWidth := 0
	if m.width >= 110 {
		detailWidth = 27
		tableWidth -= detailWidth + 1
	}
	table := m.table(rows, tableWidth, max(3, remaining))
	if detailWidth > 0 {
		var detail []string
		for _, r := range rows {
			if r.ID == m.selectedID {
				detail = append(detail, m.tr.Message("ui.detail"), Display(r.Alias))
				for _, d := range r.Details {
					detail = append(detail, m.tr.Message("ui."+d[0]), wrap(Display(d[1]), detailWidth-4))
				}
				break
			}
		}
		table = lipgloss.JoinHorizontal(lipgloss.Top, table, " ", box(strings.Join(detail, "\n"), detailWidth, max(3, remaining), false))
	}
	return head + table
}
func (m *Model) table(rows []row, width, height int) string {
	keys := []string{"alias", "target", "tags", "recent"}
	switch m.page {
	case entry.Keys:
		keys = []string{"alias", "type", "bits"}
	case entry.Proxies:
		keys = []string{"alias", "type", "address", "username"}
	case entry.Sync:
		keys = []string{"alias", "type", "location", "credentials", "default"}
	}
	if m.width < 110 {
		if m.page == entry.Servers {
			keys = keys[:2]
		}
		if m.page == entry.Proxies {
			keys = keys[:3]
		}
	}
	contentWidth := max(1, width-2)
	columnWidths := make([]int, len(keys))
	available := max(1, contentWidth-2-(len(keys)-1))
	for i := range keys {
		columnWidths[i] = available / len(keys)
	}
	columnWidths[len(keys)-1] += available % len(keys)
	var headers []string
	for i, k := range keys {
		headers = append(headers, fit(m.tr.Message("ui."+k), columnWidths[i]))
	}
	lines := []string{lipgloss.NewStyle().Foreground(muted).Render("  " + strings.Join(headers, " ")), strings.Repeat("─", contentWidth)}
	index := 0
	for i, r := range rows {
		if r.ID == m.selectedID {
			index = i
		}
	}
	visible := max(1, height-4)
	start := max(0, index-visible+1)
	end := min(len(rows), start+visible)
	for _, r := range rows[start:end] {
		var cells []string
		for i := range keys {
			cells = append(cells, fit(Display(r.Cells[i]), columnWidths[i]))
		}
		marker := "· "
		style := lipgloss.NewStyle().Foreground(text)
		if r.ID == m.selectedID {
			marker = "> "
			style = style.Background(selected).Foreground(green)
		}
		lines = append(lines, style.Render(fit(marker+strings.Join(cells, " "), contentWidth)))
	}
	return box(strings.Join(lines, "\n"), width, height, false)
}
func (m *Model) modalHeader(inner int) string {
	modal := m.modal
	hints := m.tr.Message("ui.close_hints")
	if modal.exclusive() {
		hints = m.tr.Message("ui.confirm_hints")
		switch modal.Kind {
		case HostTrust:
			hints = m.tr.Message("ui.trust_hints")
		case TransferConfirm:
			hints = m.tr.Message("ui.transfer_hints")
		case SFTPCloseConfirm:
			hints = m.tr.Message("ui.sftp_close_hints")
		}
	}
	if modal.Kind == Draft {
		hints = m.tr.Message("ui.draft_hints")
	}
	if modal.Form != nil {
		hints = m.tr.Message("ui.form_hints")
		for _, f := range modal.Form.Fields {
			if f.Multiline {
				hints += "  " + m.tr.Message("ui.multiline_hint")
				break
			}
		}
		if len(modal.Form.Sections) > 0 {
			hints += "  " + m.tr.Message("manage.tabs_hint")
		}
		for _, f := range modal.Form.Fields {
			if f.Static {
				continue
			} else if len(f.Choices) > 0 {
				hints += "  " + m.tr.Message("manage.choice_hint")
				break
			}
		}
	}
	// Header and hints are reserved; only body scrolls/crops in small windows.
	return wrap(Display(modal.Title), inner) + "\n" + lipgloss.NewStyle().Foreground(green).Render(wrap(hints, inner)) + "\n" + strings.Repeat("─", inner)
}

func (m *Model) informationScrollLimit() int {
	inner := max(1, min(78, m.width-4)-4)
	header := m.modalHeader(inner)
	available := max(1, m.height-6-lipgloss.Height(header))
	return max(0, lipgloss.Height(wrap(displayMultiline(m.modal.Body), inner))-available)
}

func (m *Model) clampModalScroll() {
	if m.modal != nil && m.modal.Kind == Information {
		m.modal.Scroll = max(0, min(m.modal.Scroll, m.informationScrollLimit()))
	}
}

func (m *Model) modalView() string {
	modal := m.modal
	width := min(78, m.width-4)
	inner := max(1, width-4)
	header := m.modalHeader(inner)
	var body []string
	if modal.Body != "" {
		body = append(body, wrap(displayMultiline(modal.Body), inner))
	}
	if modal.Form != nil {
		form := modal.Form
		if len(form.Sections) > 0 {
			var tabs []string
			for _, s := range form.Sections {
				label := s.Label
				if s.ID == form.Section {
					label = "[" + label + "]"
				}
				tabs = append(tabs, label)
			}
			body = append(body, wrap(strings.Join(tabs, " · "), inner))
		}
		start := max(0, form.Index)
		for i := start; i < len(form.Fields); i++ {
			f := form.Fields[i]
			if !form.visible(i) || (f.Section != "" && f.Section != form.Section) {
				continue
			}
			label := f.Label
			if f.Required {
				label += " *"
			}
			body = append(body, wrap(Display(label), inner))
			if f.Static {
				if f.Error != "" {
					body = append(body, wrap(Display(f.Error), inner))
				}
				continue
			} else if len(f.Choices) > 0 {
				body = append(body, fit("‹ "+Display(f.choiceLabel())+" ›", inner))
			} else if f.Multiline {
				body = append(body, wrap(displayWidget(f.Area.View()), inner))
			} else {
				body = append(body, fit(displayWidget(f.Input.View()), inner))
			}
			if f.Error != "" {
				body = append(body, lipgloss.NewStyle().Foreground(red).Render(wrap(Display(f.Error), inner)))
			}
		}
		if form.Busy {
			body = append([]string{m.tr.Message("ui.busy")}, body...)
		}
	}
	if modal.Kind == Picker {
		size := modal.PageSize
		if size < 1 {
			size = 5
		}
		start := modal.PickerPage * size
		for i := start; i < min(len(modal.Choices), start+size); i++ {
			marker := "  "
			if i == modal.Index {
				marker = "> "
			}
			body = append(body, fit(fmt.Sprintf("%s%d %s", marker, i-start+1, Display(modal.Choices[i].Label)), inner))
		}
		if modal.Forwards != nil {
			body = append(body, wrap(displayMultiline(m.forwardDetail(modal)), inner))
		}
	}
	available := max(1, m.height-6-lipgloss.Height(header))
	lines := strings.Split(strings.Join(body, "\n"), "\n")
	if modal.Form == nil {
		start := min(max(0, modal.Scroll), max(0, len(lines)-available))
		lines = lines[start:]
	}
	if len(lines) > available {
		lines = lines[:available]
	}
	return lipgloss.NewStyle().Foreground(text).Background(background).Border(lipgloss.NormalBorder()).BorderForeground(green).Padding(0, 1).Width(width).MaxHeight(m.height - 4).Render(header + "\n" + strings.Join(lines, "\n"))
}
