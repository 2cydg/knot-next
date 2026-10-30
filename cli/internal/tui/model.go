// Package tui implements the prototype's menu, operations and modal ownership.
package tui

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/corestart"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

type Focus int

const (
	MenuFocus Focus = iota
	OperationsFocus
	SearchFocus
)

type ListState string

const (
	Loading    ListState = "loading"
	Ready      ListState = "ready"
	Offline    ListState = "offline"
	AuthFailed ListState = "auth-failed"
	ReadError  ListState = "error"
)

type loadResult struct {
	generation uint64
	page       entry.Page
	snapshot   coreclient.Snapshot
	err        error
}
type streamResult struct {
	epoch  uint64
	stream *coreclient.Stream
	err    error
}
type eventMsg struct {
	epoch  uint64
	item   coreclient.StreamItem
	closed bool
}
type pollMsg struct{}
type dismissMsg struct {
	id       string
	revision uint64
}
type Notice struct {
	ID         string
	Text       string
	Error      bool
	Persistent bool
	revision   uint64
}
type NoticeMsg struct {
	ID         string
	Text       string
	Error      bool
	Persistent bool
}

// FormResult binds asynchronous completion to its original modal and instance.
// Field errors use localized strings supplied by the typed operation handler.
type FormResult struct {
	ModalID    uint64
	InstanceID string
	Fields     map[string]string
	Err        error
	Saved      bool
}

// Model is the only owner of UI state. Cmds return values; workers never mutate
// the model, drafts, selection or focus.
type Model struct {
	ctx                context.Context
	launch             entry.LaunchContext
	tr                 i18n.Translator
	endpoint           string
	coreSettings       preferences.Core
	coordinator        *corestart.Coordinator
	startupDirectory   string
	firstSetup         bool
	setupOnly          bool
	setupComplete      bool
	canceled           bool
	cancelSetup        context.CancelFunc
	load               func(context.Context, string) (coreclient.Snapshot, error)
	page               entry.Page
	settingsTab        int
	settingsDraft      i18n.Language
	settingsBusy       bool
	settingsFieldFocus bool
	saveSettings       func(preferences.Settings) error
	focus              Focus
	state              ListState
	width, height      int
	search             textinput.Model
	selectedID         string
	snapshot           coreclient.Snapshot
	lastError          error
	modal              *Modal
	exitConfirm        *Modal
	modalSequence      uint64
	notices            []Notice
	noticeRevision     uint64
	generation         uint64
	loading            bool
	cancelLoad         context.CancelFunc
	stream             *coreclient.Stream
	streamEpoch        uint64
	cancelStream       context.CancelFunc
	streamOpening      bool
	hasSnapshot        bool
	healthStatus       string
	exitCode           int
	cancelProxyTest    context.CancelFunc
	proxyTestRevision  uint64
	ssh                *sshAttempt
	handoff            *sshAttempt
	directStarted      bool
	directMessage      string
	resumeCmd          tea.Cmd
}

func New(ctx context.Context, launch entry.LaunchContext, tr i18n.Translator, endpoint string) *Model {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 1024
	page := launch.InitialPage()
	if page == "" {
		page = entry.Servers
	}
	focus := MenuFocus
	if launch.Mode() == entry.PageDirect {
		focus = OperationsFocus
	}
	return &Model{ctx: ctx, launch: launch, tr: tr, endpoint: endpoint, load: func(ctx context.Context, url string) (coreclient.Snapshot, error) {
		return coreclient.Load(ctx, url, "")
	}, coordinator: corestart.New(corestart.Dependencies{}), saveSettings: preferences.Save, page: page, focus: focus, state: Loading, width: 80, height: 24, search: input}
}

func (m *Model) Init() tea.Cmd {
	if m.firstSetup {
		return m.openCoreConnection(true)
	}
	if m.launch.Mode() == entry.AliasDirect {
		return m.beginLoad(false)
	}
	resume := m.resumeCmd
	m.resumeCmd = nil
	cmds := []tea.Cmd{m.beginLoad(false), poll(), resume}
	// A new Bubble Tea program follows every native SSH attachment. Timers
	// from the previous program cannot dismiss notices retained in this model.
	for _, notice := range m.notices {
		cmds = append(cmds, m.noticeTimer(notice))
	}
	return tea.Batch(cmds...)
}
func poll() tea.Cmd { return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return pollMsg{} }) }
func (m *Model) Close() {
	if m.cancelSetup != nil {
		m.cancelSetup()
	}
	if m.ssh != nil {
		m.ssh.Attempt.Close()
	}
	m.pause()
}
func (m *Model) pause() {
	if m.cancelProxyTest != nil {
		m.cancelProxyTest()
	}
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	m.stopStream()
	if m.snapshot.Client != nil {
		m.snapshot.Client.Close()
	}
}
func (m *Model) stopStream() {
	m.streamEpoch++
	if m.cancelStream != nil {
		m.cancelStream()
		m.cancelStream = nil
	}
	if m.stream != nil {
		m.stream.Close()
		m.stream = nil
	}
	m.streamOpening = false
}
func (m *Model) beginLoad(showLoading bool) tea.Cmd {
	if m.firstSetup || m.canceled {
		return nil
	}
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	m.generation++
	generation, page := m.generation, m.page
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	m.cancelLoad = cancel
	m.loading = true
	if showLoading {
		m.state = Loading
	}
	loader, path := m.load, m.endpoint
	return func() tea.Msg {
		defer cancel()
		s, err := loader(ctx, path)
		return loadResult{generation: generation, page: page, snapshot: s, err: err}
	}
}
func (m *Model) openStream() tea.Cmd {
	if m.stream != nil || m.streamOpening || m.snapshot.Client == nil {
		return nil
	}
	m.streamOpening = true
	epoch := m.streamEpoch
	if m.cancelStream != nil {
		m.cancelStream()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelStream = cancel
	client := m.snapshot.Client
	return func() tea.Msg { s, err := client.Events(ctx); return streamResult{epoch: epoch, stream: s, err: err} }
}
func readStream(epoch uint64, stream *coreclient.Stream) tea.Cmd {
	return func() tea.Msg { item, ok := <-stream.Items; return eventMsg{epoch: epoch, item: item, closed: !ok} }
}

func (m *Model) notify(notice NoticeMsg) tea.Cmd {
	m.noticeRevision++
	n := Notice{ID: notice.ID, Text: notice.Text, Error: notice.Error, Persistent: notice.Persistent, revision: m.noticeRevision}
	for i, old := range m.notices {
		if old.ID == notice.ID {
			m.notices[i] = n
			m.pruneNotices()
			return m.noticeTimer(n)
		}
	}
	m.notices = append(m.notices, n)
	m.pruneNotices()
	return m.noticeTimer(n)
}

func (m *Model) pruneNotices() {
	// Bound final notices separately, including active-to-final updates.
	// Active operations cannot be evicted or suppress new warning messages.
	oldest, finals := -1, 0
	for i, n := range m.notices {
		if !n.Persistent {
			finals++
			if oldest == -1 || n.revision < m.notices[oldest].revision {
				oldest = i
			}
		}
	}
	if finals > 16 {
		m.notices = append(m.notices[:oldest], m.notices[oldest+1:]...)
	}
}
func (m *Model) noticeTimer(n Notice) tea.Cmd {
	if n.Persistent {
		return nil
	}
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return dismissMsg{id: n.ID, revision: n.revision} })
}
func (m *Model) removeNotice(id string) {
	for i, n := range m.notices {
		if n.ID == id {
			m.notices = append(m.notices[:i], m.notices[i+1:]...)
			return
		}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case coreConnectionResult:
		return m, m.handleCoreConnection(msg)
	case settingsSaved:
		m.settingsBusy = false
		if msg.err != nil {
			return m, m.notify(NoticeMsg{ID: "settings", Text: m.tr.Text("settings.save_failed", map[string]any{"Reason": msg.err.Error()}), Error: true})
		}
		m.tr = i18n.New(msg.language)
		m.settingsDraft = ""
		m.settingsFieldFocus = false
		m.focus = MenuFocus
		return m, m.notify(NoticeMsg{ID: "settings", Text: m.tr.Message("settings.saved")})
	case sshResult:
		return m, m.handleSSH(msg)
	case sshPollMsg:
		if m.ssh != nil && m.ssh.Attempt == msg.Attempt && m.ssh.Revision == msg.Revision && !m.ssh.Submitting {
			return m, func() tea.Msg {
				return sshResult{Attempt: msg.Attempt, Revision: msg.Revision, Result: msg.Attempt.Poll()}
			}
		}
		return m, nil
	case sshCancelMsg:
		if m.ssh == nil || m.ssh.Attempt != msg.Attempt {
			return m, nil
		}
		return m, m.cancelSSH()
	case sshRetryMsg:
		if m.ssh == nil || m.ssh.Attempt != msg.Attempt {
			return m, nil
		}
		if m.snapshot.Runtime.InstanceID != msg.Attempt.Instance {
			return m, m.finishSSH(coreclient.Failure("INSTANCE_CHANGED"))
		}
		m.ssh, m.modal = nil, nil
		return m, m.startSSH(msg.Ref, msg.Resolve, false)
	case sshCanceled:
		var cmds []tea.Cmd
		for _, code := range msg.Warnings {
			cmds = append(cmds, m.notify(NoticeMsg{ID: "ssh-warning:" + code, Text: m.tr.Text("ui.warning", map[string]any{"Code": code}), Error: true}))
		}
		return m, tea.Batch(cmds...)
	case aliasFinishedMsg:
		if m.exitConfirm != nil {
			return m, tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return aliasFinishedMsg{} })
		}
		return m, tea.Quit
	case editorResult:
		if m.modal == nil || m.modal.ID != msg.ModalID || msg.Instance != m.snapshot.Runtime.InstanceID {
			return m, nil
		}
		if msg.Err != nil {
			m.modal.Body = m.tr.ProtocolError(coreclient.Code(msg.Err))
			return m, nil
		}
		return m, m.showEditor(msg.Draft)
	case manageResult:
		return m, m.handleManageResult(msg)
	case proxyResult:
		return m, m.handleProxyResult(msg)
	case forwardsResult:
		return m, m.handleForwards(msg)
	case forwardActionResult:
		return m, m.handleForwardAction(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.search.SetWidth(max(1, m.mainWidth()-7))
		if m.modal != nil && m.modal.Form != nil {
			m.modal.Form.Resize(m.formInputWidth())
		}
		m.clampModalScroll()
	case FormResult:
		if m.modal == nil || m.modal.ID != msg.ModalID || m.modal.Form == nil || msg.InstanceID != m.snapshot.Runtime.InstanceID {
			return m, nil
		}
		form := m.modal.Form
		form.Busy = false
		first := -1
		for i := range form.Fields {
			form.Fields[i].Error = msg.Fields[form.Fields[i].Name]
			if first == -1 && form.Fields[i].Error != "" {
				first = i
			}
		}
		if first != -1 {
			return m, form.Focus(first)
		}
		if msg.Err != nil {
			return m, m.notify(NoticeMsg{ID: "form-error", Text: m.tr.ProtocolError(coreclient.Code(msg.Err)), Error: true})
		}
		if msg.Saved {
			return m, m.dismissModal(false)
		}
	case loadResult:
		if msg.generation != m.generation || msg.page != m.page {
			if msg.snapshot.Client != nil {
				msg.snapshot.Client.Close()
			}
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.lastError = msg.err
			code := coreclient.Code(msg.err)
			m.state = ReadError
			if code == "UNREACHABLE" || code == "OFFLINE" {
				m.state = Offline
			}
			if code == "UNAUTHORIZED" {
				m.state = AuthFailed
			}
			m.hasSnapshot = false
			m.selectedID = ""
			m.stopStream()
			if m.snapshot.Client != nil {
				m.snapshot.Client.Close()
			}
			m.snapshot = coreclient.Snapshot{}
			m.healthStatus = ""
			m.removeNotice("health")
			if m.launch.Mode() == entry.AliasDirect {
				m.directStarted = true
				m.directMessage = m.tr.ProtocolError(code)
				m.exitCode = entry.ExitFailure
				return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return aliasFinishedMsg{} })
			}
			return m, nil
		}
		changed := m.snapshot.Runtime.InstanceID != "" && (m.snapshot.Runtime.InstanceID != msg.snapshot.Runtime.InstanceID || !m.snapshot.Client.SameEndpoint(msg.snapshot.Client))
		if changed {
			m.stopStream()
			m.selectedID = ""
		}
		if m.snapshot.Client != nil {
			m.snapshot.Client.Close()
		}
		m.snapshot = msg.snapshot
		m.hasSnapshot = true
		m.state = Ready
		m.lastError = nil
		m.ensureSelection()
		var cmds []tea.Cmd
		if changed {
			cmds = append(cmds, m.notify(NoticeMsg{ID: "instance", Text: m.tr.Message("ui.instance_changed"), Error: true}))
		}
		if msg.snapshot.Health.Status != "ok" && (changed || m.healthStatus != msg.snapshot.Health.Status) {
			cmds = append(cmds, m.notify(NoticeMsg{ID: "health", Text: m.tr.Message("ui.degraded"), Error: true}))
		}
		m.healthStatus = msg.snapshot.Health.Status
		if m.healthStatus == "ok" {
			m.removeNotice("health")
		}
		cmds = append(cmds, m.openStream())
		if m.launch.Mode() == entry.AliasDirect && !m.directStarted {
			m.directStarted = true
			cmds = append(cmds, m.startSSH(m.launch.ServerRef(), true))
		}
		return m, tea.Batch(cmds...)
	case streamResult:
		if msg.epoch != m.streamEpoch {
			if msg.stream != nil {
				msg.stream.Close()
			}
			return m, nil
		}
		m.streamOpening = false
		if msg.err != nil {
			if msg.stream != nil {
				msg.stream.Close()
			}
			m.stopStream()
			return m, m.notify(NoticeMsg{ID: "events", Text: m.tr.ProtocolError(coreclient.Code(msg.err)), Error: true})
		}
		m.stream = msg.stream
		m.removeNotice("events")
		return m, readStream(msg.epoch, msg.stream)
	case eventMsg:
		if msg.epoch != m.streamEpoch || m.stream == nil {
			return m, nil
		}
		if msg.closed || msg.item.Err != nil {
			m.stopStream()
			return m, tea.Batch(m.notify(NoticeMsg{ID: "events", Text: m.tr.ProtocolError("EVENTS_UNAVAILABLE"), Error: true}), m.beginLoad(false))
		}
		var cmds []tea.Cmd
		event := msg.item.Event
		// core.snapshot is only a handshake. Config changes currently converge
		// through polling; future local mutations must explicitly refresh.
		if event.Type == "core.shutdown_started" || event.Type == "session.connected" {
			if !m.loading {
				cmds = append(cmds, m.beginLoad(false))
			}
		}
		if strings.HasPrefix(event.Type, "forward.") && m.modal != nil && m.modal.Forwards != nil {
			cmds = append(cmds, m.refreshForwards(m.modal))
		}
		// Session failures are handled by the connection/terminal result path,
		// including the optional Agent fallback. They are not non-fatal warnings.
		if event.Type != "session.error" && (event.Level == "warn" || event.Level == "warning" || event.Level == "error") {
			var warning struct {
				Warning struct {
					Kind string `json:"kind"`
				} `json:"warning"`
			}
			_ = json.Unmarshal(event.Data, &warning)
			code := warning.Warning.Kind
			if code == "" {
				code = event.Type
			}
			cmds = append(cmds, m.notify(NoticeMsg{ID: "warning:" + event.ResourceID, Text: m.tr.Text("ui.warning", map[string]any{"Code": code}), Error: true}))
		}
		cmds = append(cmds, readStream(msg.epoch, m.stream))
		return m, tea.Batch(cmds...)
	case pollMsg:
		var forwardCmd tea.Cmd
		if m.modal != nil && m.modal.Forwards != nil {
			forwardCmd = m.refreshForwards(m.modal)
		}
		if m.loading {
			return m, tea.Batch(forwardCmd, poll())
		}
		return m, tea.Batch(forwardCmd, m.beginLoad(false), poll())
	case NoticeMsg:
		return m, m.notify(msg)
	case dismissMsg:
		for i, n := range m.notices {
			if n.ID == msg.id && n.revision == msg.revision && !n.Persistent {
				m.notices = append(m.notices[:i], m.notices[i+1:]...)
				break
			}
		}
	case tea.KeyPressMsg:
		if m.firstSetup && msg.String() == "ctrl+c" {
			m.canceled = true
			m.exitCode = entry.ExitCanceled
			if m.cancelSetup != nil {
				m.cancelSetup()
			}
			return m, tea.Quit
		}
		if m.exitConfirm != nil {
			return m, m.exitKey(msg)
		}
		if msg.String() == "ctrl+c" && (m.modal == nil || !m.modal.exclusive()) {
			return m, m.openExitConfirm()
		}
		if m.modal != nil {
			return m, m.modalKey(msg)
		}
		if m.ssh != nil {
			if msg.String() == "esc" {
				return m, m.cancelSSH()
			}
			return m, nil
		}
		if m.launch.Mode() == entry.AliasDirect {
			return m, nil
		}
		if m.focus == SearchFocus {
			switch msg.String() {
			case "enter", "esc":
				m.search.Blur()
				m.focus = OperationsFocus
				return m, nil
			case "/":
				if m.page == entry.Servers && !strings.HasPrefix(m.search.Value(), "/") {
					m.search.SetValue("/" + m.search.Value())
					m.search.CursorEnd()
					m.ensureSelection()
					return m, nil
				}
			}
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.ensureSelection()
			return m, cmd
		}
		return m, m.key(msg)
	case tea.PasteMsg:
		if m.exitConfirm != nil {
			return m, nil
		}
		if m.modal != nil {
			if m.modal.Form != nil && !m.modal.Form.Busy {
				cmd := m.modal.Form.update(msg)
				if m.modal.OnChange != nil {
					m.modal.OnChange()
				}
				return m, cmd
			}
			return m, nil
		}
		if m.focus == SearchFocus {
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.ensureSelection()
			return m, cmd
		}
	default:
		if m.modal != nil && m.modal.Form != nil && !m.modal.Form.Busy {
			return m, m.modal.Form.update(msg)
		}
		if m.focus == SearchFocus {
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

var pages = []entry.Page{entry.Servers, entry.Keys, entry.Proxies, entry.Sync, entry.Settings}

func (m *Model) key(msg tea.KeyPressMsg) tea.Cmd {
	binding := msg.String()
	if m.focus == MenuFocus {
		// Only the page's entry shortcuts may cross from the menu. Once
		// editing starts, Update routes all typing to the focused widget.
		gated := m.page == entry.Sync && !m.snapshot.Config.Settings.SyncPasswordSet
		if m.page != entry.Settings && ((!gated && (binding == "/" || binding == "n")) || (gated && binding == "s")) {
			m.focus = OperationsFocus
		}
	}
	if m.focus == MenuFocus {
		index := 0
		for i, p := range pages {
			if p == m.page {
				index = i
			}
		}
		switch binding {
		case "esc":
			return m.openExitConfirm()
		case "down":
			index = (index + 1) % len(pages)
		case "up":
			index = (index + len(pages) - 1) % len(pages)
		case "home":
			index = 0
		case "end":
			index = len(pages) - 1
		case "right", "enter", "tab":
			m.focus = OperationsFocus
			if m.page == entry.Settings {
				m.settingsFieldFocus = false
			}
			return nil
		default:
			return nil
		}
		m.page = pages[index]
		m.search.SetValue("")
		m.selectedID = ""
		return m.beginLoad(m.page != entry.Settings)
	}
	if m.page == entry.Settings {
		return m.settingsKey(binding)
	}
	switch binding {
	case "left", "esc", "tab", "shift+tab":
		m.focus = MenuFocus
	case "/":
		if m.page != entry.Sync || m.snapshot.Config.Settings.SyncPasswordSet {
			m.focus = SearchFocus
			return m.search.Focus()
		}
	case "up", "down", "home", "end":
		rows := m.rows()
		if len(rows) == 0 {
			return nil
		}
		index := 0
		for i, row := range rows {
			if row.ID == m.selectedID {
				index = i
			}
		}
		switch binding {
		case "up":
			index--
		case "down":
			index++
		case "home":
			index = 0
		case "end":
			index = len(rows) - 1
		}
		m.selectedID = rows[max(0, min(len(rows)-1, index))].ID
	case "enter":
		if m.state == Ready && m.selectedID != "" {
			if m.page == entry.Servers {
				return m.startSSH(m.selectedID, false)
			}
			return m.openDetail()
		}
	case "n", "e", "d", "delete", "c", "f", "p", "t", "s", "x", "a":
		if m.state == Ready && m.businessKeyAllowed(binding) {
			return m.businessAction(binding)
		}
	}
	return nil
}
func (m *Model) businessKeyAllowed(binding string) bool {
	if m.page == entry.Sync && !m.snapshot.Config.Settings.SyncPasswordSet {
		return binding == "s"
	}
	if m.page == entry.Sync && binding == "s" {
		return true
	}
	keys := map[entry.Page]string{entry.Servers: "n e d delete c f p", entry.Keys: "n d delete x", entry.Proxies: "n e d delete t", entry.Sync: "n e d delete p s a"}
	for _, key := range strings.Fields(keys[m.page]) {
		if key == binding {
			return binding == "n" || m.selectedID != ""
		}
	}
	return false
}
func (m *Model) pending() tea.Cmd {
	return m.notify(NoticeMsg{ID: "pending", Text: m.tr.Message("ui.pending")})
}

type row struct {
	ID       string
	Alias    string
	Cells    []string
	Details  [][2]string
	LastUsed time.Time
}

func (m *Model) ensureSelection() {
	rows := m.rows()
	for _, r := range rows {
		if r.ID == m.selectedID {
			return
		}
	}
	m.selectedID = ""
	if len(rows) > 0 {
		m.selectedID = rows[0].ID
	}
}
func (m *Model) rows() []row {
	rows := m.allRows()
	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	filtered := rows[:0]
	for _, r := range rows {
		match := strings.Contains(strings.ToLower(r.Alias), query)
		if m.page == entry.Servers && strings.HasPrefix(query, "/") {
			match = false
			for _, s := range m.snapshot.Config.Servers {
				if s.ID == r.ID {
					for _, tag := range s.Tags {
						if strings.Contains(strings.ToLower(tag), strings.TrimSpace(query[1:])) {
							match = true
						}
					}
					break
				}
			}
		}
		if match {
			filtered = append(filtered, r)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if m.page == entry.Servers && !filtered[i].LastUsed.Equal(filtered[j].LastUsed) {
			return filtered[i].LastUsed.After(filtered[j].LastUsed)
		}
		return strings.ToLower(filtered[i].Alias) < strings.ToLower(filtered[j].Alias)
	})
	return filtered
}
func (m *Model) openDetail() tea.Cmd {
	for _, r := range m.rows() {
		if r.ID == m.selectedID {
			return m.OpenModal(&Modal{Kind: Information, Title: m.tr.Message("ui.detail") + " · " + r.Alias, Details: r.Details})
		}
	}
	return nil
}
