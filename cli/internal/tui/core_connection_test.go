package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/corestart"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func setupModel(t *testing.T, args ...string) *Model {
	t.Helper()
	m := testModel(t, args...)
	m.firstSetup, m.setupOnly = true, true
	m.snapshot = coreclient.Snapshot{}
	m.state = Offline
	m.startupDirectory = t.TempDir()
	m.coordinator = corestart.New(corestart.Dependencies{Probe: func(context.Context, preferences.Core) error { return nil }})
	m.Init()
	return m
}
func TestFirstSetupGatesEveryEntryAndAllowsCancel(t *testing.T) {
	for _, args := range [][]string{nil, {"key"}, {"proxy"}, {"sync"}, {"ssh", "production"}} {
		m := setupModel(t, args...)
		page := m.page
		id := m.modal.ID
		for _, key := range []string{"esc", "n", "?", "[", "tab"} {
			m.Update(press(key))
			if m.modal == nil || m.modal.ID != id || m.page != page || m.setupComplete {
				t.Fatal("startup intent escaped setup", args, key)
			}
		}
		m.Update(press("ctrl+c"))
		if !m.canceled || m.exitCode != entry.ExitCanceled {
			t.Fatal("first setup cannot exit")
		}
	}
}
func TestCoreFormDynamicPathsTrackManualAndAutomaticSources(t *testing.T) {
	m := setupModel(t)
	f := m.modal.Form
	if f.visible(3) || f.visible(4) {
		t.Fatal("auto-start fields visible by default")
	}
	f.Focus(2)
	m.Update(press("right"))
	if !f.visible(3) || !f.visible(4) {
		t.Fatal("auto-start fields not visible")
	}
	f.Fields[3].Input.SetValue(filepath.Join("~", "bin space", "core"))
	f.Focus(4)
	if f.Fields[4].Value() != filepath.Join("~", "bin space", "config.toml") {
		t.Fatal("missing automatic path", f.Values())
	}
	f.Focus(3)
	f.Fields[3].Input.SetValue(filepath.Join("another", "core"))
	f.Focus(4)
	if f.Fields[4].Value() != filepath.Join("another", "config.toml") {
		t.Fatal("automatic source did not update")
	}
	f.Fields[4].Input.SetValue("manually supplied.toml")
	m.modal.OnChange()
	f.Focus(3)
	f.Fields[3].Input.SetValue(filepath.Join("third", "core"))
	f.Focus(4)
	if f.Fields[4].Value() != "manually supplied.toml" {
		t.Fatal("manual path overwritten")
	}
	f.Fields[4].Input.SetValue("")
	m.modal.OnChange()
	m.submitForm()
	if !f.Invalid() || f.Index != 4 {
		t.Fatal("manually cleared required path was overwritten")
	}
}
func TestCoreFormBilingualSizesAndSecretMasking(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			m := setupModel(t)
			m.tr = i18n.New(language)
			m.coreSettings = preferences.Core{Secret: "never-render-this-secret", AutoStart: true}
			m.openCoreConnection(true)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := ansi.Strip(m.View().Content)
			if strings.Contains(view, "never-render-this-secret") || !strings.Contains(view, "Ctrl+c") {
				t.Fatal("Secret leaked or exit hint missing")
			}
			checkSize(t, view, size)
			for i := range m.modal.Form.Fields {
				m.modal.Form.Focus(i)
				checkSize(t, ansi.Strip(m.View().Content), size)
			}
		}
	}
}
func TestCoreSaveWaitsForProbeFiltersOldResultsAndResumesIntent(t *testing.T) {
	for _, args := range [][]string{nil, {"key"}, {"ssh", "production"}} {
		m := setupModel(t, args...)
		var saved *preferences.Core
		m.saveSettings = func(s preferences.Settings) error { saved = s.Core; return nil }
		page := m.page
		_, cmd := m.Update(press("ctrl+s"))
		if cmd == nil || !m.modal.Form.Busy || saved != nil {
			t.Fatal("submitted before probe")
		}
		if _, cmd := m.Update(press("ctrl+s")); cmd != nil {
			t.Fatal("duplicate submission")
		}
		result := cmd().(coreConnectionResult)
		m.Update(coreConnectionResult{modal: result.modal + 1, settings: result.settings})
		if saved != nil || !m.modal.Form.Busy {
			t.Fatal("stale result accepted")
		}
		m.Update(result)
		if saved == nil || !m.setupComplete || m.firstSetup || m.modal != nil || m.page != page {
			t.Fatal("intent not preserved after setup")
		}
	}
}
func TestCoreSaveErrorsStayInFormAndCanceledResultCannotSave(t *testing.T) {
	m := setupModel(t)
	saves := 0
	m.saveSettings = func(preferences.Settings) error { saves++; return errors.New("raw-secret must not be rendered") }
	id := m.modal.ID
	for code, index := range map[string]int{"INVALID_URL": 0, "UNAUTHORIZED": 1, "EXECUTABLE_INVALID": 3, "CONFIG_PATH_INVALID": 4} {
		m.Update(coreConnectionResult{modal: id, err: coreclient.Failure(code)})
		if m.modal == nil || m.modal.Form.Index != index || m.modal.Form.Fields[index].Error == "" || saves != 0 {
			t.Fatal("field failure was not retained", code)
		}
	}
	m.Update(coreConnectionResult{modal: id, settings: preferences.Core{URL: preferences.DefaultCoreURL}})
	if saves != 1 || m.modal == nil || strings.Contains(m.modal.Body, "raw-secret") {
		t.Fatal("save failure leaked detail or closed form")
	}
	m.Update(press("ctrl+c"))
	m.Update(coreConnectionResult{modal: id, settings: preferences.Core{URL: preferences.DefaultCoreURL}})
	if saves != 1 || m.setupComplete {
		t.Fatal("canceled setup persisted settings")
	}
}
func TestSettingsCoreEntryWorksWhileOffline(t *testing.T) {
	m := testModel(t)
	m.page, m.focus, m.settingsTab, m.state = entry.Settings, OperationsFocus, 1, Offline
	m.Update(press("enter"))
	if m.modal == nil || m.modal.Kind != CoreConnection {
		t.Fatal("core settings inaccessible offline")
	}
}
