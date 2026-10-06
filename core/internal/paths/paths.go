package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const appName = "knot-core"

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
	configBase, err := os.UserConfigDir()
	if err != nil {
		return Layout{}, err
	}
	stateBase, err := userStateDir()
	if err != nil {
		return Layout{}, err
	}
	return NewLayout(filepath.Join(configBase, appName), filepath.Join(stateBase, appName)), nil
}

func NewLayout(configDir, stateDir string) Layout {
	runtimeDir := filepath.Join(stateDir, "runtime")
	logDir := filepath.Join(stateDir, "log")
	return Layout{
		ConfigDir:   configDir,
		StateDir:    stateDir,
		RuntimeDir:  runtimeDir,
		LogDir:      logDir,
		TokenPath:   filepath.Join(configDir, "token"),
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

func userStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir, nil
	}
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return dir, nil
		}
		return os.UserConfigDir()
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state"), nil
	}
}
