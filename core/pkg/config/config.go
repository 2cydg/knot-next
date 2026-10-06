package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"knot-core/internal/fileutil"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

const (
	AuthMethodPassword = "password"
	AuthMethodKey      = "key"
	AuthMethodAgent    = "agent"

	ProxyTypeSOCKS5 = "socks5"
	ProxyTypeHTTP   = "http"

	SyncProviderWebDAV = "webdav"
	SyncProviderS3     = "s3"

	secretPrefix = "ENC:"
)

var (
	ErrNotFound   = errors.New("config resource not found")
	ErrConflict   = errors.New("config resource conflict")
	ErrValidation = errors.New("config validation failed")
)

type Service struct {
	mu       sync.Mutex
	path     string
	layout   paths.Layout
	crypto   crypto.Provider
	loaded   bool
	cfg      Config
	modified time.Time
}

func NewService(layout paths.Layout, provider crypto.Provider) *Service {
	return &Service{
		path:   filepath.Join(layout.ConfigDir, "config.json"),
		layout: layout,
		crypto: provider,
	}
}

type Config struct {
	SchemaVersion int                           `json:"schema_version"`
	Settings      Settings                      `json:"settings"`
	Servers       map[string]ServerProfile      `json:"servers"`
	Proxies       map[string]ProxyProfile       `json:"proxies"`
	Keys          map[string]KeyMetadata        `json:"keys"`
	SyncProviders map[string]SyncProviderConfig `json:"sync_providers"`
	UpdatedAt     time.Time                     `json:"updated_at"`
}

type Settings struct {
	ForwardAgent          bool   `json:"forward_agent"`
	ClearScreenOnConnect  bool   `json:"clear_screen_on_connect"`
	BroadcastEscapeEnable bool   `json:"broadcast_escape_enable"`
	BroadcastEscapeChar   string `json:"broadcast_escape_char"`
	IdleTimeout           string `json:"idle_timeout"`
	KeepaliveInterval     string `json:"keepalive_interval"`
	LogLevel              string `json:"log_level"`
	RecentLimit           int    `json:"recent_limit"`
	DefaultSFTPLocalPath  string `json:"default_sftp_local_path,omitempty"`
	DefaultSyncProvider   string `json:"default_sync_provider,omitempty"`
	SyncPassword          string `json:"sync_password,omitempty"`
}

type ServerProfile struct {
	ID             string   `json:"id"`
	Alias          string   `json:"alias"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	User           string   `json:"user"`
	AuthMethod     string   `json:"auth_method,omitempty"`
	Password       string   `json:"password,omitempty"`
	KeyID          string   `json:"key_id,omitempty"`
	KnownHostsPath string   `json:"known_hosts_path,omitempty"`
	ProxyID        string   `json:"proxy_id,omitempty"`
	JumpHostIDs    []string `json:"jump_host_ids,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

type ProxyProfile struct {
	ID       string `json:"id"`
	Alias    string `json:"alias"`
	Type     string `json:"type,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type KeyMetadata struct {
	ID         string `json:"id"`
	Alias      string `json:"alias"`
	Type       string `json:"type,omitempty"`
	Length     int    `json:"length,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
}

type SyncProviderConfig struct {
	ID              string `json:"id"`
	Alias           string `json:"alias"`
	Type            string `json:"type"`
	URL             string `json:"url,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Key             string `json:"key,omitempty"`
	Region          string `json:"region,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
	PathStyle       bool   `json:"path_style,omitempty"`
}

type RuntimeConfig struct {
	Settings      Settings
	Servers       map[string]ServerProfile
	Proxies       map[string]ProxyProfile
	Keys          map[string]KeyMetadata
	SyncProviders map[string]SyncProviderConfig
}

type Page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

type ServerListOptions struct {
	Alias      string
	Tag        []string
	AuthMethod string
	ProxyID    string
	Query      string
	Limit      int
	Offset     int
	Sort       string
}

type Summary struct {
	SchemaVersion int                 `json:"schema_version"`
	Settings      SettingsView        `json:"settings"`
	Servers       []ServerProfileView `json:"servers"`
	Proxies       []ProxyProfileView  `json:"proxies"`
	Keys          []KeyMetadataView   `json:"keys"`
	SyncProviders []SyncProviderView  `json:"sync_providers"`
	UpdatedAt     time.Time           `json:"updated_at"`
	Metadata      Metadata            `json:"metadata"`
}

type Metadata struct {
	SchemaVersion int       `json:"schema_version"`
	Source        string    `json:"source"`
	ConfigPath    string    `json:"config_path"`
	Migration     Migration `json:"migration"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type SettingsView struct {
	ForwardAgent          SettingValue `json:"forward_agent"`
	ClearScreenOnConnect  SettingValue `json:"clear_screen_on_connect"`
	BroadcastEscapeEnable SettingValue `json:"broadcast_escape_enable"`
	BroadcastEscapeChar   SettingValue `json:"broadcast_escape_char"`
	IdleTimeout           SettingValue `json:"idle_timeout"`
	KeepaliveInterval     SettingValue `json:"keepalive_interval"`
	LogLevel              SettingValue `json:"log_level"`
	RecentLimit           SettingValue `json:"recent_limit"`
	DefaultSFTPLocalPath  SettingValue `json:"default_sftp_local_path"`
	DefaultSyncProvider   SettingValue `json:"default_sync_provider"`
	SyncPasswordSet       bool         `json:"sync_password_set"`
}

type SettingValue struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`
	Default any    `json:"default"`
	Type    string `json:"type"`
}

type ServerProfileView struct {
	ID             string   `json:"id"`
	Alias          string   `json:"alias"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	User           string   `json:"user"`
	AuthMethod     string   `json:"auth_method,omitempty"`
	PasswordSet    bool     `json:"password_set"`
	KeyID          string   `json:"key_id,omitempty"`
	KnownHostsPath string   `json:"known_hosts_path,omitempty"`
	ProxyID        string   `json:"proxy_id,omitempty"`
	JumpHostIDs    []string `json:"jump_host_ids,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

type ProxyProfileView struct {
	ID          string `json:"id"`
	Alias       string `json:"alias"`
	Type        string `json:"type,omitempty"`
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	Username    string `json:"username,omitempty"`
	PasswordSet bool   `json:"password_set"`
}

type KeyMetadataView struct {
	ID            string `json:"id"`
	Alias         string `json:"alias"`
	Type          string `json:"type,omitempty"`
	Length        int    `json:"length,omitempty"`
	PrivateKeySet bool   `json:"private_key_set"`
	SourcePath    string `json:"source_path,omitempty"`
}

type SyncProviderView struct {
	ID                 string `json:"id"`
	Alias              string `json:"alias"`
	Type               string `json:"type"`
	URL                string `json:"url,omitempty"`
	Username           string `json:"username,omitempty"`
	PasswordSet        bool   `json:"password_set"`
	Bucket             string `json:"bucket,omitempty"`
	Key                string `json:"key,omitempty"`
	Region             string `json:"region,omitempty"`
	Endpoint           string `json:"endpoint,omitempty"`
	AccessKeyIDSet     bool   `json:"access_key_id_set"`
	SecretAccessKeySet bool   `json:"secret_access_key_set"`
	SessionTokenSet    bool   `json:"session_token_set"`
	PathStyle          bool   `json:"path_style,omitempty"`
	Default            bool   `json:"default"`
}

type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors,omitempty"`
}

type ValidationError struct {
	Resource string `json:"resource"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

type Migration struct {
	LegacyPath       string `json:"legacy_path"`
	LegacyExists     bool   `json:"legacy_exists"`
	NeedsMigration   bool   `json:"needs_migration"`
	ConflictDetected bool   `json:"conflict_detected"`
}

type MigrationPlan struct {
	Migration Migration       `json:"migration"`
	Items     []MigrationItem `json:"items"`
}

type MigrationItem struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
	Reason   string `json:"reason,omitempty"`
}

type MigrationApplyRequest struct {
	Mode string `json:"mode"`
}

func (s *Service) Summary() (Summary, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return Summary{}, err
	}
	return summaryFromConfig(cfg, s.metadataFor(cfg)), nil
}

func (s *Service) Metadata() (Metadata, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return Metadata{}, err
	}
	return s.metadataFor(cfg), nil
}

func (s *Service) ValidateConfig(cfg Config) ValidationResult {
	return validate(cfg)
}

func (s *Service) Settings() (SettingsView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return SettingsView{}, err
	}
	return settingsView(cfg.Settings), nil
}

func (s *Service) Setting(key string) (SettingValue, error) {
	view, err := s.Settings()
	if err != nil {
		return SettingValue{}, err
	}
	values := settingMap(view)
	value, ok := values[key]
	if !ok {
		return SettingValue{}, ErrNotFound
	}
	return value, nil
}

func (s *Service) UpdateSetting(key string, value any) (SettingValue, error) {
	var out SettingValue
	err := s.withConfig(func(cfg *Config) error {
		if err := setSetting(&cfg.Settings, key, value); err != nil {
			return err
		}
		out = settingMap(settingsView(cfg.Settings))[key]
		return nil
	})
	return out, err
}

func (s *Service) ResetSetting(key string) (SettingValue, error) {
	defaults := defaultConfig().Settings
	var out SettingValue
	err := s.withConfig(func(cfg *Config) error {
		switch key {
		case "forward_agent":
			cfg.Settings.ForwardAgent = defaults.ForwardAgent
		case "clear_screen_on_connect":
			cfg.Settings.ClearScreenOnConnect = defaults.ClearScreenOnConnect
		case "broadcast_escape_enable":
			cfg.Settings.BroadcastEscapeEnable = defaults.BroadcastEscapeEnable
		case "broadcast_escape_char":
			cfg.Settings.BroadcastEscapeChar = defaults.BroadcastEscapeChar
		case "idle_timeout":
			cfg.Settings.IdleTimeout = defaults.IdleTimeout
		case "keepalive_interval":
			cfg.Settings.KeepaliveInterval = defaults.KeepaliveInterval
		case "log_level":
			cfg.Settings.LogLevel = defaults.LogLevel
		case "recent_limit":
			cfg.Settings.RecentLimit = defaults.RecentLimit
		case "default_sftp_local_path":
			cfg.Settings.DefaultSFTPLocalPath = defaults.DefaultSFTPLocalPath
		case "default_sync_provider":
			cfg.Settings.DefaultSyncProvider = defaults.DefaultSyncProvider
		default:
			return ErrNotFound
		}
		out = settingMap(settingsView(cfg.Settings))[key]
		return nil
	})
	return out, err
}

func (s *Service) ListServers() ([]ServerProfileView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return serverViews(cfg), nil
}

func (s *Service) ListServersPage(opts ServerListOptions) (Page[ServerProfileView], error) {
	cfg, err := s.snapshot()
	if err != nil {
		return Page[ServerProfileView]{}, err
	}
	items := filterServerViews(serverViews(cfg), opts)
	total := len(items)
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return Page[ServerProfileView]{
		Items:  items[offset:end],
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

func (s *Service) GetServer(id string) (ServerProfileView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return ServerProfileView{}, err
	}
	server, ok := cfg.Servers[id]
	if !ok {
		return ServerProfileView{}, ErrNotFound
	}
	return serverView(server), nil
}

func (s *Service) CreateServer(server ServerProfile) (ServerProfileView, error) {
	var out ServerProfileView
	err := s.withConfig(func(cfg *Config) error {
		if server.Password != "" {
			return secretFieldError("server password")
		}
		if server.ID == "" {
			server.ID = newID("srv")
		}
		if _, ok := cfg.Servers[server.ID]; ok {
			return ErrConflict
		}
		if aliasExistsServer(*cfg, server.Alias, server.ID) {
			return ErrConflict
		}
		normalizeServer(&server)
		cfg.Servers[server.ID] = server
		out = serverView(server)
		return nil
	})
	return out, err
}

func (s *Service) UpdateServer(id string, server ServerProfile) (ServerProfileView, error) {
	var out ServerProfileView
	err := s.withConfig(func(cfg *Config) error {
		if server.Password != "" {
			return secretFieldError("server password")
		}
		current, ok := cfg.Servers[id]
		if !ok {
			return ErrNotFound
		}
		if server.ID == "" {
			server.ID = id
		}
		if server.ID != id {
			return fmt.Errorf("%w: id cannot be changed", ErrValidation)
		}
		if aliasExistsServer(*cfg, server.Alias, id) {
			return ErrConflict
		}
		if server.Password == "" {
			server.Password = current.Password
		}
		normalizeServer(&server)
		cfg.Servers[id] = server
		out = serverView(server)
		return nil
	})
	return out, err
}

func (s *Service) DeleteServer(id string) error {
	return s.withConfig(func(cfg *Config) error {
		if _, ok := cfg.Servers[id]; !ok {
			return ErrNotFound
		}
		delete(cfg.Servers, id)
		return nil
	})
}

func (s *Service) ResolveServer(ref string) (ServerProfileView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return ServerProfileView{}, err
	}
	if server, ok := cfg.Servers[ref]; ok {
		return serverView(server), nil
	}
	for _, server := range cfg.Servers {
		if server.Alias == ref {
			return serverView(server), nil
		}
	}
	return ServerProfileView{}, ErrNotFound
}

func (s *Service) RuntimeConfig() (RuntimeConfig, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return RuntimeConfig{}, err
	}
	if err := s.decryptRuntimeSecrets(&cfg); err != nil {
		return RuntimeConfig{}, err
	}
	return RuntimeConfig{
		Settings:      cfg.Settings,
		Servers:       cfg.Servers,
		Proxies:       cfg.Proxies,
		Keys:          cfg.Keys,
		SyncProviders: cfg.SyncProviders,
	}, nil
}

func (s *Service) ListProxies() ([]ProxyProfileView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return proxyViews(cfg), nil
}

func (s *Service) GetProxy(id string) (ProxyProfileView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return ProxyProfileView{}, err
	}
	proxy, ok := cfg.Proxies[id]
	if !ok {
		return ProxyProfileView{}, ErrNotFound
	}
	return proxyView(proxy), nil
}

func (s *Service) CreateProxy(proxy ProxyProfile) (ProxyProfileView, error) {
	var out ProxyProfileView
	err := s.withConfig(func(cfg *Config) error {
		if proxy.Password != "" {
			return secretFieldError("proxy password")
		}
		if proxy.ID == "" {
			proxy.ID = newID("prx")
		}
		if _, ok := cfg.Proxies[proxy.ID]; ok || aliasExistsProxy(*cfg, proxy.Alias, proxy.ID) {
			return ErrConflict
		}
		cfg.Proxies[proxy.ID] = proxy
		out = proxyView(proxy)
		return nil
	})
	return out, err
}

func (s *Service) UpdateProxy(id string, proxy ProxyProfile) (ProxyProfileView, error) {
	var out ProxyProfileView
	err := s.withConfig(func(cfg *Config) error {
		if proxy.Password != "" {
			return secretFieldError("proxy password")
		}
		current, ok := cfg.Proxies[id]
		if !ok {
			return ErrNotFound
		}
		if proxy.ID == "" {
			proxy.ID = id
		}
		if proxy.ID != id {
			return fmt.Errorf("%w: id cannot be changed", ErrValidation)
		}
		if aliasExistsProxy(*cfg, proxy.Alias, id) {
			return ErrConflict
		}
		if proxy.Password == "" {
			proxy.Password = current.Password
		}
		cfg.Proxies[id] = proxy
		out = proxyView(proxy)
		return nil
	})
	return out, err
}

func (s *Service) DeleteProxy(id string) error {
	return s.withConfig(func(cfg *Config) error {
		if _, ok := cfg.Proxies[id]; !ok {
			return ErrNotFound
		}
		delete(cfg.Proxies, id)
		return nil
	})
}

func (s *Service) ListKeys() ([]KeyMetadataView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return keyViews(cfg), nil
}

func (s *Service) GetKey(id string) (KeyMetadataView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return KeyMetadataView{}, err
	}
	key, ok := cfg.Keys[id]
	if !ok {
		return KeyMetadataView{}, ErrNotFound
	}
	return keyView(key), nil
}

func (s *Service) CreateKey(key KeyMetadata) (KeyMetadataView, error) {
	var out KeyMetadataView
	err := s.withConfig(func(cfg *Config) error {
		if key.PrivateKey != "" {
			return secretFieldError("private key")
		}
		if key.ID == "" {
			key.ID = newID("key")
		}
		if _, ok := cfg.Keys[key.ID]; ok || aliasExistsKey(*cfg, key.Alias, key.ID) {
			return ErrConflict
		}
		cfg.Keys[key.ID] = key
		out = keyView(key)
		return nil
	})
	return out, err
}

func (s *Service) UpdateKey(id string, key KeyMetadata) (KeyMetadataView, error) {
	var out KeyMetadataView
	err := s.withConfig(func(cfg *Config) error {
		if key.PrivateKey != "" {
			return secretFieldError("private key")
		}
		current, ok := cfg.Keys[id]
		if !ok {
			return ErrNotFound
		}
		if key.ID == "" {
			key.ID = id
		}
		if key.ID != id {
			return fmt.Errorf("%w: id cannot be changed", ErrValidation)
		}
		if aliasExistsKey(*cfg, key.Alias, id) {
			return ErrConflict
		}
		if key.PrivateKey == "" {
			key.PrivateKey = current.PrivateKey
		}
		cfg.Keys[id] = key
		out = keyView(key)
		return nil
	})
	return out, err
}

func (s *Service) DeleteKey(id string) error {
	return s.withConfig(func(cfg *Config) error {
		if _, ok := cfg.Keys[id]; !ok {
			return ErrNotFound
		}
		delete(cfg.Keys, id)
		return nil
	})
}

func (s *Service) ListSyncProviders() ([]SyncProviderView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return syncProviderViews(cfg), nil
}

func (s *Service) GetSyncProvider(id string) (SyncProviderView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return SyncProviderView{}, err
	}
	provider, ok := cfg.SyncProviders[id]
	if !ok {
		return SyncProviderView{}, ErrNotFound
	}
	return syncProviderView(provider, cfg.Settings.DefaultSyncProvider), nil
}

func (s *Service) CreateSyncProvider(provider SyncProviderConfig) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		if providerHasSecretInput(provider) {
			return secretFieldError("sync provider secret")
		}
		if provider.ID == "" {
			provider.ID = newID("sync")
		}
		if _, ok := cfg.SyncProviders[provider.ID]; ok || aliasExistsSync(*cfg, provider.Alias, provider.ID) {
			return ErrConflict
		}
		cfg.SyncProviders[provider.ID] = provider
		out = syncProviderView(provider, cfg.Settings.DefaultSyncProvider)
		return nil
	})
	return out, err
}

func (s *Service) UpdateSyncProvider(id string, provider SyncProviderConfig) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		if providerHasSecretInput(provider) {
			return secretFieldError("sync provider secret")
		}
		current, ok := cfg.SyncProviders[id]
		if !ok {
			return ErrNotFound
		}
		if provider.ID == "" {
			provider.ID = id
		}
		if provider.ID != id {
			return fmt.Errorf("%w: id cannot be changed", ErrValidation)
		}
		if aliasExistsSync(*cfg, provider.Alias, id) {
			return ErrConflict
		}
		keepProviderSecrets(&provider, current)
		cfg.SyncProviders[id] = provider
		out = syncProviderView(provider, cfg.Settings.DefaultSyncProvider)
		return nil
	})
	return out, err
}

func (s *Service) DeleteSyncProvider(id string) error {
	return s.withConfig(func(cfg *Config) error {
		if _, ok := cfg.SyncProviders[id]; !ok {
			return ErrNotFound
		}
		delete(cfg.SyncProviders, id)
		if cfg.Settings.DefaultSyncProvider == id {
			cfg.Settings.DefaultSyncProvider = ""
		}
		return nil
	})
}

func (s *Service) SetDefaultSyncProvider(id string) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		provider, ok := cfg.SyncProviders[id]
		if !ok {
			return ErrNotFound
		}
		cfg.Settings.DefaultSyncProvider = id
		out = syncProviderView(provider, id)
		return nil
	})
	return out, err
}

func (s *Service) ClearDefaultSyncProvider() error {
	return s.withConfig(func(cfg *Config) error {
		cfg.Settings.DefaultSyncProvider = ""
		return nil
	})
}

func (s *Service) Migration() Migration {
	legacyPath := filepath.Join(filepath.Dir(s.layout.ConfigDir), "knot", "config.toml")
	_, legacyErr := os.Stat(legacyPath)
	_, currentErr := os.Stat(s.path)
	legacyExists := legacyErr == nil
	currentExists := currentErr == nil
	return Migration{
		LegacyPath:       legacyPath,
		LegacyExists:     legacyExists,
		NeedsMigration:   legacyExists && !currentExists,
		ConflictDetected: legacyExists && currentExists,
	}
}

func (s *Service) MigrationPlan() MigrationPlan {
	migration := s.Migration()
	items := []MigrationItem{}
	switch {
	case !migration.LegacyExists:
		items = append(items, MigrationItem{Resource: "legacy_config", Action: "skip", Reason: "legacy config file was not found"})
	default:
		legacy, err := LoadLegacyConfig(migration.LegacyPath)
		if err != nil {
			items = append(items, MigrationItem{Resource: "legacy_config", Action: "error", Reason: err.Error()})
			break
		}
		current, err := s.snapshot()
		if err != nil {
			items = append(items, MigrationItem{Resource: "config", Action: "error", Reason: err.Error()})
			break
		}
		if migration.ConflictDetected {
			items = append(items, MigrationItem{Resource: "config", Action: "info", Reason: "current knot-core config already exists; skip_existing mode will keep existing resources"})
		}
		_, convertedItems, _ := ConvertLegacyConfig(legacy, current, "skip_existing")
		items = append(items, convertedItems...)
	}
	migration.ConflictDetected = migration.ConflictDetected || hasMigrationConflict(items)
	return MigrationPlan{Migration: migration, Items: items}
}

func (s *Service) ApplyMigration(mode string) (Summary, error) {
	if mode == "" {
		mode = "fail_on_conflict"
	}
	if mode != "skip_existing" && mode != "overwrite" && mode != "fail_on_conflict" {
		return Summary{}, fmt.Errorf("%w: unsupported migration mode", ErrValidation)
	}
	var out Summary
	err := s.withConfig(func(cfg *Config) error {
		migration := s.Migration()
		if !migration.LegacyExists {
			return ErrNotFound
		}
		legacy, err := LoadLegacyConfig(migration.LegacyPath)
		if err != nil {
			return err
		}
		next, items, err := ConvertLegacyConfig(legacy, *cfg, mode)
		if err != nil {
			return err
		}
		if mode == "fail_on_conflict" && hasMigrationConflict(items) {
			return fmt.Errorf("%w: migration has conflicts", ErrConflict)
		}
		if err := s.reencryptMigratedSecrets(&next); err != nil {
			return err
		}
		*cfg = next
		out = summaryFromConfig(next, s.metadataFor(next))
		return nil
	})
	return out, err
}

func (s *Service) SetServerPassword(id string, password string) (ServerProfileView, error) {
	var out ServerProfileView
	err := s.withConfig(func(cfg *Config) error {
		server, ok := cfg.Servers[id]
		if !ok {
			return ErrNotFound
		}
		encrypted, err := s.encryptSecret(password)
		if err != nil {
			return err
		}
		server.Password = encrypted
		if server.AuthMethod == "" {
			server.AuthMethod = AuthMethodPassword
		}
		cfg.Servers[id] = server
		out = serverView(server)
		return nil
	})
	return out, err
}

func (s *Service) ClearServerPassword(id string) (ServerProfileView, error) {
	var out ServerProfileView
	err := s.withConfig(func(cfg *Config) error {
		server, ok := cfg.Servers[id]
		if !ok {
			return ErrNotFound
		}
		server.Password = ""
		cfg.Servers[id] = server
		out = serverView(server)
		return nil
	})
	return out, err
}

func (s *Service) SetKeyPrivate(id string, privateKey string, sourcePath string) (KeyMetadataView, error) {
	var out KeyMetadataView
	err := s.withConfig(func(cfg *Config) error {
		key, ok := cfg.Keys[id]
		if !ok {
			return ErrNotFound
		}
		if privateKey != "" {
			encrypted, err := s.encryptSecret(privateKey)
			if err != nil {
				return err
			}
			key.PrivateKey = encrypted
		}
		key.SourcePath = sourcePath
		cfg.Keys[id] = key
		out = keyView(key)
		return nil
	})
	return out, err
}

func (s *Service) ClearKeyPrivate(id string) (KeyMetadataView, error) {
	var out KeyMetadataView
	err := s.withConfig(func(cfg *Config) error {
		key, ok := cfg.Keys[id]
		if !ok {
			return ErrNotFound
		}
		key.PrivateKey = ""
		key.SourcePath = ""
		cfg.Keys[id] = key
		out = keyView(key)
		return nil
	})
	return out, err
}

func (s *Service) SetProxyPassword(id string, password string) (ProxyProfileView, error) {
	var out ProxyProfileView
	err := s.withConfig(func(cfg *Config) error {
		proxy, ok := cfg.Proxies[id]
		if !ok {
			return ErrNotFound
		}
		encrypted, err := s.encryptSecret(password)
		if err != nil {
			return err
		}
		proxy.Password = encrypted
		cfg.Proxies[id] = proxy
		out = proxyView(proxy)
		return nil
	})
	return out, err
}

func (s *Service) ClearProxyPassword(id string) (ProxyProfileView, error) {
	var out ProxyProfileView
	err := s.withConfig(func(cfg *Config) error {
		proxy, ok := cfg.Proxies[id]
		if !ok {
			return ErrNotFound
		}
		proxy.Password = ""
		cfg.Proxies[id] = proxy
		out = proxyView(proxy)
		return nil
	})
	return out, err
}

func (s *Service) SetSyncPassword(password string) (SettingsView, error) {
	var out SettingsView
	err := s.withConfig(func(cfg *Config) error {
		encrypted, err := s.encryptSecret(password)
		if err != nil {
			return err
		}
		cfg.Settings.SyncPassword = encrypted
		out = settingsView(cfg.Settings)
		return nil
	})
	return out, err
}

func (s *Service) ClearSyncPassword() (SettingsView, error) {
	var out SettingsView
	err := s.withConfig(func(cfg *Config) error {
		cfg.Settings.SyncPassword = ""
		out = settingsView(cfg.Settings)
		return nil
	})
	return out, err
}

func (s *Service) SetSyncProviderPassword(id string, password string) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		provider, ok := cfg.SyncProviders[id]
		if !ok {
			return ErrNotFound
		}
		encrypted, err := s.encryptSecret(password)
		if err != nil {
			return err
		}
		provider.Password = encrypted
		cfg.SyncProviders[id] = provider
		out = syncProviderView(provider, cfg.Settings.DefaultSyncProvider)
		return nil
	})
	return out, err
}

func (s *Service) ClearSyncProviderPassword(id string) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		provider, ok := cfg.SyncProviders[id]
		if !ok {
			return ErrNotFound
		}
		provider.Password = ""
		cfg.SyncProviders[id] = provider
		out = syncProviderView(provider, cfg.Settings.DefaultSyncProvider)
		return nil
	})
	return out, err
}

func (s *Service) SetSyncProviderS3Credentials(id string, accessKeyID string, secretAccessKey string, sessionToken string) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		provider, ok := cfg.SyncProviders[id]
		if !ok {
			return ErrNotFound
		}
		var err error
		provider.AccessKeyID, err = s.encryptSecret(accessKeyID)
		if err != nil {
			return err
		}
		provider.SecretAccessKey, err = s.encryptSecret(secretAccessKey)
		if err != nil {
			return err
		}
		if sessionToken != "" {
			provider.SessionToken, err = s.encryptSecret(sessionToken)
			if err != nil {
				return err
			}
		} else {
			provider.SessionToken = ""
		}
		cfg.SyncProviders[id] = provider
		out = syncProviderView(provider, cfg.Settings.DefaultSyncProvider)
		return nil
	})
	return out, err
}

func (s *Service) ClearSyncProviderS3Credentials(id string) (SyncProviderView, error) {
	var out SyncProviderView
	err := s.withConfig(func(cfg *Config) error {
		provider, ok := cfg.SyncProviders[id]
		if !ok {
			return ErrNotFound
		}
		provider.AccessKeyID = ""
		provider.SecretAccessKey = ""
		provider.SessionToken = ""
		cfg.SyncProviders[id] = provider
		out = syncProviderView(provider, cfg.Settings.DefaultSyncProvider)
		return nil
	})
	return out, err
}

func (s *Service) SecretSummary() (map[string]any, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	servers := make(map[string]bool, len(cfg.Servers))
	for id, server := range cfg.Servers {
		servers[id] = server.Password != ""
	}
	proxies := make(map[string]bool, len(cfg.Proxies))
	for id, proxy := range cfg.Proxies {
		proxies[id] = proxy.Password != ""
	}
	keys := make(map[string]bool, len(cfg.Keys))
	for id, key := range cfg.Keys {
		keys[id] = key.PrivateKey != "" || key.SourcePath != ""
	}
	return map[string]any{
		"servers":           servers,
		"proxies":           proxies,
		"keys":              keys,
		"sync_providers":    syncProvidersSecretSummary(cfg),
		"sync_password_set": cfg.Settings.SyncPassword != "",
	}, nil
}

func (s *Service) snapshot() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Config
	err := fileutil.WithLock(s.path+".lock", func() error {
		s.loaded = false
		if err := s.loadLocked(); err != nil {
			return err
		}
		out = cloneConfig(s.cfg)
		return nil
	})
	if err != nil {
		return Config{}, err
	}
	return out, nil
}

func (s *Service) withConfig(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fileutil.WithLock(s.path+".lock", func() error {
		s.loaded = false
		if err := s.loadLocked(); err != nil {
			return err
		}
		next := cloneConfig(s.cfg)
		if err := fn(&next); err != nil {
			return err
		}
		result := validate(next)
		if !result.Valid {
			return fmt.Errorf("%w: %s", ErrValidation, result.Errors[0].Message)
		}
		next.UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(next); err != nil {
			return err
		}
		s.cfg = next
		s.modified = next.UpdatedAt
		s.loaded = true
		return nil
	})
}

func (s *Service) loadLocked() error {
	if s.loaded {
		return nil
	}
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		s.cfg = defaultConfig()
		s.loaded = true
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	ensureMaps(&cfg)
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = 1
	}
	s.cfg = cfg
	s.modified = cfg.UpdatedAt
	s.loaded = true
	return nil
}

func (s *Service) saveLocked(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(s.path, raw, 0o600)
}

func (s *Service) metadataFor(cfg Config) Metadata {
	return Metadata{
		SchemaVersion: cfg.SchemaVersion,
		Source:        "knot-core",
		ConfigPath:    s.path,
		Migration:     s.Migration(),
		UpdatedAt:     cfg.UpdatedAt,
	}
}

func (s *Service) encryptSecret(value string) (string, error) {
	if s.crypto == nil || !s.crypto.Available() {
		return "", errors.New("secret encryption provider is not available")
	}
	encrypted, err := s.crypto.Encrypt([]byte(value))
	if err != nil {
		return "", err
	}
	return secretPrefix + string(encrypted), nil
}

func (s *Service) decryptRuntimeSecrets(cfg *Config) error {
	var err error
	for id, server := range cfg.Servers {
		server.Password, err = s.decryptSecret(server.Password)
		if err != nil {
			return fmt.Errorf("decrypt server password %s: %w", id, err)
		}
		cfg.Servers[id] = server
	}
	for id, proxy := range cfg.Proxies {
		proxy.Password, err = s.decryptSecret(proxy.Password)
		if err != nil {
			return fmt.Errorf("decrypt proxy password %s: %w", id, err)
		}
		cfg.Proxies[id] = proxy
	}
	for id, key := range cfg.Keys {
		key.PrivateKey, err = s.decryptSecret(key.PrivateKey)
		if err != nil {
			return fmt.Errorf("decrypt private key %s: %w", id, err)
		}
		cfg.Keys[id] = key
	}
	cfg.Settings.SyncPassword, err = s.decryptSecret(cfg.Settings.SyncPassword)
	if err != nil {
		return fmt.Errorf("decrypt sync password: %w", err)
	}
	for id, provider := range cfg.SyncProviders {
		provider.Password, err = s.decryptSecret(provider.Password)
		if err != nil {
			return fmt.Errorf("decrypt sync provider password %s: %w", id, err)
		}
		provider.AccessKeyID, err = s.decryptSecret(provider.AccessKeyID)
		if err != nil {
			return fmt.Errorf("decrypt sync provider access key id %s: %w", id, err)
		}
		provider.SecretAccessKey, err = s.decryptSecret(provider.SecretAccessKey)
		if err != nil {
			return fmt.Errorf("decrypt sync provider secret access key %s: %w", id, err)
		}
		provider.SessionToken, err = s.decryptSecret(provider.SessionToken)
		if err != nil {
			return fmt.Errorf("decrypt sync provider session token %s: %w", id, err)
		}
		cfg.SyncProviders[id] = provider
	}
	return nil
}

func (s *Service) decryptSecret(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, secretPrefix) {
		return value, nil
	}
	if s.crypto == nil || !s.crypto.Available() {
		return "", errors.New("secret encryption provider is not available")
	}
	plaintext, err := s.crypto.Decrypt([]byte(strings.TrimPrefix(value, secretPrefix)))
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *Service) reencryptMigratedSecrets(cfg *Config) error {
	for id, server := range cfg.Servers {
		value, err := s.normalizeMigratedSecret(server.Password)
		if err != nil {
			return fmt.Errorf("migrate server password %s: %w", id, err)
		}
		server.Password = value
		cfg.Servers[id] = server
	}
	for id, proxy := range cfg.Proxies {
		value, err := s.normalizeMigratedSecret(proxy.Password)
		if err != nil {
			return fmt.Errorf("migrate proxy password %s: %w", id, err)
		}
		proxy.Password = value
		cfg.Proxies[id] = proxy
	}
	for id, key := range cfg.Keys {
		value, err := s.normalizeMigratedSecret(key.PrivateKey)
		if err != nil {
			return fmt.Errorf("migrate private key %s: %w", id, err)
		}
		key.PrivateKey = value
		cfg.Keys[id] = key
	}
	value, err := s.normalizeMigratedSecret(cfg.Settings.SyncPassword)
	if err != nil {
		return fmt.Errorf("migrate sync password: %w", err)
	}
	cfg.Settings.SyncPassword = value
	for id, provider := range cfg.SyncProviders {
		if provider.Password, err = s.normalizeMigratedSecret(provider.Password); err != nil {
			return fmt.Errorf("migrate sync provider password %s: %w", id, err)
		}
		if provider.AccessKeyID, err = s.normalizeMigratedSecret(provider.AccessKeyID); err != nil {
			return fmt.Errorf("migrate sync provider access key id %s: %w", id, err)
		}
		if provider.SecretAccessKey, err = s.normalizeMigratedSecret(provider.SecretAccessKey); err != nil {
			return fmt.Errorf("migrate sync provider secret access key %s: %w", id, err)
		}
		if provider.SessionToken, err = s.normalizeMigratedSecret(provider.SessionToken); err != nil {
			return fmt.Errorf("migrate sync provider session token %s: %w", id, err)
		}
		cfg.SyncProviders[id] = provider
	}
	return nil
}

func (s *Service) normalizeMigratedSecret(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, secretPrefix) {
		plaintext, err := s.decryptSecret(value)
		if err == nil {
			return s.encryptSecret(plaintext)
		}
		raw := strings.TrimPrefix(value, secretPrefix)
		plaintext, compatErr := s.decryptLegacyCiphertext(raw)
		if compatErr != nil {
			return "", compatErr
		}
		return s.encryptSecret(plaintext)
	}
	return s.encryptSecret(value)
}

func (s *Service) decryptLegacyCiphertext(value string) (string, error) {
	for _, layout := range legacyLayoutCandidates(s.layout) {
		for _, provider := range legacyProviders(layout) {
			plaintext, err := decryptLegacyEncodedValue(value, provider)
			if err == nil {
				return plaintext, nil
			}
		}
	}
	return "", fmt.Errorf("%w: legacy secret could not be decrypted with available providers", crypto.ErrDecryptionFailed)
}

func legacyLayoutCandidates(layout paths.Layout) []paths.Layout {
	var candidates []paths.Layout
	configRoot := filepath.Dir(layout.ConfigDir)
	stateRoot := filepath.Dir(layout.StateDir)
	if configRoot != "" && stateRoot != "" {
		candidates = append(candidates, paths.NewLayout(filepath.Join(configRoot, "knot"), filepath.Join(stateRoot, "knot")))
	}
	candidates = append(candidates, paths.NewLayout(layout.ConfigDir, layout.StateDir))
	return candidates
}

func legacyProviders(layout paths.Layout) []crypto.Provider {
	var providers []crypto.Provider
	if provider, err := crypto.NewDefaultProvider(layout); err == nil {
		providers = append(providers, provider)
	}
	if provider, err := crypto.NewLocalProvider(filepath.Join(layout.ConfigDir, "secret.key")); err == nil {
		providers = append(providers, provider)
	}
	return providers
}

func decryptLegacyEncodedValue(value string, provider crypto.Provider) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	plaintext, err := provider.Decrypt(decoded)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func defaultConfig() Config {
	now := time.Now().UTC()
	return Config{
		SchemaVersion: 1,
		Settings: Settings{
			ForwardAgent:          true,
			ClearScreenOnConnect:  true,
			BroadcastEscapeEnable: false,
			BroadcastEscapeChar:   "~",
			IdleTimeout:           "30m",
			KeepaliveInterval:     "20s",
			LogLevel:              "error",
			RecentLimit:           5,
		},
		Servers:       map[string]ServerProfile{},
		Proxies:       map[string]ProxyProfile{},
		Keys:          map[string]KeyMetadata{},
		SyncProviders: map[string]SyncProviderConfig{},
		UpdatedAt:     now,
	}
}

func ensureMaps(cfg *Config) {
	if cfg.Servers == nil {
		cfg.Servers = map[string]ServerProfile{}
	}
	if cfg.Proxies == nil {
		cfg.Proxies = map[string]ProxyProfile{}
	}
	if cfg.Keys == nil {
		cfg.Keys = map[string]KeyMetadata{}
	}
	if cfg.SyncProviders == nil {
		cfg.SyncProviders = map[string]SyncProviderConfig{}
	}
}

func cloneConfig(cfg Config) Config {
	clone := cfg
	clone.Servers = make(map[string]ServerProfile, len(cfg.Servers))
	for id, server := range cfg.Servers {
		server.JumpHostIDs = append([]string(nil), server.JumpHostIDs...)
		server.Tags = append([]string(nil), server.Tags...)
		clone.Servers[id] = server
	}
	clone.Proxies = make(map[string]ProxyProfile, len(cfg.Proxies))
	for id, proxy := range cfg.Proxies {
		clone.Proxies[id] = proxy
	}
	clone.Keys = make(map[string]KeyMetadata, len(cfg.Keys))
	for id, key := range cfg.Keys {
		clone.Keys[id] = key
	}
	clone.SyncProviders = make(map[string]SyncProviderConfig, len(cfg.SyncProviders))
	for id, provider := range cfg.SyncProviders {
		clone.SyncProviders[id] = provider
	}
	return clone
}

func summaryFromConfig(cfg Config, metadata Metadata) Summary {
	return Summary{
		SchemaVersion: cfg.SchemaVersion,
		Settings:      settingsView(cfg.Settings),
		Servers:       serverViews(cfg),
		Proxies:       proxyViews(cfg),
		Keys:          keyViews(cfg),
		SyncProviders: syncProviderViews(cfg),
		UpdatedAt:     cfg.UpdatedAt,
		Metadata:      metadata,
	}
}

func settingsView(settings Settings) SettingsView {
	defaults := defaultConfig().Settings
	return SettingsView{
		ForwardAgent:          setting("forward_agent", settings.ForwardAgent, defaults.ForwardAgent, "bool"),
		ClearScreenOnConnect:  setting("clear_screen_on_connect", settings.ClearScreenOnConnect, defaults.ClearScreenOnConnect, "bool"),
		BroadcastEscapeEnable: setting("broadcast_escape_enable", settings.BroadcastEscapeEnable, defaults.BroadcastEscapeEnable, "bool"),
		BroadcastEscapeChar:   setting("broadcast_escape_char", settings.BroadcastEscapeChar, defaults.BroadcastEscapeChar, "string"),
		IdleTimeout:           setting("idle_timeout", settings.IdleTimeout, defaults.IdleTimeout, "duration"),
		KeepaliveInterval:     setting("keepalive_interval", settings.KeepaliveInterval, defaults.KeepaliveInterval, "duration"),
		LogLevel:              setting("log_level", settings.LogLevel, defaults.LogLevel, "string"),
		RecentLimit:           setting("recent_limit", settings.RecentLimit, defaults.RecentLimit, "int"),
		DefaultSFTPLocalPath:  setting("default_sftp_local_path", settings.DefaultSFTPLocalPath, defaults.DefaultSFTPLocalPath, "string"),
		DefaultSyncProvider:   setting("default_sync_provider", settings.DefaultSyncProvider, defaults.DefaultSyncProvider, "string"),
		SyncPasswordSet:       settings.SyncPassword != "",
	}
}

func setting(key string, value any, fallback any, kind string) SettingValue {
	return SettingValue{Key: key, Value: value, Default: fallback, Type: kind}
}

func settingMap(view SettingsView) map[string]SettingValue {
	return map[string]SettingValue{
		"forward_agent":           view.ForwardAgent,
		"clear_screen_on_connect": view.ClearScreenOnConnect,
		"broadcast_escape_enable": view.BroadcastEscapeEnable,
		"broadcast_escape_char":   view.BroadcastEscapeChar,
		"idle_timeout":            view.IdleTimeout,
		"keepalive_interval":      view.KeepaliveInterval,
		"log_level":               view.LogLevel,
		"recent_limit":            view.RecentLimit,
		"default_sftp_local_path": view.DefaultSFTPLocalPath,
		"default_sync_provider":   view.DefaultSyncProvider,
	}
}

func setSetting(settings *Settings, key string, value any) error {
	switch key {
	case "forward_agent":
		v, ok := value.(bool)
		if !ok {
			return ErrValidation
		}
		settings.ForwardAgent = v
	case "clear_screen_on_connect":
		v, ok := value.(bool)
		if !ok {
			return ErrValidation
		}
		settings.ClearScreenOnConnect = v
	case "broadcast_escape_enable":
		v, ok := value.(bool)
		if !ok {
			return ErrValidation
		}
		settings.BroadcastEscapeEnable = v
	case "broadcast_escape_char":
		v, ok := value.(string)
		if !ok {
			return ErrValidation
		}
		settings.BroadcastEscapeChar = v
	case "idle_timeout":
		v, ok := value.(string)
		if !ok {
			return ErrValidation
		}
		settings.IdleTimeout = v
	case "keepalive_interval":
		v, ok := value.(string)
		if !ok {
			return ErrValidation
		}
		settings.KeepaliveInterval = v
	case "log_level":
		v, ok := value.(string)
		if !ok {
			return ErrValidation
		}
		settings.LogLevel = v
	case "recent_limit":
		switch v := value.(type) {
		case float64:
			settings.RecentLimit = int(v)
		case int:
			settings.RecentLimit = v
		default:
			return ErrValidation
		}
	case "default_sftp_local_path":
		v, ok := value.(string)
		if !ok {
			return ErrValidation
		}
		settings.DefaultSFTPLocalPath = v
	case "default_sync_provider":
		v, ok := value.(string)
		if !ok {
			return ErrValidation
		}
		settings.DefaultSyncProvider = v
	default:
		return ErrNotFound
	}
	return nil
}

func serverViews(cfg Config) []ServerProfileView {
	views := make([]ServerProfileView, 0, len(cfg.Servers))
	for _, server := range cfg.Servers {
		views = append(views, serverView(server))
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].Alias < views[j].Alias
	})
	return views
}

func serverView(server ServerProfile) ServerProfileView {
	return ServerProfileView{
		ID:             server.ID,
		Alias:          server.Alias,
		Host:           server.Host,
		Port:           server.Port,
		User:           server.User,
		AuthMethod:     server.AuthMethod,
		PasswordSet:    server.Password != "",
		KeyID:          server.KeyID,
		KnownHostsPath: server.KnownHostsPath,
		ProxyID:        server.ProxyID,
		JumpHostIDs:    append([]string(nil), server.JumpHostIDs...),
		Tags:           append([]string(nil), server.Tags...),
	}
}

func proxyViews(cfg Config) []ProxyProfileView {
	views := make([]ProxyProfileView, 0, len(cfg.Proxies))
	for _, proxy := range cfg.Proxies {
		views = append(views, proxyView(proxy))
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].Alias < views[j].Alias
	})
	return views
}

func proxyView(proxy ProxyProfile) ProxyProfileView {
	return ProxyProfileView{
		ID:          proxy.ID,
		Alias:       proxy.Alias,
		Type:        proxy.Type,
		Host:        proxy.Host,
		Port:        proxy.Port,
		Username:    proxy.Username,
		PasswordSet: proxy.Password != "",
	}
}

func keyViews(cfg Config) []KeyMetadataView {
	views := make([]KeyMetadataView, 0, len(cfg.Keys))
	for _, key := range cfg.Keys {
		views = append(views, keyView(key))
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].Alias < views[j].Alias
	})
	return views
}

func keyView(key KeyMetadata) KeyMetadataView {
	return KeyMetadataView{
		ID:            key.ID,
		Alias:         key.Alias,
		Type:          key.Type,
		Length:        key.Length,
		PrivateKeySet: key.PrivateKey != "",
		SourcePath:    key.SourcePath,
	}
}

func syncProviderViews(cfg Config) []SyncProviderView {
	views := make([]SyncProviderView, 0, len(cfg.SyncProviders))
	for _, provider := range cfg.SyncProviders {
		views = append(views, syncProviderView(provider, cfg.Settings.DefaultSyncProvider))
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].Alias < views[j].Alias
	})
	return views
}

func syncProviderView(provider SyncProviderConfig, defaultID string) SyncProviderView {
	return SyncProviderView{
		ID:                 provider.ID,
		Alias:              provider.Alias,
		Type:               provider.Type,
		URL:                provider.URL,
		Username:           provider.Username,
		PasswordSet:        provider.Password != "",
		Bucket:             provider.Bucket,
		Key:                provider.Key,
		Region:             provider.Region,
		Endpoint:           provider.Endpoint,
		AccessKeyIDSet:     provider.AccessKeyID != "",
		SecretAccessKeySet: provider.SecretAccessKey != "",
		SessionTokenSet:    provider.SessionToken != "",
		PathStyle:          provider.PathStyle,
		Default:            provider.ID == defaultID,
	}
}

func syncProvidersSecretSummary(cfg Config) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(cfg.SyncProviders))
	for id, provider := range cfg.SyncProviders {
		out[id] = map[string]bool{
			"password_set":          provider.Password != "",
			"access_key_id_set":     provider.AccessKeyID != "",
			"secret_access_key_set": provider.SecretAccessKey != "",
			"session_token_set":     provider.SessionToken != "",
		}
	}
	return out
}

func filterServerViews(items []ServerProfileView, opts ServerListOptions) []ServerProfileView {
	out := make([]ServerProfileView, 0, len(items))
	alias := strings.ToLower(strings.TrimSpace(opts.Alias))
	authMethod := strings.TrimSpace(opts.AuthMethod)
	proxyID := strings.TrimSpace(opts.ProxyID)
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	tags := make([]string, 0, len(opts.Tag))
	for _, tag := range opts.Tag {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	for _, item := range items {
		if alias != "" && !strings.Contains(strings.ToLower(item.Alias), alias) {
			continue
		}
		if authMethod != "" && item.AuthMethod != authMethod {
			continue
		}
		if proxyID != "" && item.ProxyID != proxyID {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Alias+" "+item.Host+" "+item.User), query) {
			continue
		}
		if len(tags) > 0 && !serverHasAllTags(item.Tags, tags) {
			continue
		}
		out = append(out, item)
	}
	switch opts.Sort {
	case "host":
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Host == out[j].Host {
				return out[i].Alias < out[j].Alias
			}
			return out[i].Host < out[j].Host
		})
	case "created", "id":
		sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	default:
		sort.SliceStable(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	}
	return out
}

func serverHasAllTags(serverTags []string, required []string) bool {
	have := map[string]bool{}
	for _, tag := range serverTags {
		have[strings.ToLower(tag)] = true
	}
	for _, tag := range required {
		if !have[tag] {
			return false
		}
	}
	return true
}

func validate(cfg Config) ValidationResult {
	var errs []ValidationError
	add := func(resource, field, message string) {
		errs = append(errs, ValidationError{Resource: resource, Field: field, Message: message})
	}
	if cfg.SchemaVersion <= 0 {
		add("config", "schema_version", "schema version must be positive")
	}
	if cfg.Settings.BroadcastEscapeChar == "" {
		add("settings", "broadcast_escape_char", "broadcast escape char is required")
	}
	if cfg.Settings.RecentLimit < 0 {
		add("settings", "recent_limit", "recent limit must not be negative")
	}
	if cfg.Settings.DefaultSyncProvider != "" {
		if _, ok := cfg.SyncProviders[cfg.Settings.DefaultSyncProvider]; !ok {
			add("settings", "default_sync_provider", "default sync provider does not exist")
		}
	}
	aliases := map[string]string{}
	for id, server := range cfg.Servers {
		resource := "servers/" + id
		if server.ID != id {
			add(resource, "id", "server id must match map key")
		}
		if msg := aliasValidationMessage("server alias", server.Alias); msg != "" {
			add(resource, "alias", msg)
		}
		if other := aliases[server.Alias]; server.Alias != "" && other != "" {
			add(resource, "alias", "server alias conflicts with "+other)
		}
		aliases[server.Alias] = id
		if !validRequiredText(server.Host) {
			add(resource, "host", "server host is required")
		}
		if server.Port <= 0 || server.Port > 65535 {
			add(resource, "port", "server port must be between 1 and 65535")
		}
		if !validRequiredText(server.User) {
			add(resource, "user", "server user is required")
		}
		if server.AuthMethod != "" && server.AuthMethod != AuthMethodPassword && server.AuthMethod != AuthMethodKey && server.AuthMethod != AuthMethodAgent {
			add(resource, "auth_method", "unsupported auth method")
		}
		if server.AuthMethod == AuthMethodKey && server.KeyID == "" {
			add(resource, "key_id", "key auth requires key_id")
		}
		if server.KeyID != "" {
			if _, ok := cfg.Keys[server.KeyID]; !ok {
				add(resource, "key_id", "referenced key does not exist")
			}
		}
		if server.ProxyID != "" {
			if _, ok := cfg.Proxies[server.ProxyID]; !ok {
				add(resource, "proxy_id", "referenced proxy does not exist")
			}
		}
		for _, jumpID := range server.JumpHostIDs {
			if jumpID == id {
				add(resource, "jump_host_ids", "jump host chain cannot include itself")
			}
			if _, ok := cfg.Servers[jumpID]; !ok {
				add(resource, "jump_host_ids", "referenced jump host does not exist")
			}
		}
	}
	for id := range cfg.Servers {
		if detectsJumpCycle(cfg.Servers, id) {
			add("servers/"+id, "jump_host_ids", "jump host chain contains a cycle")
		}
	}
	proxyAliases := map[string]string{}
	for id, proxy := range cfg.Proxies {
		resource := "proxies/" + id
		if proxy.ID != id {
			add(resource, "id", "proxy id must match map key")
		}
		if msg := aliasValidationMessage("proxy alias", proxy.Alias); msg != "" {
			add(resource, "alias", msg)
		}
		if other := proxyAliases[proxy.Alias]; proxy.Alias != "" && other != "" {
			add(resource, "alias", "proxy alias conflicts with "+other)
		}
		proxyAliases[proxy.Alias] = id
		if proxy.Type != "" && proxy.Type != ProxyTypeSOCKS5 && proxy.Type != ProxyTypeHTTP {
			add(resource, "type", "unsupported proxy type")
		}
		if !validRequiredText(proxy.Host) {
			add(resource, "host", "proxy host is required")
		}
		if proxy.Port <= 0 || proxy.Port > 65535 {
			add(resource, "port", "proxy port must be between 1 and 65535")
		}
	}
	keyAliases := map[string]string{}
	for id, key := range cfg.Keys {
		resource := "keys/" + id
		if key.ID != id {
			add(resource, "id", "key id must match map key")
		}
		if msg := aliasValidationMessage("key alias", key.Alias); msg != "" {
			add(resource, "alias", msg)
		}
		if other := keyAliases[key.Alias]; key.Alias != "" && other != "" {
			add(resource, "alias", "key alias conflicts with "+other)
		}
		keyAliases[key.Alias] = id
	}
	syncAliases := map[string]string{}
	for id, provider := range cfg.SyncProviders {
		resource := "sync_providers/" + id
		if provider.ID != id {
			add(resource, "id", "sync provider id must match map key")
		}
		if msg := aliasValidationMessage("sync provider alias", provider.Alias); msg != "" {
			add(resource, "alias", msg)
		}
		if other := syncAliases[provider.Alias]; provider.Alias != "" && other != "" {
			add(resource, "alias", "sync provider alias conflicts with "+other)
		}
		syncAliases[provider.Alias] = id
		switch provider.Type {
		case SyncProviderWebDAV:
			if !validRequiredText(provider.URL) {
				add(resource, "url", "webdav provider URL is required")
			} else if _, err := url.ParseRequestURI(provider.URL); err != nil {
				add(resource, "url", "webdav provider URL must be valid")
			}
		case SyncProviderS3:
			if provider.Bucket == "" {
				add(resource, "bucket", "s3 provider bucket is required")
			}
			if provider.Key == "" {
				add(resource, "key", "s3 provider key is required")
			}
			if provider.Region == "" {
				add(resource, "region", "s3 provider region is required")
			}
			if provider.Region == "auto" && provider.Endpoint == "" {
				add(resource, "endpoint", "s3 region auto requires endpoint")
			}
			if provider.AccessKeyID == "" && provider.SecretAccessKey != "" {
				add(resource, "access_key_id", "s3 access key id is required when secret access key is set")
			}
			if provider.SecretAccessKey == "" && provider.AccessKeyID != "" {
				add(resource, "secret_access_key", "s3 secret access key is required when access key id is set")
			}
			if provider.Endpoint != "" && !validS3Endpoint(provider.Endpoint) {
				add(resource, "endpoint", "s3 endpoint must be an origin URL without path, query, fragment, or userinfo")
			}
		default:
			add(resource, "type", "unsupported sync provider type")
		}
	}
	return ValidationResult{Valid: len(errs) == 0, Errors: errs}
}

func normalizeServer(server *ServerProfile) {
	if server.Port == 0 {
		server.Port = 22
	}
	if server.AuthMethod == "" {
		server.AuthMethod = AuthMethodAgent
	}
	server.JumpHostIDs = append([]string(nil), server.JumpHostIDs...)
	server.Tags = append([]string(nil), server.Tags...)
}

func keepProviderSecrets(next *SyncProviderConfig, current SyncProviderConfig) {
	if next.Password == "" {
		next.Password = current.Password
	}
	if next.AccessKeyID == "" {
		next.AccessKeyID = current.AccessKeyID
	}
	if next.SecretAccessKey == "" {
		next.SecretAccessKey = current.SecretAccessKey
	}
	if next.SessionToken == "" {
		next.SessionToken = current.SessionToken
	}
}

func secretFieldError(name string) error {
	return fmt.Errorf("%w: %s must be written through the Secret API", ErrValidation, name)
}

func providerHasSecretInput(provider SyncProviderConfig) bool {
	return provider.Password != "" || provider.AccessKeyID != "" || provider.SecretAccessKey != "" || provider.SessionToken != ""
}

func validAlias(value string) bool {
	if len(value) < 1 || len(value) > 255 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

func aliasValidationMessage(label string, value string) string {
	if validAlias(value) {
		return ""
	}
	if strings.TrimSpace(value) == "" {
		return label + " is required"
	}
	return label + " contains invalid characters (allowed: [A-Za-z0-9_.-])"
}

func validRequiredText(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
}

func validS3Endpoint(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/")
}

func detectsJumpCycle(servers map[string]ServerProfile, start string) bool {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, next := range servers[id].JumpHostIDs {
			if _, ok := servers[next]; ok && visit(next) {
				return true
			}
		}
		delete(visiting, id)
		visited[id] = true
		return false
	}
	return visit(start)
}

func aliasExistsServer(cfg Config, alias string, self string) bool {
	for id, server := range cfg.Servers {
		if id != self && alias != "" && server.Alias == alias {
			return true
		}
	}
	return false
}

func aliasExistsProxy(cfg Config, alias string, self string) bool {
	for id, proxy := range cfg.Proxies {
		if id != self && alias != "" && proxy.Alias == alias {
			return true
		}
	}
	return false
}

func aliasExistsKey(cfg Config, alias string, self string) bool {
	for id, key := range cfg.Keys {
		if id != self && alias != "" && key.Alias == alias {
			return true
		}
	}
	return false
}

func aliasExistsSync(cfg Config, alias string, self string) bool {
	for id, provider := range cfg.SyncProviders {
		if id != self && alias != "" && provider.Alias == alias {
			return true
		}
	}
	return false
}

func newID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
