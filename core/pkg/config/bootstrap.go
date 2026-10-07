package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"knot-core/internal/fileutil"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"

	"github.com/BurntSushi/toml"
)

// Initialize is the intentional startup bootstrap path. Reads and previews do
// not call it. Selection and compatibility boundaries mirror legacy Knot's
// loadFromPathBootstrapLocked; all secrets are verified before committing.
func (s *Service) Initialize() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bootstrap, ok := crypto.IsBootstrapProvider(s.crypto)
	if !ok {
		s.loaded = false
		if err := s.loadLocked(); err != nil {
			return err
		}
		cfg := cloneConfig(s.cfg)
		return s.decryptRuntimeSecrets(&cfg)
	}
	return fileutil.WithLock(s.path+".lock", func() error {
		s.loaded = false
		if err := s.loadLocked(); err != nil {
			return err
		}
		if fixed := bootstrap.Persisted(); fixed != nil {
			return nil
		}
		cfg := cloneConfig(s.cfg)
		migration := false
		err := visitSecrets(&cfg, func(value string) (string, error) {
			if !strings.HasPrefix(value, secretPrefix) {
				return value, nil
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, secretPrefix))
			if err != nil {
				return "", crypto.ErrDecryptionFailed
			}
			for _, candidate := range bootstrap.Candidates() {
				plain, err := candidate.Decrypt(raw)
				if err == nil {
					migration = migration || candidate.Name() != bootstrap.Selected().Name()
					return string(plain), nil
				}
			}
			return "", fmt.Errorf("%w: existing secrets do not match available legacy key material", crypto.ErrDecryptionFailed)
		})
		if err != nil {
			return err
		}
		// Future fields may contain secrets; never rewrite a partially understood file.
		if migration && s.unknown {
			return fmt.Errorf("%w: unknown fields prevent crypto bootstrap migration", ErrValidation)
		}
		state, err := crypto.NewState(bootstrap.Selected())
		if err != nil {
			return err
		}
		stateRaw, err := crypto.MarshalState(state)
		if err != nil {
			return err
		}
		var configRaw []byte
		if migration {
			if err := visitSecrets(&cfg, func(v string) (string, error) {
				if v == "" {
					return "", nil
				}
				return s.encryptSecret(v)
			}); err != nil {
				return err
			}
			if id := cfg.Settings.DefaultSyncProvider; id != "" {
				cfg.Settings.DefaultSyncProvider = cfg.SyncProviders[id].Alias
			}
			var buf bytes.Buffer
			if err := toml.NewEncoder(&buf).Encode(diskFromConfig(cfg)); err != nil {
				return err
			}
			configRaw = buf.Bytes()
		}
		if err := commitBootstrap(s.layout, configRaw, stateRaw); err != nil {
			return err
		}
		bootstrap.MarkPersisted(bootstrap.Selected())
		s.loaded = false
		return s.loadLocked()
	})
}

type bootstrapCommit struct {
	OldConfig string `json:"old_config"`
	Config    []byte `json:"config,omitempty"`
	State     []byte `json:"state"`
}

func fileRevision(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	return contentRevision(raw), nil
}
func contentRevision(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// The recovery marker contains encrypted TOML and the encrypted probe only.
// It is published before either replacement, making forced exit replayable.
func commitBootstrap(layout paths.Layout, configRaw, stateRaw []byte) error {
	configPath := filepath.Join(layout.ConfigDir, "config.toml")
	rev, err := fileRevision(configPath)
	if err != nil {
		return err
	}
	if len(configRaw) > 0 {
		original, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		if err := fileutil.AtomicWriteFile(configPath+".bootstrap.bak", original, 0600); err != nil {
			return err
		}
	}
	marker, err := json.Marshal(bootstrapCommit{OldConfig: rev, Config: configRaw, State: stateRaw})
	if err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFile(filepath.Join(layout.ConfigDir, ".bootstrap-recovery"), marker, 0600); err != nil {
		return err
	}
	return recoverBootstrapLocked(layout)
}

// RecoverBootstrap runs under the daemon instance lock before opening crypto.
// A conflicting external edit is preserved and requires explicit recovery.
func RecoverBootstrap(layout paths.Layout) error {
	marker := filepath.Join(layout.ConfigDir, ".bootstrap-recovery")
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fileutil.WithLock(filepath.Join(layout.ConfigDir, "config.toml.lock"), func() error { return recoverBootstrapLocked(layout) })
}
func recoverBootstrapLocked(layout paths.Layout) error {
	marker := filepath.Join(layout.ConfigDir, ".bootstrap-recovery")
	raw, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	var txn bootstrapCommit
	if json.Unmarshal(raw, &txn) != nil || len(txn.State) == 0 || txn.OldConfig == "" {
		return errors.New("invalid bootstrap recovery marker")
	}
	var state crypto.State
	if json.Unmarshal(txn.State, &state) != nil || state.ProbeCiphertext == "" || state.Provider == "" {
		return errors.New("invalid staged crypto state")
	}
	statePath := filepath.Join(layout.ConfigDir, ".crypto-state")
	if existing, err := os.ReadFile(statePath); err == nil && !bytes.Equal(existing, txn.State) {
		return fmt.Errorf("%w: crypto state changed during bootstrap recovery", ErrConflict)
	}
	configPath := filepath.Join(layout.ConfigDir, "config.toml")
	rev, err := fileRevision(configPath)
	if err != nil {
		return err
	}
	if rev != txn.OldConfig && (len(txn.Config) == 0 || rev != contentRevision(txn.Config)) {
		return fmt.Errorf("%w: config changed during bootstrap recovery", ErrConflict)
	}
	if len(txn.Config) > 0 && rev != contentRevision(txn.Config) {
		if err := fileutil.AtomicWriteFile(configPath, txn.Config, 0600); err != nil {
			return err
		}
	}
	if err := fileutil.AtomicWriteFile(filepath.Join(layout.ConfigDir, ".crypto-state"), txn.State, 0600); err != nil {
		return err
	}
	return os.Remove(marker)
}
