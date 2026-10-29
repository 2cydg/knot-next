package tui

import (
	"fmt"
	"image/color"
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
	tagSearch  = lipgloss.Color("#8fb8d8")
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
		// textinput pads wide placeholder characters with NULs. They have
		// zero display width but can disturb Lip Gloss border placement.
		if r == 0 {
			continue
		}
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

// Catalog hints use "key description" pairs separated by two spaces.
// Style each pair before wrapping so key and description keep distinct colors.
func renderHints(value string, width int) string {
	var parts []string
	for _, hint := range strings.Split(value, "  ") {
		key, description, _ := strings.Cut(hint, " ")
		parts = append(parts, lipgloss.NewStyle().Foreground(green).Render(key)+" "+lipgloss.NewStyle().Foreground(muted).Render(description))
	}
	return wrap(strings.Join(parts, "  "), width)
}
func box(content string, width, height int, focus bool) string {
	color := line
	if focus {
		color = green
	}
	return coloredBox(content, width, height, color)
}
func coloredBox(content string, width, height int, borderColor color.Color) string {
	return lipgloss.NewStyle().Foreground(text).Background(background).Border(lipgloss.NormalBorder()).BorderForeground(borderColor).Width(max(1, width)).Height(max(1, height)).MaxWidth(width).MaxHeight(height).Render(content)
}
func (m *Model) sidebarWidth() int {
	if m.width < 100 {
		return 6
	}
	return 19
}
func (m *Model) mainWidth() int      { return max(1, m.width-m.sidebarWidth()-1) }
func (m *Model) pageName() string    { return m.tr.Message("menu." + string(m.page)) }
func (m *Model) formInputWidth() int { return max(1, min(78, m.width-4)-6) }

func (m *Model) View() tea.View {
	if m.width < 24 || m.height < 10 {
		v := tea.NewView(wrap(m.tr.Message("ui.small"), m.width))
		v.AltScreen = true
		return v
	}
	w, h := m.width, m.height
	modal := m.modal
	if m.exitConfirm != nil {
		modal = m.exitConfirm
	}
	versions := "cli " + Display(buildinfo.Version) + "    core " + m.tr.Message("ui.unknown")
	if m.hasSnapshot {
		versions = "cli " + Display(buildinfo.Version) + "    core " + Display(m.snapshot.Version.Version)
	}
	versions = ansi.Truncate(versions, max(1, w-7), "…")
	top := lipgloss.NewStyle().Foreground(green).Render("Knot") + strings.Repeat(" ", max(1, w-4-ansi.StringWidth(versions))) + lipgloss.NewStyle().Foreground(muted).Render(versions)
	var menu []string
	icons := []string{"🖥️", "🔑", "🔀", "🔄", "⚙️"}
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
			if m.focus == MenuFocus && modal == nil {
				style = style.Background(selected)
			}
		}
		menu = append(menu, style.Render(value))
	}
	sidebar := box(strings.Join(menu, "\n\n"), m.sidebarWidth(), h-2, m.focus == MenuFocus && modal == nil)
	main := box(lipgloss.NewStyle().Padding(0, 1).Render(m.mainContent(m.mainWidth()-4, h-4)), m.mainWidth(), h-2, m.focus != MenuFocus && modal == nil)
	base := top + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " ", main)
	if m.launch.Mode() == entry.AliasDirect {
		message := m.directMessage
		if message == "" {
			message = m.tr.Message("ui.loading")
		}
		base = top + "\n\n" + wrap(Display(m.launch.ServerRef())+"\n"+message, w) + "\n" + renderHints(m.tr.Message("ssh.cancel_hint"), w)
	}
	canvas := lipgloss.NewCanvas(w, h).Compose(lipgloss.NewCompositor(lipgloss.NewLayer(base)))
	// Notifications use available top-right space; a modal always renders above
	// them so its body and confirmation keys cannot be obscured.
	if len(m.notices) > 0 {
		nw := min(38, w-4)
		available := min(h-3, 9)
		if modal != nil {
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
			if n.ID == "ssh" && m.ssh != nil {
				content += "\n" + renderHints(m.tr.Message("ssh.cancel_hint"), nw-4)
			}
			if modal != nil {
				content = ansi.Truncate(strings.ReplaceAll(content, "\n", " "), nw-4, "…")
			}
			part := lipgloss.NewStyle().Foreground(color).Background(background).Border(lipgloss.NormalBorder()).BorderForeground(color).Width(nw).MaxHeight(available).Render(content)
			if modal != nil {
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
	if modal != nil {
		content := m.modalView()
		mw, mh := lipgloss.Width(content), lipgloss.Height(content)
		y := max(3, (h-mh)/2)
		if modal.Form != nil {
			// Keep form titles and tabs anchored while their content grows or shrinks.
			y = 3
		}
		canvas.Compose(lipgloss.NewCompositor(lipgloss.NewLayer(content).X(max(0, (w-mw)/2)).Y(y)))
	}
	v := tea.NewView(canvas.Render())
	v.AltScreen = true
	return v
}

var primaryActions = map[entry.Page][]string{
	entry.Servers: {"n", "e", "Delete", "c/Enter", "f"},
	entry.Keys:    {"n", "Delete", "x"},
	entry.Proxies: {"n", "e", "Delete", "t"},
	entry.Sync:    {"n", "e", "d", "p", "a", "s"},
}

func (m *Model) mainContent(width, height int) string {
	if m.page == entry.Settings {
		return m.settingsContent(width)
	}
	hints := m.tr.Message("ui.region_hints")
	for _, key := range primaryActions[m.page] {
		label := m.tr.Message("ui.action." + key)
		if key == "s" && m.page == entry.Sync {
			label = m.tr.Message("manage.sync_password")
		}
		hints += "  " + key + " " + label
	}
	if m.page == entry.Sync && m.hasSnapshot && !m.snapshot.Config.Settings.SyncPasswordSet {
		hints = m.tr.Message("ui.sync_hint")
	}
	head := lipgloss.NewStyle().Foreground(green).Render(m.pageName()) + "\n" + renderHints(hints, width) + "\n" + strings.Repeat("─", width)
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
	searchColor := line
	if m.focus == SearchFocus {
		searchColor = green
	}
	if strings.HasPrefix(m.search.Value(), "/") && m.page == entry.Servers {
		placeholder = m.tr.Message("ui.search_tags")
		searchColor = tagSearch
	}
	input := m.search
	input.Placeholder = placeholder
	search := displayWidget(input.View())
	head += "\n" + coloredBox(fit(search, max(1, width-2)), width, 3, searchColor) + "\n"
	remaining := max(3, height-lipgloss.Height(head)-1)
	rows := m.rows()
	tableWidth := width
	detailWidth := 0
	if m.width >= 110 && len(rows) > 0 {
		detailWidth = 27
		tableWidth -= detailWidth + 1
	}
	table := m.table(rows, tableWidth, max(3, remaining))
	if detailWidth > 0 {
		var detail []string
		for _, r := range rows {
			if r.ID == m.selectedID {
				contentWidth := detailWidth - 4
				padding := lipgloss.NewStyle().Padding(0, 1)
				detail = append(detail, padding.Foreground(muted).Render(m.tr.Message("ui.detail")), strings.Repeat("─", detailWidth-2))
				if m.page == entry.Sync {
					detail = append(detail, padding.Foreground(green).Bold(true).Render(wrap(Display(r.Alias), contentWidth)), "")
				}
				detail = append(detail, padding.Render(m.detailFields(r.Details, contentWidth)))
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
		if m.page == entry.Servers {
			marker = "  "
		}
		style := lipgloss.NewStyle().Foreground(text)
		if r.ID == m.selectedID {
			marker = "> "
			style = style.Background(selected).Foreground(green)
		}
		lines = append(lines, style.Render(fit(marker+strings.Join(cells, " "), contentWidth)))
	}
	return box(strings.Join(lines, "\n"), width, height, false)
}
func (m *Model) detailFields(details [][2]string, width int) string {
	var blocks []string
	for _, d := range details {
		label := lipgloss.NewStyle().Foreground(muted).Render(wrap(m.tr.Message("ui."+d[0]), width))
		value := lipgloss.NewStyle().Foreground(text).Render(wrap(Display(d[1]), width-2))
		blocks = append(blocks, label+"\n"+lipgloss.NewStyle().PaddingLeft(2).Render(value))
	}
	return strings.Join(blocks, "\n\n")
}

func (m *Model) modalBody(modal *Modal, width int) string {
	var parts []string
	if modal.Body != "" {
		parts = append(parts, wrap(displayMultiline(modal.Body), width))
	}
	if len(modal.Details) > 0 {
		parts = append(parts, m.detailFields(modal.Details, width))
	}
	return strings.Join(parts, "\n\n")
}

func (m *Model) modalHeader(modal *Modal, inner int) string {
	hints := m.tr.Message("ui.close_hints")
	if modal.Kind == ExitConfirm {
		hints = m.tr.Message("ui.exit_hints")
	}
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
	if modal.Hints != "" {
		hints += "  " + modal.Hints
	}
	// Header and hints are reserved; only body scrolls/crops in small windows.
	return wrap(Display(modal.Title), inner) + "\n" + renderHints(hints, inner) + "\n" + strings.Repeat("─", inner)
}

func (m *Model) informationScrollLimit() int {
	inner := max(1, min(78, m.width-4)-4)
	header := m.modalHeader(m.modal, inner)
	available := max(1, m.height-6-lipgloss.Height(header))
	return max(0, lipgloss.Height(m.modalBody(m.modal, inner))-available)
}

func (m *Model) clampModalScroll() {
	if m.modal != nil && m.modal.Kind == Information {
		m.modal.Scroll = max(0, min(m.modal.Scroll, m.informationScrollLimit()))
	}
}

func (m *Model) modalView() string {
	modal := m.modal
	if m.exitConfirm != nil {
		modal = m.exitConfirm
	}
	width := min(78, m.width-4)
	inner := max(1, width-4)
	header := m.modalHeader(modal, inner)
	var body []string
	if content := m.modalBody(modal, inner); content != "" {
		body = append(body, content)
	}
	if modal.Kind == ExitConfirm {
		var choices []string
		for i, key := range []string{"ui.yes", "ui.no"} {
			style := lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
			if i == modal.Index {
				style = style.Foreground(green).Background(selected).Bold(true)
			}
			choices = append(choices, style.Render(m.tr.Message(key)))
		}
		body = append(body, "", strings.Join(choices, "  "))
	}
	if modal.Form != nil {
		form := modal.Form
		if form.Busy {
			body = append(body, m.tr.Message("ui.busy"))
		}
		if len(form.Sections) > 0 {
			var tabs []string
			for _, s := range form.Sections {
				style := lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
				if s.ID == form.Section {
					style = style.Foreground(green).Background(selected)
				}
				tabs = append(tabs, style.Render(Display(s.Label)))
			}
			body = append(body, wrap(strings.Join(tabs, " "), inner), "")
		}
		available := max(1, m.height-6-lipgloss.Height(header))
		if len(body) > 0 {
			available -= lipgloss.Height(strings.Join(body, "\n")) + 1
		}
		body = append(body, formView(form, inner, max(1, available)))
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

// Render all visible fields, scrolling only when the focused field no longer
// fits. Advancing focus must not hide values that still fit in the viewport.
func formView(form *Form, width, height int) string {
	var blocks []string
	var starts []int
	focusStart, focusEnd, offset := 0, 0, 0
	for i, f := range form.Fields {
		if !form.visible(i) || (f.Section != "" && f.Section != form.Section) {
			continue
		}
		label := f.Label
		if f.Required {
			label += " *"
		}
		parts := []string{wrap(Display(label), width)}
		if f.Static {
			parts = []string{renderHints(label, width)}
		}
		if !f.Static {
			inputWidth := max(1, width-2)
			value := ""
			switch {
			case len(f.Choices) > 0:
				value = fit("‹ "+Display(f.choiceLabel())+" ›", inputWidth)
			case f.Multiline:
				value = displayWidget(f.Area.View())
			default:
				value = fit(displayWidget(f.Input.View()), inputWidth)
			}
			color := line
			if i == form.Index {
				color = green
			}
			parts = append(parts, lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(color).Width(width).Render(value))
		}
		if f.Error != "" {
			parts = append(parts, lipgloss.NewStyle().Foreground(red).Render(wrap(Display(f.Error), width)))
		}
		block := strings.Join(parts, "\n")
		starts = append(starts, offset)
		if i == form.Index {
			focusStart, focusEnd = offset, offset+lipgloss.Height(block)
		}
		offset += lipgloss.Height(block) + 1
		blocks = append(blocks, block)
	}
	start := 0
	if focusEnd > height {
		start = focusStart
		for _, at := range starts {
			if at >= focusEnd-height {
				start = min(at, focusStart)
				break
			}
		}
	}
	lines := strings.Split(strings.Join(blocks, "\n\n"), "\n")
	start = min(start, len(lines)-1)
	return strings.Join(lines[start:min(len(lines), start+height)], "\n")
}
