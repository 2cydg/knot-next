package config

// AuthChoice contains only the verified, persistent authentication selection.
// Private-key passphrases remain attempt-only.
type AuthChoice struct {
	Method   string
	Password string
	KeyID    string
}

// RememberServerAuth commits a verified choice against the latest profile under
// the configuration/file lock, preserving unrelated concurrent profile edits.
func (s *Service) RememberServerAuth(id string, choice AuthChoice) error {
	return s.withConfig(func(cfg *Config) error {
		server, ok := cfg.Servers[id]
		if !ok {
			return ErrNotFound
		}
		switch choice.Method {
		case AuthMethodPassword:
			if choice.Password == "" {
				return ErrValidation
			}
			encrypted, err := s.encryptSecret(choice.Password)
			if err != nil {
				return err
			}
			server.Password, server.KeyID = encrypted, ""
		case AuthMethodKey:
			if _, ok := cfg.Keys[choice.KeyID]; !ok || choice.KeyID == "" {
				return ErrNotFound
			}
			server.KeyID, server.Password = choice.KeyID, ""
		case AuthMethodAgent:
			server.KeyID, server.Password = "", ""
		default:
			return ErrValidation
		}
		server.AuthMethod = choice.Method
		cfg.Servers[id] = server
		return nil
	})
}

// ResolveRuntimeServer matches the management resolver: exact ID first, then
// the smallest ID among legacy profiles sharing an alias.
func ResolveRuntimeServer(cfg RuntimeConfig, ref string) (ServerProfile, error) {
	if server, ok := cfg.Servers[ref]; ok {
		return server, nil
	}
	selected := ""
	for id, server := range cfg.Servers {
		if server.Alias == ref && (selected == "" || id < selected) {
			selected = id
		}
	}
	if selected == "" {
		return ServerProfile{}, ErrNotFound
	}
	return cfg.Servers[selected], nil
}
