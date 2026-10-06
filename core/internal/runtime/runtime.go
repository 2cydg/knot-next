package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"knot-core/internal/paths"
)

type Info struct {
	Version         string    `json:"version"`
	APIVersion      string    `json:"api_version"`
	PID             int       `json:"pid"`
	Port            int       `json:"port"`
	ListenAddresses []string  `json:"listen_addresses"`
	TokenPath       string    `json:"token_path"`
	TokenPresent    bool      `json:"token_present"`
	LogPath         string    `json:"log_path"`
	RuntimePath     string    `json:"runtime_path"`
	ConfigDir       string    `json:"config_dir"`
	StateDir        string    `json:"state_dir"`
	StartedAt       time.Time `json:"started_at"`
}

func NewInfo(version, apiVersion string, port int, listenAddresses []string, layout paths.Layout, startedAt time.Time, tokenPresent bool) Info {
	return Info{
		Version:         version,
		APIVersion:      apiVersion,
		PID:             os.Getpid(),
		Port:            port,
		ListenAddresses: append([]string(nil), listenAddresses...),
		TokenPath:       layout.TokenPath,
		TokenPresent:    tokenPresent,
		LogPath:         layout.LogPath,
		RuntimePath:     layout.RuntimePath,
		ConfigDir:       layout.ConfigDir,
		StateDir:        layout.StateDir,
		StartedAt:       startedAt.UTC(),
	}
}

func WriteInfo(path string, info Info) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".core-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if err := json.NewEncoder(tmp).Encode(info); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
