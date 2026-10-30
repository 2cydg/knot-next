package preferences

import (
	"os"
	"path/filepath"
	"testing"

	"knot-cli/internal/i18n"
)

func TestClientSettingsRoundTripAndIndependentFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	path, err := Path()
	if err != nil || path != filepath.Join(base, "knot", "client.toml") {
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	t.Setenv("XDG_CONFIG_HOME", path)
	if err := Save(Settings{Language: i18n.English}); err == nil {
		t.Fatal("unwritable settings must report failure")
	}
}
