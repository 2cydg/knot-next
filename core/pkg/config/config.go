package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"knot-core/internal/fileutil"
	"knot-core/internal/keyutil"
	"knot-core/internal/logger"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
	"knot-core/pkg/recent"

	"github.com/BurntSushi/toml"
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
	mu             sync.Mutex
	path           string
	layout         paths.Layout
	crypto         crypto.Provider
	loaded         bool
	cfg            Config
	writeFile      func(string, []byte, os.FileMode) error
	importProvider func(paths.Layout) (crypto.Provider, error)
	revision       string
	modified       time.Time
	unknown        bool
	recent         *recent.Service
}

func NewService(layout paths.Layout, provider crypto.Provider) *Service {
	return &Service{
		path:      filepath.Join(layout.ConfigDir, "config.toml"),
		layout:    layout,
		recent:    recent.New(layout, nil),
		crypto:    provider,
		writeFile: fileutil.AtomicWriteFile,
	}
}

type Config struct {
	Warnings      []ValidationError             `json:"-" toml:"-"`
	SchemaVersion int                           `json:"schema_version" toml:"-"`
	Settings      Settings                      `json:"settings" toml:"settings"`
	Servers       map[string]ServerProfile      `json:"servers" toml:"servers"`
	Proxies       map[string]ProxyProfile       `json:"proxies" toml:"proxies"`
	Keys          map[string]KeyMetadata        `json:"keys" toml:"keys"`
	SyncProviders map[string]SyncProviderConfig `json:"sync_providers" toml:"sync_providers"`
	UpdatedAt     time.Time                     `json:"updated_at" toml:"-"`
}

type Settings struct {
	ForwardAgent          bool   `json:"forward_agent" toml:"forward_agent"`
	ClearScreenOnConnect  bool   `json:"clear_screen_on_connect" toml:"clear_screen_on_connect"`
	BroadcastEscapeEnable bool   `json:"broadcast_escape_enable" toml:"broadcast_escape_enable"`
	BroadcastEscapeChar   string `json:"broadcast_escape_char" toml:"broadcast_escape_char"`
	IdleTimeout           string `json:"idle_timeout" toml:"idle_timeout"`
	KeepaliveInterval     string `json:"keepalive_interval" toml:"keepalive_interval"`
	LogLevel              string `json:"log_level" toml:"log_level"`
	RecentLimit           int    `json:"recent_limit" toml:"recent_limit"`
	DefaultSFTPLocalPath  string `json:"default_sftp_local_path,omitempty" toml:"default_sftp_local_path,omitempty"`
	DefaultSyncProvider   string `json:"default_sync_provider,omitempty" toml:"default_sync_provider,omitempty"`
	SyncPassword          string `json:"sync_password,omitempty" toml:"sync_password,omitempty"`
}

type ForwardConfig struct {
	Type       string `toml:"type"`
	LocalPort  int    `toml:"local_port"`
	RemoteAddr string `toml:"remote_addr,omitempty"`
}

type ServerProfile struct {
	ID             string          `json:"id" toml:"id"`
	Alias          string          `json:"alias" toml:"alias"`
	Host           string          `json:"host" toml:"host"`
	Port           int             `json:"port" toml:"port"`
	User           string          `json:"user" toml:"user"`
	AuthMethod     string          `json:"auth_method,omitempty" toml:"auth_method,omitempty"`
	Password       string          `json:"password,omitempty" toml:"password,omitempty"`
	KeyID          string          `json:"key_id,omitempty" toml:"key_id,omitempty"`
	KnownHostsPath string          `json:"known_hosts_path,omitempty" toml:"known_hosts_path,omitempty"`
	ProxyID        string          `json:"proxy_id,omitempty" toml:"proxy_id,omitempty"`
	JumpHostIDs    []string        `json:"jump_host_ids,omitempty" toml:"jump_host_ids,omitempty"`
	Forwards       []ForwardConfig `json:"-" toml:"forwards,omitempty"`
	Tags           []string        `json:"tags,omitempty" toml:"tags,omitempty"`
}

type ProxyProfile struct {
	ID       string `json:"id" toml:"id"`
	Alias    string `json:"alias" toml:"alias"`
	Type     string `json:"type,omitempty" toml:"type,omitempty"`
	Host     string `json:"host,omitempty" toml:"host,omitempty"`
	Port     int    `json:"port,omitempty" toml:"port,omitempty"`
	Username string `json:"username,omitempty" toml:"username,omitempty"`
	Password string `json:"password,omitempty" toml:"password,omitempty"`
}

type KeyMetadata struct {
	Passphrase  string `json:"-" toml:"-"`
	Fingerprint string `json:"fingerprint,omitempty" toml:"fingerprint,omitempty"`
	Encrypted   bool   `json:"encrypted,omitempty" toml:"encrypted,omitempty"`
	ID          string `json:"id" toml:"id"`
	Alias       string `json:"alias" toml:"alias"`
	Type        string `json:"type,omitempty" toml:"type,omitempty"`
	Length      int    `json:"length,omitempty" toml:"length,omitempty"`
	PrivateKey  string `json:"private_key,omitempty" toml:"private_key,omitempty"`
	SourcePath  string `json:"source_path,omitempty" toml:"source_path,omitempty"`
}

type SyncProviderConfig struct {
	ID              string `json:"id" toml:"id"`
	Alias           string `json:"alias" toml:"alias"`
	Type            string `json:"type" toml:"type"`
	URL             string `json:"url,omitempty" toml:"url,omitempty"`
	Username        string `json:"username,omitempty" toml:"username,omitempty"`
	Password        string `json:"password,omitempty" toml:"password,omitempty"`
	Bucket          string `json:"bucket,omitempty" toml:"bucket,omitempty"`
	Key             string `json:"key,omitempty" toml:"key,omitempty"`
	Region          string `json:"region,omitempty" toml:"region,omitempty"`
	Endpoint        string `json:"endpoint,omitempty" toml:"endpoint,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty" toml:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty" toml:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty" toml:"session_token,omitempty"`
	PathStyle       bool   `json:"path_style,omitempty" toml:"path_style,omitempty"`
}

type RuntimeConfig struct {
	Settings      Settings                      `json:"-"`
	Servers       map[string]ServerProfile      `json:"-"`
	Proxies       map[string]ProxyProfile       `json:"-"`
	Keys          map[string]KeyMetadata        `json:"-"`
	SyncProviders map[string]SyncProviderConfig `json:"-"`
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
	Warnings      []ValidationError `json:"warnings,omitempty"`
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"source"`
	ConfigPath    string            `json:"config_path"`
	Migration     Migration         `json:"migration"`
	UpdatedAt     time.Time         `json:"updated_at"`
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
	LastUsed       *time.Time `json:"last_used"`
	ID             string     `json:"id"`
	Alias          string     `json:"alias"`
	Host           string     `json:"host"`
	Port           int        `json:"port"`
	User           string     `json:"user"`
	AuthMethod     string     `json:"auth_method,omitempty"`
	PasswordSet    bool       `json:"password_set"`
	KeyID          string     `json:"key_id,omitempty"`
	KnownHostsPath string     `json:"known_hosts_path,omitempty"`
	ProxyID        string     `json:"proxy_id,omitempty"`
	JumpHostIDs    []string   `json:"jump_host_ids,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
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
	Fingerprint   string `json:"fingerprint,omitempty"`
	Encrypted     bool   `json:"encrypted"`
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
	Source           string `json:"source"`
	JSONExists       bool   `json:"json_exists"`
}

type MigrationPlan struct {
	Warnings       []ValidationError            `json:"warnings,omitempty"`
	Migration      Migration                    `json:"migration"`
	Items          []MigrationItem              `json:"items"`
	SourceRevision string                       `json:"source_revision,omitempty"`
	TargetRevision string                       `json:"target_revision,omitempty"`
	IDMappings     map[string]map[string]string `json:"id_mappings,omitempty"`
}

type MigrationItem struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
	Reason   string `json:"reason,omitempty"`
}

type MigrationApplyRequest struct {
	Mode           string `json:"mode"`
	SourcePath     string `json:"source_path,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	TargetRevision string `json:"target_revision,omitempty"`
}

func (s *Service) Summary() (Summary, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return Summary{}, err
	}
	out := summaryFromConfig(cfg, s.metadataFor(cfg))
	out.Servers = s.serverViewsWithRecent(cfg)
	return out, nil
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
	return s.serverViewsWithRecent(cfg), nil
}

func (s *Service) ListServersPage(opts ServerListOptions) (Page[ServerProfileView], error) {
	cfg, err := s.snapshot()
	if err != nil {
		return Page[ServerProfileView]{}, err
	}
	views := s.serverViewsWithRecent(cfg)
	items := filterServerViews(views, opts)
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
	return s.serverViewWithRecent(cfg, server), nil
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
		server.Password = current.Password
		server.Forwards = append([]ForwardConfig(nil), current.Forwards...)
		normalizeServer(&server)
		cfg.Servers[id] = server
		out = serverView(server)
		return nil
	})
	return out, err
}

func (s *Service) DeleteServer(id string) error {
	err := s.withConfig(func(cfg *Config) error {
		if _, ok := cfg.Servers[id]; !ok {
			return ErrNotFound
		}
		for _, server := range cfg.Servers {
			for _, jump := range server.JumpHostIDs {
				if jump == id {
					return fmt.Errorf("%w: server is referenced as a jump host", ErrConflict)
				}
			}
		}
		delete(cfg.Servers, id)
		return nil
	})

	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.loadLocked(); err != nil {
		return err
	}
	if err = s.recent.Prune(validServers(s.cfg), s.cfg.Settings.RecentLimit); err != nil {
		logger.Diagnostic(slog.LevelWarn, "recent history prune failed", "resource_id", id, "error", err)
	}
	return nil
}

func (s *Service) ResolveServer(ref string) (ServerProfileView, error) {
	cfg, err := s.snapshot()
	if err != nil {
		return ServerProfileView{}, err
	}
	server, err := ResolveRuntimeServer(RuntimeConfig{Servers: cfg.Servers}, ref)
	if err != nil {
		return ServerProfileView{}, err
	}
	return s.serverViewWithRecent(cfg, server), nil
}

func (s *Service) RuntimeConfig() (RuntimeConfig, error) {
	cfg, err := s.snapshot()
	if err != nil {
		logger.RegisterSecrets(cfg)
		return RuntimeConfig{}, err
	}
	if err := s.decryptRuntimeSecrets(&cfg); err != nil {
		logger.RegisterSecrets(cfg)
		return RuntimeConfig{}, err
	}
	for id, server := range cfg.Servers {
		if server.KnownHostsPath == "" {
			server.KnownHostsPath = filepath.Join(s.layout.ConfigDir, "known_hosts")
			cfg.Servers[id] = server
		}
	}
	logger.RegisterSecrets(cfg)
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
		for _, server := range cfg.Servers {
			if server.ProxyID == id {
				return fmt.Errorf("%w: resource is referenced by a server", ErrConflict)
			}
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
		key.Type = ""
		key.Length = 0
		key.Fingerprint = ""
		key.Encrypted = false
		if key.SourcePath != "" {
			if err := s.inspectKey(&key); err != nil {
				return err
			}
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
		if key.SourcePath == "" {
			key.SourcePath = current.SourcePath
		}
		key.Type = current.Type
		key.Length = current.Length
		key.Fingerprint = current.Fingerprint
		key.Encrypted = current.Encrypted
		if key.SourcePath != "" {
			key.PrivateKey = ""
			if err := s.inspectKey(&key); err != nil {
				return err
			}
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
		for _, server := range cfg.Servers {
			if server.KeyID == id {
				return fmt.Errorf("%w: resource is referenced by a server", ErrConflict)
			}
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
	return s.SetKeyPrivateWithPassphrase(id, privateKey, sourcePath, "")
}

func (s *Service) SetKeyPrivateWithPassphrase(id string, privateKey string, sourcePath string, passphrase string) (KeyMetadataView, error) {
	var out KeyMetadataView
	err := s.withConfig(func(cfg *Config) error {
		key, ok := cfg.Keys[id]
		if !ok {
			return ErrNotFound
		}
		if privateKey != "" && sourcePath != "" {
			return fmt.Errorf("%w: choose private_key or source_path", ErrValidation)
		}
		raw := []byte(privateKey)
		if sourcePath != "" {
			var err error
			raw, err = os.ReadFile(sourcePath)
			if err != nil {
				return fmt.Errorf("%w: private key source cannot be read", ErrValidation)
			}
		}
		if privateKey != "" {
			if _, err := keyutil.Signer(raw, passphrase); err != nil {
				return fmt.Errorf("%w: %w", ErrValidation, err)
			}
		}
		key.Type, key.Length, key.Fingerprint = "", 0, ""
		if err := inspectStoredKey(&key, raw, passphrase); err != nil {
			return err
		}
		key.PrivateKey = ""
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
		key.Type = ""
		key.Length = 0
		key.Fingerprint = ""
		key.Encrypted = false
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

func (s *Service) snapshot() (Config, error) { cfg, _, err := s.snapshotRevision(); return cfg, err }
func (s *Service) snapshotRevision() (Config, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	if err := s.loadLocked(); err != nil {
		return Config{}, "", err
	}
	return cloneConfig(s.cfg), s.revision, nil
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
		s.revision = "absent"
		s.unknown = false
		s.loaded = true
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	disk := diskFromConfig(defaultConfig())
	md, err := toml.Decode(string(raw), &disk)
	cfg := configFromDisk(disk)
	if err != nil {
		return fmt.Errorf("%w: invalid TOML configuration", ErrValidation)
	}
	s.unknown = len(md.Undecoded()) != 0
	s.revision = contentRevision(raw)
	ensureMaps(&cfg)
	if err := applyDiskDefaults(&cfg); err != nil {
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		cfg.UpdatedAt = info.ModTime().UTC()
	}

	logger.RegisterSecrets(cfg)
	s.cfg = cfg
	s.modified = cfg.UpdatedAt
	s.loaded = true
	return nil
}

func (s *Service) saveLocked(cfg Config) error {
	if s.unknown {
		return fmt.Errorf("%w: configuration contains unknown TOML fields; refusing a lossy write", ErrValidation)
	}
	verified := cloneConfig(cfg)
	if err := s.decryptRuntimeSecrets(&verified); err != nil {
		return err
	}
	// Encrypt plaintext legacy fields on the first intentional write. Existing
	// ENC values retain their exact ciphertext; ordinary upgrades do not rekey.
	if err := s.encryptPlainSecrets(&cfg); err != nil {
		return err
	}
	disk := cloneConfig(cfg)
	if id := disk.Settings.DefaultSyncProvider; id != "" {
		disk.Settings.DefaultSyncProvider = disk.SyncProviders[id].Alias
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(diskFromConfig(disk)); err != nil {
		return err
	}
	return s.writeFile(s.path, buf.Bytes(), 0o600)
}

func (s *Service) metadataFor(cfg Config) Metadata {
	return Metadata{
		Warnings:      append([]ValidationError(nil), cfg.Warnings...),
		SchemaVersion: cfg.SchemaVersion,
		Source:        "legacy-toml",
		ConfigPath:    s.path,
		Migration:     s.Migration(),
		UpdatedAt:     cfg.UpdatedAt,
	}
}

func (s *Service) encryptSecret(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if s.crypto == nil || !s.crypto.Available() {
		return "", errors.New("secret encryption provider is not available")
	}
	encrypted, err := s.crypto.Encrypt([]byte(value))
	if err != nil {
		return "", crypto.ErrEncryptionFailed
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(encrypted), nil
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
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, secretPrefix))
	if err != nil {
		return "", crypto.ErrDecryptionFailed
	}
	plaintext, err := s.crypto.Decrypt(raw)
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
	clone.Warnings = append([]ValidationError(nil), cfg.Warnings...)
	clone.Servers = make(map[string]ServerProfile, len(cfg.Servers))
	for id, server := range cfg.Servers {
		server.JumpHostIDs = append([]string(nil), server.JumpHostIDs...)
		server.Tags = append([]string(nil), server.Tags...)
		server.Forwards = append([]ForwardConfig(nil), server.Forwards...)
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
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < 0 || v >= float64(int(^uint(0)>>1)) {
				return ErrValidation
			}
			settings.RecentLimit = int(v)
		case int:
			if v < 0 {
				return ErrValidation
			}
			settings.RecentLimit = v
		default:
			return ErrValidation
		}
		if settings.RecentLimit == 0 {
			settings.RecentLimit = defaultConfig().Settings.RecentLimit
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
		if views[i].Alias == views[j].Alias {
			return views[i].ID < views[j].ID
		}
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
		Fingerprint: key.Fingerprint, Encrypted: key.Encrypted,
		ID:            key.ID,
		Alias:         key.Alias,
		Type:          key.Type,
		Length:        key.Length,
		PrivateKeySet: key.PrivateKey != "" || key.SourcePath != "",
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
	case "recent":
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].LastUsed, out[j].LastUsed
			if a == nil && b != nil {
				return false
			}
			if a != nil && b == nil {
				return true
			}
			if a != nil && b != nil && !a.Equal(*b) {
				return a.After(*b)
			}
			if out[i].Alias == out[j].Alias {
				return out[i].ID < out[j].ID
			}
			return out[i].Alias < out[j].Alias
		})
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
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Alias == out[j].Alias {
				return out[i].ID < out[j].ID
			}
			return out[i].Alias < out[j].Alias
		})
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
	for _, value := range []string{cfg.Settings.DefaultSFTPLocalPath, cfg.Settings.BroadcastEscapeChar, cfg.Settings.LogLevel} {
		if hasControl(value) {
			add("settings", "value", "settings must not contain control characters")
		}
	}
	for _, duration := range []string{cfg.Settings.IdleTimeout, cfg.Settings.KeepaliveInterval} {
		if parsed, err := time.ParseDuration(duration); err != nil || parsed < 0 {
			add("settings", "duration", "duration must be valid and nonnegative")
		}
	}
	aliases := map[string]string{}
	for id, server := range cfg.Servers {
		resource := "servers/" + id
		if hasControl(server.ID) || hasControl(server.KnownHostsPath) {
			add(resource, "id/path", "server ID and path must not contain control characters")
		}
		for _, f := range server.Forwards {
			if f.LocalPort <= 0 || f.LocalPort > 65535 || (f.Type != "L" && f.Type != "R" && f.Type != "D") || hasControl(f.RemoteAddr) {
				add(resource, "forwards", "invalid stored forwarding definition")
			}
		}
		for _, tag := range server.Tags {
			if hasControl(tag) {
				add(resource, "tags", "tags must not contain control characters")
			}
		}
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
		if hasControl(proxy.ID) || hasControl(proxy.Username) {
			add(resource, "id/username", "proxy fields must not contain control characters")
		}
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
		if hasControl(key.ID) || hasControl(key.SourcePath) {
			add(resource, "id/path", "key ID and path must not contain control characters")
		}
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
		if hasControl(provider.ID) || hasControl(provider.Username) || hasControl(provider.URL) || hasControl(provider.Bucket) || hasControl(provider.Key) || hasControl(provider.Region) || hasControl(provider.Endpoint) {
			add(resource, "value", "provider fields must not contain control characters")
		}
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
	server.Forwards = append([]ForwardConfig(nil), server.Forwards...)
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

func hasControl(value string) bool {
	for _, r := range value {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}
