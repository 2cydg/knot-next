// Package preferences stores client-only settings independently of core config.
package preferences

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"knot-cli/internal/i18n"
)

type Settings struct {
	Language i18n.Language `toml:"language"`
}

// Path uses the same XDG/home layout as core, with a separate client file.
func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	base, err := filepath.Abs(base)
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
		return Settings{}, err
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
