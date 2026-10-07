package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"knot-core/internal/fileutil"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"

	"github.com/BurntSushi/toml"
)

func (s *Service) Migration() Migration {
	legacyPath := filepath.Join(filepath.Dir(s.layout.ConfigDir), "knot", "config.toml")
	_, err := os.Stat(legacyPath)
	_, jsonErr := os.Stat(filepath.Join(s.layout.ConfigDir, "config.json"))
	if errors.Is(jsonErr, os.ErrNotExist) {
		_, jsonErr = os.Stat(filepath.Join(filepath.Dir(s.layout.ConfigDir), "knot-core", "config.json"))
	}
	same := samePath(legacyPath, s.path)
	return Migration{LegacyPath: legacyPath, LegacyExists: err == nil, NeedsMigration: !same && err == nil && !fileExists(s.path),
		ConflictDetected: !same && err == nil && fileExists(s.path), Source: "legacy-toml", JSONExists: jsonErr == nil}
}
func (s *Service) MigrationPlan() MigrationPlan {
	m := s.Migration()
	if samePath(m.LegacyPath, s.path) {
		var warnings []ValidationError
		items := []MigrationItem{{Resource: "config", Action: "reuse", Reason: "legacy TOML is the active configuration; apply is unnecessary"},
			{Resource: "forwards", Action: "preserve", Reason: "stored forwarding definitions are preserved; engine remains planned"},
			{Resource: "known_hosts/state.json", Action: "reuse", Reason: "trust and recent history remain at their original paths"}}
		if m.JSONExists {
			items = append(items, MigrationItem{Resource: "config.json", Action: "unsupported", Reason: "JSON import is not supported; TOML remains authoritative and JSON is preserved"})
		}
		if cfg, err := s.snapshot(); err != nil {
			items = append(items, MigrationItem{Resource: "config", Action: "error", Reason: "active TOML cannot be read"})
		} else {
			warnings = append(warnings, cfg.Warnings...)
			items = append(items, s.recentCompatibility(cfg))
		}
		return MigrationPlan{Migration: m, Items: items, Warnings: warnings}
	}
	plan, err := s.PlanImport(MigrationApplyRequest{SourcePath: m.LegacyPath})
	if err != nil {
		return MigrationPlan{Migration: m, Items: []MigrationItem{{Resource: "config", Action: "error", Reason: "import source is unavailable or invalid"}}}
	}
	if m.ConflictDetected {
		plan.Items = append(plan.Items, MigrationItem{Resource: "config", Action: "info", Reason: "target configuration exists; import uses the requested conflict policy"})
	}
	plan.Migration = m
	return plan
}
func (s *Service) ApplyMigration(mode string) (Summary, error) {
	if mode != "" && mode != "fail_on_conflict" && mode != "skip_existing" && mode != "overwrite" {
		return Summary{}, fmt.Errorf("%w: invalid import mode", ErrValidation)
	}
	m := s.Migration()
	if samePath(m.LegacyPath, s.path) {
		return s.Summary()
	}
	req := MigrationApplyRequest{Mode: mode, SourcePath: m.LegacyPath}
	plan, err := s.PlanImport(req)
	if err != nil {
		return Summary{}, err
	}
	req.SourceRevision = plan.SourceRevision
	req.TargetRevision = plan.TargetRevision
	return s.ApplyImport(req)
}
func (s *Service) PlanImport(req MigrationApplyRequest) (MigrationPlan, error) {
	if err := validateImportRequest(&req, false); err != nil {
		return MigrationPlan{}, err
	}
	current, targetRev, err := s.snapshotRevision()
	if err != nil {
		return MigrationPlan{}, err
	}
	source, sourceRev, settings, err := s.readImportSource(req.SourcePath)
	if err != nil {
		return MigrationPlan{}, err
	}
	_, items, mappings, err := mergeImport(source, current, req.Mode, settings)
	if err != nil {
		return MigrationPlan{}, err
	}
	warnings := append([]ValidationError(nil), current.Warnings...)
	warnings = append(warnings, source.Warnings...)
	return MigrationPlan{Migration: Migration{LegacyPath: req.SourcePath, LegacyExists: true, NeedsMigration: true, ConflictDetected: hasMigrationConflict(items), Source: "explicit-toml-import"}, Items: items, SourceRevision: sourceRev, TargetRevision: targetRev, IDMappings: mappings, Warnings: warnings}, nil
}
func (s *Service) ApplyImport(req MigrationApplyRequest) (Summary, error) {
	if err := validateImportRequest(&req, true); err != nil {
		return Summary{}, err
	}
	if samePath(req.SourcePath, s.path) {
		return Summary{}, fmt.Errorf("%w: active TOML already reused directly", ErrConflict)
	}
	err := s.withConfig(func(current *Config) error {
		rev, err := fileRevision(s.path)
		if err != nil {
			return err
		}
		if rev != req.TargetRevision {
			return fmt.Errorf("%w: target revision changed", ErrConflict)
		}
		source, sourceRev, settings, err := s.readImportSource(req.SourcePath)
		if err != nil {
			return err
		}
		if sourceRev != req.SourceRevision {
			return fmt.Errorf("%w: source revision changed", ErrConflict)
		}
		next, items, _, err := mergeImport(source, *current, req.Mode, settings)
		if err != nil {
			return err
		}
		if hasMigrationConflict(items) {
			return fmt.Errorf("%w: import has unresolved conflicts", ErrConflict)
		}
		if result := validate(next); !result.Valid {
			return fmt.Errorf("%w: merged configuration is invalid", ErrValidation)
		}
		if s.unknown {
			return fmt.Errorf("%w: unknown target fields prevent import", ErrValidation)
		}
		// Finish encryption before creating the backup or touching the target.
		if err := s.encryptPlainSecrets(&next); err != nil {
			return err
		}
		if rev, err := fileRevision(req.SourcePath); err != nil || rev != req.SourceRevision {
			return fmt.Errorf("%w: source revision changed", ErrConflict)
		}
		if original, err := os.ReadFile(s.path); err == nil {
			if err := fileutil.AtomicWriteFile(s.path+".import.bak", original, 0600); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		*current = next
		return nil
	})
	if err != nil {
		return Summary{}, err
	}
	return s.Summary()
}
func validateImportRequest(req *MigrationApplyRequest, apply bool) error {
	if req.SourcePath == "" {
		return fmt.Errorf("%w: source_path is required", ErrValidation)
	}
	if strings.ToLower(filepath.Ext(req.SourcePath)) != ".toml" {
		return fmt.Errorf("%w: only TOML import is supported; source was preserved", ErrValidation)
	}
	if req.Mode == "" {
		req.Mode = "fail_on_conflict"
	}
	if req.Mode != "fail_on_conflict" && req.Mode != "skip_existing" && req.Mode != "overwrite" {
		return fmt.Errorf("%w: invalid import mode", ErrValidation)
	}
	if apply && (req.SourceRevision == "" || req.TargetRevision == "") {
		return fmt.Errorf("%w: source_revision and target_revision from preview are required", ErrValidation)
	}
	return nil
}
func (s *Service) readImportSource(path string) (Config, string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, "", false, err
	}
	disk := diskFromConfig(defaultConfig())
	md, err := toml.Decode(string(raw), &disk)
	cfg := configFromDisk(disk)
	if err != nil {
		return Config{}, "", false, fmt.Errorf("%w: invalid source TOML", ErrValidation)
	}
	if len(md.Undecoded()) > 0 {
		return Config{}, "", false, fmt.Errorf("%w: source contains unknown fields", ErrValidation)
	}
	if err := applyDiskDefaults(&cfg); err != nil {
		return Config{}, "", false, err
	}
	ensureMaps(&cfg)
	if result := validate(cfg); !result.Valid {
		return Config{}, "", false, fmt.Errorf("%w: source configuration is invalid", ErrValidation)
	}
	// Open provider only if encrypted fields exist; plaintext import needs no material.
	encrypted := false
	_ = visitSecrets(&cfg, func(v string) (string, error) {
		encrypted = encrypted || strings.HasPrefix(v, secretPrefix)
		return v, nil
	})
	sourceSvc := NewService(paths.NewLayout(filepath.Dir(path), s.layout.StateDir), nil)
	if encrypted {
		open := crypto.OpenExistingProvider
		if s.importProvider != nil {
			open = s.importProvider
		}
		sourceSvc.crypto, err = open(sourceSvc.layout)
		if err != nil {
			return Config{}, "", false, errors.New("source encryption material unavailable; no changes written")
		}
		if bootstrap, ok := crypto.IsBootstrapProvider(sourceSvc.crypto); ok {
			err = visitSecrets(&cfg, func(v string) (string, error) {
				if !strings.HasPrefix(v, secretPrefix) {
					return v, nil
				}
				for _, candidate := range bootstrap.Candidates() {
					sourceSvc.crypto = candidate
					if plain, err := sourceSvc.decryptSecret(v); err == nil {
						return plain, nil
					}
				}
				return "", crypto.ErrDecryptionFailed
			})
		} else {
			err = sourceSvc.decryptRuntimeSecrets(&cfg)
		}
		if err != nil {
			return Config{}, "", false, errors.New("source secrets cannot be decrypted; no changes written")
		}
	}
	for id, key := range cfg.Keys {
		var material []byte
		if key.PrivateKey != "" {
			material = []byte(key.PrivateKey)
		} else if key.SourcePath != "" {
			material, err = os.ReadFile(key.SourcePath)
			if err != nil {
				return Config{}, "", false, fmt.Errorf("%w: source private key path cannot be read", ErrValidation)
			}
		}
		if len(material) > 0 {
			if err := inspectStoredKey(&key, material, ""); err != nil {
				return Config{}, "", false, fmt.Errorf("%w: source contains an invalid private key", ErrValidation)
			}
			cfg.Keys[id] = key
		}
	}
	return cfg, contentRevision(raw), md.IsDefined("settings"), nil
}

// Resource merging follows the legacy archive/configsync identity policy:
// alias matches reuse the destination ID and references use the resulting maps.
func mergeResources[T any](source, target map[string]T, mode, resource string, identity func(T) (string, string), setID func(T, string) T) (map[string]T, map[string]string, []MigrationItem, error) {
	out := make(map[string]T, len(target)+len(source))
	aliases := map[string]string{}
	for id, v := range target {
		out[id] = v
		_, alias := identity(v)
		aliases[alias] = id
	}
	mapping := map[string]string{}
	items := []MigrationItem{}
	ids := make([]string, 0, len(source))
	for id := range source {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v := source[id]
		_, alias := identity(v)
		targetID := id
		action := "create"
		_, idExists := target[id]
		aliasID := aliases[alias]
		if idExists && aliasID != "" && aliasID != id {
			items = append(items, MigrationItem{Resource: resource + "/" + id, Action: "conflict", Reason: "ID and alias identify different target resources"})
			continue
		}
		if idExists || aliasID != "" {
			if aliasID != "" {
				targetID = aliasID
			}
			switch mode {
			case "overwrite":
				action = "overwrite"
			case "skip_existing":
				action = "skip"
			default:
				action = "conflict"
			}
		}
		mapping[id] = targetID
		if action == "create" || action == "overwrite" {
			out[targetID] = setID(v, targetID)
		}
		items = append(items, MigrationItem{Resource: resource + "/" + id, Action: action})
	}
	return out, mapping, items, nil
}
func mergeImport(source, current Config, mode string, settings bool) (Config, []MigrationItem, map[string]map[string]string, error) {
	next := cloneConfig(current)
	items := []MigrationItem{}
	maps := map[string]map[string]string{}
	var part []MigrationItem
	var err error
	next.Proxies, maps["proxies"], part, err = mergeResources(source.Proxies, current.Proxies, mode, "proxies", func(v ProxyProfile) (string, string) { return v.ID, v.Alias }, func(v ProxyProfile, id string) ProxyProfile { v.ID = id; return v })
	items = append(items, part...)
	if err != nil {
		return next, items, maps, err
	}
	next.Keys, maps["keys"], part, err = mergeResources(source.Keys, current.Keys, mode, "keys", func(v KeyMetadata) (string, string) { return v.ID, v.Alias }, func(v KeyMetadata, id string) KeyMetadata { v.ID = id; return v })
	items = append(items, part...)
	if err != nil {
		return next, items, maps, err
	}
	next.SyncProviders, maps["sync_providers"], part, err = mergeResources(source.SyncProviders, current.SyncProviders, mode, "sync_providers", func(v SyncProviderConfig) (string, string) { return v.ID, v.Alias }, func(v SyncProviderConfig, id string) SyncProviderConfig { v.ID = id; return v })
	items = append(items, part...)
	if err != nil {
		return next, items, maps, err
	}
	next.Servers, maps["servers"], part, err = mergeResources(source.Servers, current.Servers, mode, "servers", func(v ServerProfile) (string, string) { return v.ID, v.Alias }, func(v ServerProfile, id string) ServerProfile { v.ID = id; return v })
	items = append(items, part...)
	if err != nil {
		return next, items, maps, err
	}
	for sourceID, v := range source.Servers {
		targetID := maps["servers"][sourceID]
		if targetID == "" {
			continue
		}
		if mode == "skip_existing" {
			if _, exists := current.Servers[targetID]; exists {
				continue
			}
		}
		v.JumpHostIDs = append([]string(nil), v.JumpHostIDs...)
		remapImportedServerRefs(&v, maps["keys"], maps["proxies"], maps["servers"])

		v.ID = targetID
		next.Servers[targetID] = v
	}
	if settings {
		action := migrationAction("settings", "", "", hasCurrentSettings(current.Settings), mode)
		items = append(items, MigrationItem{Resource: "settings", Action: action})
		if action == "create" || action == "overwrite" {
			next.Settings = source.Settings
			if id := source.Settings.DefaultSyncProvider; id != "" {
				next.Settings.DefaultSyncProvider = maps["sync_providers"][id]
			}
		}
	}
	// Overwriting a provider alias must update the target's default ID semantics.
	if !hasMigrationConflict(items) && !validate(next).Valid {
		return next, items, maps, fmt.Errorf("%w: merged references or aliases are invalid", ErrConflict)
	}
	return next, items, maps, nil
}
func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	if aa == bb {
		return true
	}
	ai, ae := os.Stat(a)
	bi, be := os.Stat(b)
	return ae == nil && be == nil && os.SameFile(ai, bi)
}
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func (s *Service) recentCompatibility(cfg Config) MigrationItem {
	item := MigrationItem{Resource: "state.json", Action: "preserve", Reason: "recent history remains unchanged; usage callbacks are implemented in the client-data phase"}
	raw, err := os.ReadFile(filepath.Join(s.layout.StateDir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return item
	}
	if err != nil {
		item.Action = "error"
		item.Reason = "recent history cannot be read"
		return item
	}
	var state struct {
		Recent []struct {
			ServerID string    `json:"server_id"`
			LastUsed time.Time `json:"last_used"`
		} `json:"recent"`
	}
	if json.Unmarshal(raw, &state) != nil {
		item.Action = "error"
		item.Reason = "recent history has an invalid format"
		return item
	}
	unknown := 0
	for _, entry := range state.Recent {
		if _, ok := cfg.Servers[entry.ServerID]; !ok {
			unknown++
		}
	}
	if unknown > 0 {
		item.Reason = fmt.Sprintf("recent history preserved; %d entries refer to deleted or unknown server IDs and must not resolve by alias", unknown)
	}
	return item
}

// Adapted directly from knot/pkg/configsync/merge.go.remapServerRefs,
// source e0b4d51eea6647e192371059381039b99fb301a2.
func remapImportedServerRefs(server *ServerProfile, keyIDMap, proxyIDMap, serverIDMap map[string]string) {
	if server.KeyID != "" {
		if newID, ok := keyIDMap[server.KeyID]; ok {
			server.KeyID = newID
		}
	}
	if server.ProxyID != "" {
		if newID, ok := proxyIDMap[server.ProxyID]; ok {
			server.ProxyID = newID
		}
	}
	for i, id := range server.JumpHostIDs {
		if newID, ok := serverIDMap[id]; ok {
			server.JumpHostIDs[i] = newID
		}
	}
}
