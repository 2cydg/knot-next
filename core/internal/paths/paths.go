package paths

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
)

type Layout struct {
	ConfigFile string `json:"-"`
	ConfigDir  string `json:"config_dir"`
	StateDir   string `json:"state_dir"`
	RuntimeDir string `json:"runtime_dir"`
	LogDir     string `json:"log_dir"`
	LogPath    string `json:"log_path"`
}

func DefaultLayout() (Layout, error) {
	configBase, err := legacyBaseDir(".config")
	if err != nil {
		return Layout{}, err
	}
	stateBase, err := legacyBaseDir(filepath.Join(".local", "state"))
	if err != nil {
		return Layout{}, err
	}
	return NewLayout(filepath.Join(configBase, "knot"), filepath.Join(stateBase, "knot")), nil
}

func NewLayout(configDir, stateDir string) Layout {
	runtimeDir := filepath.Join(stateDir, "runtime")
	logDir := filepath.Join(stateDir, "log")
	return Layout{
		ConfigDir:  configDir,
		StateDir:   stateDir,
		RuntimeDir: runtimeDir,
		LogDir:     logDir,
		LogPath:    filepath.Join(logDir, "core.log"),
	}
}

func (l Layout) Ensure() error {
	for _, dir := range []string{l.ConfigDir, l.StateDir, l.RuntimeDir, l.LogDir} {
		if dir == "" {
			return errors.New("path layout contains an empty directory")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Legacy Knot used the same home fallbacks on all three platforms.
func legacyBaseDir(suffix string) (string, error) {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, suffix), nil
	}
	usr, userErr := user.Current()
	if userErr == nil && usr.HomeDir != "" {
		return filepath.Join(usr.HomeDir, suffix), nil
	}
	if err != nil {
		return "", err
	}
	return "", userErr
}

// ConfigurationFile is the single source for configuration writes and locks.
func (l Layout) ConfigurationFile() string {
	if l.ConfigFile != "" {
		return l.ConfigFile
	}
	return filepath.Join(l.ConfigDir, "config.toml")
}

// WithConfig anchors explicit files and their state to a canonical directory.
// Configurations sharing crypto/trust state also share the instance lock.
func WithConfig(path string) (Layout, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Layout{}, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return Layout{}, err
	}
	dir := filepath.Dir(absolute)
	layout := NewLayout(dir, filepath.Join(dir, ".knot-state"))
	layout.ConfigFile = absolute
	return layout, nil
}
