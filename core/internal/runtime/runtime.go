package runtime

import (
	"os"
	"time"

	"knot-core/internal/paths"
)

type Info struct {
	Version         string    `json:"version"`
	APIVersion      string    `json:"api_version"`
	InstanceID      string    `json:"instance_id"`
	PID             int       `json:"pid"`
	Port            int       `json:"port"`
	ListenAddresses []string  `json:"listen_addresses"`
	TokenPresent    bool      `json:"token_present"`
	LogPath         string    `json:"log_path"`
	ConfigDir       string    `json:"config_dir"`
	StateDir        string    `json:"state_dir"`
	StartedAt       time.Time `json:"started_at"`
}

func NewInfo(version, apiVersion, instanceID string, port int, listenAddresses []string, layout paths.Layout, startedAt time.Time, tokenPresent bool) Info {
	return Info{
		Version:         version,
		APIVersion:      apiVersion,
		InstanceID:      instanceID,
		PID:             os.Getpid(),
		Port:            port,
		ListenAddresses: append([]string(nil), listenAddresses...),
		TokenPresent:    tokenPresent,
		LogPath:         layout.LogPath,
		ConfigDir:       layout.ConfigDir,
		StateDir:        layout.StateDir,
		StartedAt:       startedAt.UTC(),
	}
}
