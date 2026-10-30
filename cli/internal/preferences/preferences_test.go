package preferences

import (
	"os"
	"path/filepath"
	"testing"

	"knot-cli/internal/i18n"
)

func TestClientSettingsRoundTripAndIndependentFile(t *testing.T) {
	base := t.TempDir()
	setTestHome(t, base)
	path, err := Path()
	if err != nil || path != filepath.Join(base, ".config", "knot", "client.toml") {
		t.Fatal("unexpected client settings path", path, err)
	}
	if settings, err := Load(); err != nil || settings.Language != "" {
		t.Fatal("missing file must keep system detection", settings, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("reading missing settings must not create directories", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	corePath := filepath.Join(filepath.Dir(path), "config.toml")
	const coreConfig = "# core config must remain untouched\n"
	if err := os.WriteFile(corePath, []byte(coreConfig), 0600); err != nil {
		t.Fatal(err)
	}
	for _, language := range []i18n.Language{i18n.Chinese, i18n.English} {
		if err := Save(Settings{Language: language}); err != nil {
			t.Fatal(err)
		}
		settings, err := Load()
		if err != nil || settings.Language != language {
			t.Fatal("language did not survive a file reload", settings, err)
		}
	}
	if body, err := os.ReadFile(corePath); err != nil || string(body) != coreConfig {
		t.Fatal("client settings changed core config", string(body), err)
	}
	if err := Save(Settings{Language: "invalid"}); err == nil {
		t.Fatal("invalid language must not overwrite saved settings")
	}
	if settings, err := Load(); err != nil || settings.Language != i18n.English {
		t.Fatal("invalid save lost the existing settings", settings, err)
	}
}

func TestClientSettingsTOMLValidationAndWriteFailure(t *testing.T) {
	setTestHome(t, t.TempDir())
	if err := Save(Settings{Language: i18n.Chinese}); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"language = [", "language = 5", "language = 'fr'"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if settings, err := Load(); err == nil || settings.Language != "" {
			t.Fatal("invalid TOML must return an error without selecting a language", settings, err)
		}
	}
	if err := os.WriteFile(path, []byte("# comment\nlanguage = 'zh'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if settings, err := Load(); err != nil || settings.Language != i18n.Chinese {
		t.Fatal("valid TOML comments and literal strings must work", settings, err)
	}
	// A file in place of the config directory produces an error on all platforms.
	setTestHome(t, path)
	if err := Save(Settings{Language: i18n.English}); err == nil {
		t.Fatal("unwritable settings must report failure")
	}
}

func TestLanguageAndCoreSavesPreserveEachOther(t *testing.T) {
	setTestHome(t, t.TempDir())
	if err := Save(Settings{Language: i18n.Chinese}); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load()
	if err != nil || legacy.Configured() {
		t.Fatal("language-only preferences must require setup", err)
	}
	core := Core{URL: DefaultCoreURL, Secret: "", AutoStart: true, Executable: "/path with spaces/core", ConfigFile: "/separate/config.toml"}
	if err := Save(Settings{Core: &core}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Settings{Language: i18n.English}); err != nil {
		t.Fatal(err)
	}
	saved, err := Load()
	if err != nil || !saved.Configured() || *saved.Core != core || saved.Language != i18n.English {
		t.Fatal("settings group was lost", err)
	}
	core.Secret = "private-secret"
	if err := Save(Settings{Core: &core}); err != nil {
		t.Fatal(err)
	}
	saved, err = Load()
	if err != nil || saved.Language != i18n.English || saved.Core.Secret != core.Secret {
		t.Fatal("core save lost language", err)
	}
	path, _ := Path()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("client credentials have permissive mode", err)
	}
}

func TestIncompleteAutoStartPreferencesRequireSetup(t *testing.T) {
	for _, core := range []*Core{nil, {}, {URL: " "}, {URL: DefaultCoreURL, AutoStart: true}, {URL: DefaultCoreURL, AutoStart: true, Executable: "core"}} {
		if (Settings{Core: core}).Configured() {
			t.Fatal("incomplete preferences bypass setup")
		}
	}
	if !(Settings{Core: &Core{URL: DefaultCoreURL}}).Configured() {
		t.Fatal("empty Secret requires configuration")
	}
}

func TestClientPathIgnoresXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := Path()
	if err != nil || path != filepath.Join(home, ".config", "knot", "client.toml") {
		t.Fatal("client path used XDG_CONFIG_HOME", path, err)
	}
}
