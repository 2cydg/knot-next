package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/corestart"
	"knot-cli/internal/preferences"
)

type coreConnectionResult struct {
	modal    uint64
	settings preferences.Core
	err      error
}

func (m *Model) configureCore(settings preferences.Core) {
	m.coreSettings = settings
	m.endpoint = settings.URL
	coordinator, directory := m.coordinator, m.startupDirectory
	m.load = func(ctx context.Context, _ string) (coreclient.Snapshot, error) {
		selected, err := corestart.ResolvePaths(settings, directory)
		if err != nil {
			return coreclient.Snapshot{}, err
		}
		if err = coordinator.Ensure(ctx, selected); err != nil {
			return coreclient.Snapshot{}, err
		}
		return coreclient.Load(ctx, selected.URL, selected.Secret)
	}
}

func (m *Model) openCoreConnection(first bool) tea.Cmd {
	settings := m.coreSettings
	if settings.URL == "" {
		settings.URL = preferences.DefaultCoreURL
	}
	yes, no := m.tr.Message("ui.yes"), m.tr.Message("ui.no")
	auto := "false"
	if settings.AutoStart {
		auto = "true"
	}
	fields := []Field{
		TextField("url", m.tr.Message("core.url"), settings.URL, true, false),
		TextField("secret", m.tr.Message("core.secret"), settings.Secret, false, true),
		ChoiceField("auto_start", m.tr.Message("core.auto_start"), auto, Choice{ID: "false", Label: no}, Choice{ID: "true", Label: yes}),
		TextField("executable", m.tr.Message("core.executable"), settings.Executable, true, false),
		TextField("config_file", m.tr.Message("core.config_file"), settings.ConfigFile, true, false),
	}
	for i := 3; i < len(fields); i++ {
		fields[i].When = func(values map[string]string) bool { return values["auto_start"] == "true" }
	}
	form := NewForm(fields...)
	// Source state is independent of string equality: once manually edited the
	// config path remains user-owned, even if it equals a generated value.
	manual := settings.ConfigFile != ""
	generated := settings.ConfigFile
	form.OnFocus = func(index int) {
		if index == 4 && !manual {
			executable := form.Fields[3].Value()
			if executable != "" {
				generated = filepath.Join(filepath.Dir(executable), "config.toml")
				form.Fields[4].Input.SetValue(generated)
			}
		}
	}
	form.BeforeValidate = func() {
		if form.Fields[2].Value() == "true" && form.Fields[4].Value() == "" && !manual {
			form.OnFocus(4)
		}
	}
	modal := &Modal{Kind: CoreConnection, Title: m.tr.Message("core.title"), Form: form}
	modal.OnChange = func() {
		if form.Index == 4 && form.Fields[4].Value() != generated {
			manual = true
		}
	}
	modal.OnKey = func(key tea.KeyPressMsg) (bool, tea.Cmd) {
		if first && key.String() == "esc" {
			return true, nil
		}
		return false, nil
	}
	var cancelRequest context.CancelFunc
	modal.OnCancel = func() tea.Msg {
		if cancelRequest != nil {
			cancelRequest()
		}
		return nil
	}
	modal.OnSubmit = func(values map[string]string) tea.Cmd {
		selected := preferences.Core{URL: values["url"], Secret: values["secret"], AutoStart: values["auto_start"] == "true", Executable: values["executable"], ConfigFile: values["config_file"]}
		id := modal.ID
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		m.cancelSetup = cancel
		cancelRequest = cancel
		directory := m.startupDirectory
		if directory == "" {
			directory, _ = os.Getwd()
		}
		coordinator := m.coordinator
		return func() tea.Msg {
			defer cancel()
			resolved, err := corestart.ResolvePaths(selected, directory)
			if err == nil {
				err = coordinator.Ensure(ctx, resolved)
			}
			return coreConnectionResult{modal: id, settings: resolved, err: err}
		}
	}
	return m.OpenModal(modal)
}

func (m *Model) handleCoreConnection(result coreConnectionResult) tea.Cmd {
	if m.canceled || m.ctx.Err() != nil || m.modal == nil || m.modal.Kind != CoreConnection || m.modal.ID != result.modal {
		return nil
	}
	modal := m.modal
	modal.Form.Busy = false
	if result.err != nil {
		code := coreclient.Code(result.err)
		modal.Body = m.tr.ProtocolError(code)
		if code == "CORE_START_FAILED" {
			var failure *coreclient.Error
			var details struct {
				ExitCode *int `json:"exit_code"`
			}
			if errors.As(result.err, &failure) && json.Unmarshal(failure.Details, &details) == nil && details.ExitCode != nil {
				modal.Body = m.tr.Text("core.start_exit", map[string]any{"Code": *details.ExitCode})
			}
		}
		field := -1
		switch code {
		case "INVALID_URL":
			field = 0
		case "INVALID_SECRET", "UNAUTHORIZED":
			field = 1
		case "EXECUTABLE_INVALID":
			field = 3
		case "CONFIG_PATH_INVALID":
			field = 4
		}
		if field >= 0 {
			modal.Form.Fields[field].Error = modal.Body
			return modal.Form.Focus(field)
		}
		return nil
	}
	// Saving only occurs in Update, after checking cancellation and modal identity.
	if err := m.saveSettings(preferences.Settings{Core: &result.settings}); err != nil {
		modal.Body = m.tr.Message("core.save_failed")
		return nil
	}
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	m.generation++
	m.stopStream()
	if m.snapshot.Client != nil {
		m.snapshot.Client.Close()
	}
	m.snapshot = coreclient.Snapshot{}
	m.hasSnapshot = false
	m.selectedID = ""
	m.healthStatus = ""
	m.configureCore(result.settings)
	m.modal = nil
	m.firstSetup = false
	m.setupComplete = true
	if m.setupOnly {
		return tea.Quit
	}
	return m.beginLoad(true)
}

func (m *Model) cancelConnectionSetup() {
	m.canceled = true
	if m.cancelSetup != nil {
		m.cancelSetup()
	}
}
