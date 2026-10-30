// Package coreclient implements the public core protocol without importing core.
package coreclient

import (
	"encoding/json"
	"time"
)

type Runtime struct {
	Version         string   `json:"version"`
	APIVersion      string   `json:"api_version"`
	InstanceID      string   `json:"instance_id"`
	Port            int      `json:"port"`
	ListenAddresses []string `json:"listen_addresses"`
}

type Version struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}
type Health struct {
	Status        string        `json:"status"`
	Authenticated bool          `json:"authenticated"`
	Checks        []HealthCheck `json:"checks"`
}
type HealthCheck struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Available *bool  `json:"available,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
type Capability struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Supported bool   `json:"supported"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}
type Settings struct {
	SyncPasswordSet bool `json:"sync_password_set"`
}
type Summary struct {
	Settings      Settings       `json:"settings"`
	Servers       []Server       `json:"servers"`
	Keys          []Key          `json:"keys"`
	Proxies       []Proxy        `json:"proxies"`
	SyncProviders []SyncProvider `json:"sync_providers"`
}
type Server struct {
	ID             string     `json:"id"`
	Alias          string     `json:"alias"`
	Host           string     `json:"host"`
	Port           int        `json:"port"`
	User           string     `json:"user"`
	AuthMethod     string     `json:"auth_method"`
	PasswordSet    bool       `json:"password_set"`
	KeyID          string     `json:"key_id,omitempty"`
	KnownHostsPath string     `json:"known_hosts_path,omitempty"`
	ProxyID        string     `json:"proxy_id,omitempty"`
	JumpHostIDs    []string   `json:"jump_host_ids,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
	LastUsed       *time.Time `json:"last_used"`
}
type Key struct {
	ID            string `json:"id"`
	Alias         string `json:"alias"`
	Type          string `json:"type"`
	Length        int    `json:"length"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Encrypted     bool   `json:"encrypted"`
	PrivateKeySet bool   `json:"private_key_set"`
	SourcePath    string `json:"source_path,omitempty"`
}
type Proxy struct {
	ID          string `json:"id"`
	Alias       string `json:"alias"`
	Type        string `json:"type"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"password_set"`
}
type SyncProvider struct {
	ID                 string `json:"id"`
	Alias              string `json:"alias"`
	Type               string `json:"type"`
	URL                string `json:"url"`
	Username           string `json:"username"`
	PasswordSet        bool   `json:"password_set"`
	Bucket             string `json:"bucket"`
	Key                string `json:"key"`
	Region             string `json:"region"`
	Endpoint           string `json:"endpoint"`
	AccessKeyIDSet     bool   `json:"access_key_id_set"`
	SecretAccessKeySet bool   `json:"secret_access_key_set"`
	SessionTokenSet    bool   `json:"session_token_set"`
	PathStyle          bool   `json:"path_style"`
	Default            bool   `json:"default"`
}
type Event struct {
	Type       string          `json:"type"`
	APIVersion string          `json:"api_version,omitempty"`
	Resource   string          `json:"resource,omitempty"`
	ResourceID string          `json:"resource_id,omitempty"`
	Level      string          `json:"level,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// Snapshot always contains newly decoded DTOs. Replace it, never merge it:
// omitted fields in a complete response can represent cleared associations.
type Snapshot struct {
	Client       *Client
	Runtime      Runtime
	Version      Version
	Health       Health
	Capabilities []Capability
	Config       Summary
}

func (s Snapshot) Available(name string) bool {
	for _, c := range s.Capabilities {
		if c.Name == name {
			return c.Available
		}
	}
	return false
}
