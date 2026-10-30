// Package preferences stores client-only settings independently of core config.
package preferences

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"knot-cli/internal/i18n"
)

type Settings struct {
	Language i18n.Language `toml:"language"`
	Core     *Core         `toml:"core,omitempty"`
}

const DefaultCoreURL = "http://127.0.0.1:17898"

type Core struct {
	URL        string `toml:"url"`
	Secret     string `toml:"secret" json:"-"`
	AutoStart  bool   `toml:"auto_start"`
	Executable string `toml:"executable"`
	ConfigFile string `toml:"config_file"`
}

func (s Settings) Configured() bool {
	if s.Core == nil || strings.TrimSpace(s.Core.URL) == "" {
		return false
	}
	return !s.Core.AutoStart || strings.TrimSpace(s.Core.Executable) != "" && strings.TrimSpace(s.Core.ConfigFile) != ""
}

// Path places client preferences under the user home directory.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, ".config")
	base, err = filepath.Abs(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "knot", "client.toml"), nil
}

func (s Settings) validate() error {
	if s.Language != "" && s.Language != i18n.English && s.Language != i18n.Chinese {
		return fmt.Errorf("unsupported client language %q", s.Language)
	}
	return nil
}

// A missing file or language keeps automatic system language detection.
func Load() (Settings, error) {
	path, err := Path()
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if _, err := toml.DecodeFile(path, &settings); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Settings{}, nil
		}
		var pathError *os.PathError
		if errors.As(err, &pathError) {
			return Settings{}, err
		}
		return Settings{}, errors.New("invalid client preferences TOML")
	}
	if err := settings.validate(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// Save replaces the file only after the complete TOML has been written.
func Save(settings Settings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	existing, err := Load()
	if err != nil {
		return err
	}
	if settings.Core == nil {
		settings.Core = existing.Core
	}
	if settings.Language == "" {
		settings.Language = existing.Language
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".client-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := toml.NewEncoder(f).Encode(settings); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
