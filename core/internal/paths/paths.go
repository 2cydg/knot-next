package paths

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
)

type Layout struct {
	ConfigDir   string `json:"config_dir"`
	StateDir    string `json:"state_dir"`
	RuntimeDir  string `json:"runtime_dir"`
	LogDir      string `json:"log_dir"`
	TokenPath   string `json:"token_path"`
	RuntimePath string `json:"runtime_path"`
	LogPath     string `json:"log_path"`
}

func DefaultLayout() (Layout, error) {
	configBase, err := legacyBaseDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Layout{}, err
	}
	stateBase, err := legacyBaseDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return Layout{}, err
	}
	return NewLayout(filepath.Join(configBase, "knot"), filepath.Join(stateBase, "knot")), nil
}

func NewLayout(configDir, stateDir string) Layout {
	runtimeDir := filepath.Join(stateDir, "runtime")
	logDir := filepath.Join(stateDir, "log")
	return Layout{
		ConfigDir:   configDir,
		StateDir:    stateDir,
		RuntimeDir:  runtimeDir,
		LogDir:      logDir,
		TokenPath:   filepath.Join(runtimeDir, "token"),
		RuntimePath: filepath.Join(runtimeDir, "core.json"),
		LogPath:     filepath.Join(logDir, "core.log"),
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
func legacyBaseDir(env, suffix string) (string, error) {
	if dir := os.Getenv(env); dir != "" {
		return dir, nil
	}
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
