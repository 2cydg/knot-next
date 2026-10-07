package config

// diskConfig preserves legacy TOML's root without API schema/timestamp fields.
// Resource values are shared domain types; public responses use secret-free views.
type diskConfig struct {
	Settings      Settings                      `toml:"settings"`
	Servers       map[string]ServerProfile      `toml:"servers"`
	Proxies       map[string]ProxyProfile       `toml:"proxies"`
	Keys          map[string]KeyMetadata        `toml:"keys"`
	SyncProviders map[string]SyncProviderConfig `toml:"sync_providers"`
}

func diskFromConfig(cfg Config) diskConfig {
	return diskConfig{Settings: cfg.Settings, Servers: cfg.Servers, Proxies: cfg.Proxies, Keys: cfg.Keys, SyncProviders: cfg.SyncProviders}
}
func configFromDisk(disk diskConfig) Config {
	return Config{SchemaVersion: 1, Settings: disk.Settings, Servers: disk.Servers, Proxies: disk.Proxies, Keys: disk.Keys, SyncProviders: disk.SyncProviders}
}

// Disk defaults mirror knot/pkg/config.loadRawConfig at e0b4d51.
func applyDiskDefaults(cfg *Config) error {
	defaults := defaultConfig().Settings
	if cfg.Settings.IdleTimeout == "" {
		cfg.Settings.IdleTimeout = defaults.IdleTimeout
	}
	if cfg.Settings.KeepaliveInterval == "" {
		cfg.Settings.KeepaliveInterval = defaults.KeepaliveInterval
	}
	if cfg.Settings.LogLevel == "" {
		cfg.Settings.LogLevel = defaults.LogLevel
	}
	if cfg.Settings.RecentLimit <= 0 {
		cfg.Settings.RecentLimit = 5
	}
	if cfg.Settings.BroadcastEscapeChar == "" {
		cfg.Settings.BroadcastEscapeChar = "~"
	}
	if alias := cfg.Settings.DefaultSyncProvider; alias != "" {
		found := false
		for id, provider := range cfg.SyncProviders {
			if provider.Alias == alias {
				cfg.Settings.DefaultSyncProvider = id
				found = true
				break
			}
		}
		if !found {
			cfg.Settings.DefaultSyncProvider = ""
			cfg.Warnings = append(cfg.Warnings, ValidationError{Resource: "settings", Field: "default_sync_provider", Message: "stored alias does not exist; default sync provider is unset"})
		}
	}
	return nil
}

func (s *Service) encryptPlainSecrets(cfg *Config) error {
	return visitSecrets(cfg, func(value string) (string, error) {
		if value == "" || len(value) >= 4 && value[:4] == secretPrefix {
			return value, nil
		}
		return s.encryptSecret(value)
	})
}

func visitSecrets(cfg *Config, fn func(string) (string, error)) error {
	var err error
	if cfg.Settings.SyncPassword, err = fn(cfg.Settings.SyncPassword); err != nil {
		return err
	}
	for id, v := range cfg.Servers {
		if v.Password, err = fn(v.Password); err != nil {
			return err
		}
		cfg.Servers[id] = v
	}
	for id, v := range cfg.Proxies {
		if v.Password, err = fn(v.Password); err != nil {
			return err
		}
		cfg.Proxies[id] = v
	}
	for id, v := range cfg.Keys {
		if v.PrivateKey, err = fn(v.PrivateKey); err != nil {
			return err
		}
		cfg.Keys[id] = v
	}
	for id, v := range cfg.SyncProviders {
		for _, field := range []*string{&v.Password, &v.AccessKeyID, &v.SecretAccessKey, &v.SessionToken} {
			if *field, err = fn(*field); err != nil {
				return err
			}
		}
		cfg.SyncProviders[id] = v
	}
	return nil
}
