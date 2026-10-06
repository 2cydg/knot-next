package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type LegacyConfig struct {
	Settings      legacySettings                `toml:"settings"`
	Servers       map[string]legacyServer       `toml:"servers"`
	Proxies       map[string]legacyProxy        `toml:"proxies"`
	Keys          map[string]legacyKey          `toml:"keys"`
	SyncProviders map[string]legacySyncProvider `toml:"sync_providers"`
}

type legacySettings struct {
	ForwardAgent          *bool  `toml:"forward_agent"`
	ClearScreenOnConnect  *bool  `toml:"clear_screen_on_connect"`
	BroadcastEscapeEnable *bool  `toml:"broadcast_escape_enable"`
	BroadcastEscapeChar   string `toml:"broadcast_escape_char"`
	IdleTimeout           string `toml:"idle_timeout"`
	KeepaliveInterval     string `toml:"keepalive_interval"`
	LogLevel              string `toml:"log_level"`
	RecentLimit           int    `toml:"recent_limit"`
	DefaultSFTPLocalPath  string `toml:"default_sftp_local_path"`
	DefaultSyncProvider   string `toml:"default_sync_provider"`
	SyncPassword          string `toml:"sync_password"`
}

type legacyServer struct {
	ID             string   `toml:"id"`
	Alias          string   `toml:"alias"`
	Host           string   `toml:"host"`
	Port           int      `toml:"port"`
	User           string   `toml:"user"`
	AuthMethod     string   `toml:"auth_method"`
	Password       string   `toml:"password"`
	KeyID          string   `toml:"key_id"`
	KnownHostsPath string   `toml:"known_hosts_path"`
	ProxyID        string   `toml:"proxy_id"`
	JumpHostIDs    []string `toml:"jump_host_ids"`
	Tags           []string `toml:"tags"`
}

type legacyProxy struct {
	ID       string `toml:"id"`
	Alias    string `toml:"alias"`
	Type     string `toml:"type"`
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

type legacyKey struct {
	ID         string `toml:"id"`
	Alias      string `toml:"alias"`
	Type       string `toml:"type"`
	Length     int    `toml:"length"`
	PrivateKey string `toml:"private_key"`
	SourcePath string `toml:"source_path"`
}

type legacySyncProvider struct {
	ID              string `toml:"id"`
	Alias           string `toml:"alias"`
	Type            string `toml:"type"`
	URL             string `toml:"url"`
	Username        string `toml:"username"`
	Password        string `toml:"password"`
	Bucket          string `toml:"bucket"`
	Key             string `toml:"key"`
	Region          string `toml:"region"`
	Endpoint        string `toml:"endpoint"`
	AccessKeyID     string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
	SessionToken    string `toml:"session_token"`
	PathStyle       bool   `toml:"path_style"`
}

func LoadLegacyConfig(path string) (LegacyConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return LegacyConfig{}, err
	}
	var legacy LegacyConfig
	if _, err := toml.Decode(string(raw), &legacy); err != nil {
		return LegacyConfig{}, err
	}
	if legacy.Servers == nil {
		legacy.Servers = map[string]legacyServer{}
	}
	if legacy.Proxies == nil {
		legacy.Proxies = map[string]legacyProxy{}
	}
	if legacy.Keys == nil {
		legacy.Keys = map[string]legacyKey{}
	}
	if legacy.SyncProviders == nil {
		legacy.SyncProviders = map[string]legacySyncProvider{}
	}
	return legacy, nil
}

func ConvertLegacyConfig(legacy LegacyConfig, current Config, mode string) (Config, []MigrationItem, error) {
	if mode == "" {
		mode = "skip_existing"
	}
	next := cloneConfig(current)
	ensureMaps(&next)
	items := []MigrationItem{}

	if hasLegacySettings(legacy.Settings) {
		action := migrationAction("settings", "", "", hasCurrentSettings(current.Settings), mode)
		items = append(items, MigrationItem{Resource: "settings", Action: action})
		if action == "create" || action == "overwrite" {
			applyLegacySettings(&next.Settings, legacy.Settings)
		}
		if legacy.Settings.SyncPassword != "" {
			items = append(items, MigrationItem{Resource: "settings/sync_password", Action: actionForSecret(action), Reason: secretReason(legacy.Settings.SyncPassword)})
		}
	}

	for _, id := range sortedLegacyProxyIDs(legacy.Proxies) {
		proxy := legacy.Proxies[id]
		profile := ProxyProfile{ID: chooseID(proxy.ID, id), Alias: proxy.Alias, Type: proxy.Type, Host: proxy.Host, Port: proxy.Port, Username: proxy.Username, Password: proxy.Password}
		action, reason := migrationResourceAction(profile.ID, profile.Alias, mode, current.Proxies[profile.ID].Alias != "", proxyAliasExists(current.Proxies, profile.Alias))
		items = append(items, MigrationItem{Resource: "proxies/" + profile.ID, Action: action, Reason: reason})
		if action == "create" || action == "overwrite" {
			next.Proxies[profile.ID] = profile
		}
		if proxy.Password != "" {
			items = append(items, MigrationItem{Resource: "proxies/" + profile.ID + "/password", Action: actionForSecret(action), Reason: secretReason(proxy.Password)})
		}
	}
	for _, id := range sortedLegacyKeyIDs(legacy.Keys) {
		key := legacy.Keys[id]
		profile := KeyMetadata{ID: chooseID(key.ID, id), Alias: key.Alias, Type: key.Type, Length: key.Length, PrivateKey: key.PrivateKey, SourcePath: key.SourcePath}
		action, reason := migrationResourceAction(profile.ID, profile.Alias, mode, current.Keys[profile.ID].Alias != "", keyAliasExists(current.Keys, profile.Alias))
		items = append(items, MigrationItem{Resource: "keys/" + profile.ID, Action: action, Reason: reason})
		if action == "create" || action == "overwrite" {
			next.Keys[profile.ID] = profile
		}
		if key.PrivateKey != "" {
			items = append(items, MigrationItem{Resource: "keys/" + profile.ID + "/private_key", Action: actionForSecret(action), Reason: secretReason(key.PrivateKey)})
		}
	}
	for _, id := range sortedLegacyServerIDs(legacy.Servers) {
		server := legacy.Servers[id]
		profile := ServerProfile{ID: chooseID(server.ID, id), Alias: server.Alias, Host: server.Host, Port: server.Port, User: server.User, AuthMethod: server.AuthMethod, Password: server.Password, KeyID: server.KeyID, KnownHostsPath: server.KnownHostsPath, ProxyID: server.ProxyID, JumpHostIDs: append([]string(nil), server.JumpHostIDs...), Tags: append([]string(nil), server.Tags...)}
		action, reason := migrationResourceAction(profile.ID, profile.Alias, mode, current.Servers[profile.ID].Alias != "", serverAliasExists(current.Servers, profile.Alias))
		if profile.KeyID != "" && legacy.Keys[profile.KeyID].Alias == "" && current.Keys[profile.KeyID].Alias == "" {
			action, reason = "conflict", "referenced key is missing"
		}
		if profile.ProxyID != "" && legacy.Proxies[profile.ProxyID].Alias == "" && current.Proxies[profile.ProxyID].Alias == "" {
			action, reason = "conflict", "referenced proxy is missing"
		}
		items = append(items, MigrationItem{Resource: "servers/" + profile.ID, Action: action, Reason: reason})
		if action == "create" || action == "overwrite" {
			next.Servers[profile.ID] = profile
		}
		if server.Password != "" {
			items = append(items, MigrationItem{Resource: "servers/" + profile.ID + "/password", Action: actionForSecret(action), Reason: secretReason(server.Password)})
		}
	}
	for _, id := range sortedLegacySyncIDs(legacy.SyncProviders) {
		provider := legacy.SyncProviders[id]
		profile := SyncProviderConfig{ID: chooseID(provider.ID, id), Alias: provider.Alias, Type: provider.Type, URL: provider.URL, Username: provider.Username, Password: provider.Password, Bucket: provider.Bucket, Key: provider.Key, Region: provider.Region, Endpoint: provider.Endpoint, AccessKeyID: provider.AccessKeyID, SecretAccessKey: provider.SecretAccessKey, SessionToken: provider.SessionToken, PathStyle: provider.PathStyle}
		action, reason := migrationResourceAction(profile.ID, profile.Alias, mode, current.SyncProviders[profile.ID].Alias != "", syncAliasExists(current.SyncProviders, profile.Alias))
		items = append(items, MigrationItem{Resource: "sync_providers/" + profile.ID, Action: action, Reason: reason})
		if action == "create" || action == "overwrite" {
			next.SyncProviders[profile.ID] = profile
		}
		if provider.Password != "" {
			items = append(items, MigrationItem{Resource: "sync_providers/" + profile.ID + "/password", Action: actionForSecret(action), Reason: secretReason(provider.Password)})
		}
		if provider.AccessKeyID != "" || provider.SecretAccessKey != "" || provider.SessionToken != "" {
			items = append(items, MigrationItem{Resource: "sync_providers/" + profile.ID + "/s3_credentials", Action: actionForSecret(action), Reason: secretReason(firstNonEmpty(provider.AccessKeyID, provider.SecretAccessKey, provider.SessionToken))})
		}
	}
	if legacy.Settings.DefaultSyncProvider != "" {
		if _, ok := next.SyncProviders[legacy.Settings.DefaultSyncProvider]; ok {
			next.Settings.DefaultSyncProvider = legacy.Settings.DefaultSyncProvider
			items = append(items, MigrationItem{Resource: "settings/default_sync_provider", Action: "overwrite"})
		} else {
			items = append(items, MigrationItem{Resource: "settings/default_sync_provider", Action: "conflict", Reason: "default sync provider does not exist"})
		}
	}
	if mode == "fail_on_conflict" && hasMigrationConflict(items) {
		return current, items, nil
	}
	return next, items, nil
}

func hasLegacySettings(settings legacySettings) bool {
	return settings.ForwardAgent != nil || settings.ClearScreenOnConnect != nil || settings.BroadcastEscapeEnable != nil || settings.BroadcastEscapeChar != "" || settings.IdleTimeout != "" || settings.KeepaliveInterval != "" || settings.LogLevel != "" || settings.RecentLimit != 0 || settings.DefaultSFTPLocalPath != "" || settings.DefaultSyncProvider != "" || settings.SyncPassword != ""
}

func hasCurrentSettings(settings Settings) bool {
	defaults := defaultConfig().Settings
	return settings != defaults
}

func applyLegacySettings(settings *Settings, legacy legacySettings) {
	if legacy.ForwardAgent != nil {
		settings.ForwardAgent = *legacy.ForwardAgent
	}
	if legacy.ClearScreenOnConnect != nil {
		settings.ClearScreenOnConnect = *legacy.ClearScreenOnConnect
	}
	if legacy.BroadcastEscapeEnable != nil {
		settings.BroadcastEscapeEnable = *legacy.BroadcastEscapeEnable
	}
	if legacy.BroadcastEscapeChar != "" {
		settings.BroadcastEscapeChar = legacy.BroadcastEscapeChar
	}
	if legacy.IdleTimeout != "" {
		settings.IdleTimeout = legacy.IdleTimeout
	}
	if legacy.KeepaliveInterval != "" {
		settings.KeepaliveInterval = legacy.KeepaliveInterval
	}
	if legacy.LogLevel != "" {
		settings.LogLevel = legacy.LogLevel
	}
	if legacy.RecentLimit != 0 {
		settings.RecentLimit = legacy.RecentLimit
	}
	if legacy.DefaultSFTPLocalPath != "" {
		settings.DefaultSFTPLocalPath = legacy.DefaultSFTPLocalPath
	}
	if legacy.SyncPassword != "" {
		settings.SyncPassword = legacy.SyncPassword
	}
}

func migrationResourceAction(id string, alias string, mode string, idExists bool, aliasExists bool) (string, string) {
	if idExists {
		return migrationAction("", id, alias, true, mode), "id already exists"
	}
	if aliasExists {
		return migrationAction("", id, alias, true, mode), "alias already exists"
	}
	return "create", ""
}

func migrationAction(resource, id, alias string, conflicts bool, mode string) string {
	if !conflicts {
		return "create"
	}
	switch mode {
	case "overwrite":
		return "overwrite"
	case "fail_on_conflict":
		return "conflict"
	default:
		return "skip"
	}
}

func actionForSecret(resourceAction string) string {
	switch resourceAction {
	case "create", "overwrite":
		return resourceAction
	case "skip", "conflict", "error":
		return resourceAction
	default:
		return "create"
	}
}

func secretReason(value string) string {
	if strings.HasPrefix(value, secretPrefix) {
		return "secret will be re-encrypted with the current provider"
	}
	return "plaintext secret will be encrypted with the current provider"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func hasMigrationConflict(items []MigrationItem) bool {
	for _, item := range items {
		if item.Action == "conflict" || item.Action == "error" {
			return true
		}
	}
	return false
}

func chooseID(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

func sortedLegacyProxyIDs(values map[string]legacyProxy) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedLegacyKeyIDs(values map[string]legacyKey) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedLegacyServerIDs(values map[string]legacyServer) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedLegacySyncIDs(values map[string]legacySyncProvider) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func proxyAliasExists(values map[string]ProxyProfile, alias string) bool {
	for _, item := range values {
		if alias != "" && item.Alias == alias {
			return true
		}
	}
	return false
}

func keyAliasExists(values map[string]KeyMetadata, alias string) bool {
	for _, item := range values {
		if alias != "" && item.Alias == alias {
			return true
		}
	}
	return false
}

func serverAliasExists(values map[string]ServerProfile, alias string) bool {
	for _, item := range values {
		if alias != "" && item.Alias == alias {
			return true
		}
	}
	return false
}

func syncAliasExists(values map[string]SyncProviderConfig, alias string) bool {
	for _, item := range values {
		if alias != "" && item.Alias == alias {
			return true
		}
	}
	return false
}

func (l LegacyConfig) String() string {
	return fmt.Sprintf("servers=%d proxies=%d keys=%d sync_providers=%d", len(l.Servers), len(l.Proxies), len(l.Keys), len(l.SyncProviders))
}
