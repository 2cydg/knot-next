package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

func legacyFixture(t *testing.T, providerName string) (*Service, map[string]string) {
	t.Helper()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "knot"), filepath.Join(root, "state"))
	source := filepath.Join("testdata", "legacy", providerName)
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		dir := layout.ConfigDir
		if entry.Name() == "state.json" {
			dir = layout.StateDir
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	if providerName == crypto.ProviderLinuxMachine {
		salt := make([]byte, 32)
		for i := range salt {
			salt[i] = byte(i)
		}
		key = crypto.DeriveKey("fixture-machine\x001000", salt)
	}
	provider := crypto.NewKeyProvider(providerName, key, nil)
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy", "plaintext-hashes.json"))
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	if err := json.Unmarshal(raw, &hashes); err != nil {
		t.Fatal(err)
	}
	return NewService(layout, provider), hashes
}
func treeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = contentRevision(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func assertHash(t *testing.T, value, expected string) {
	t.Helper()
	sum := sha256.Sum256([]byte(value))
	if hex.EncodeToString(sum[:]) != expected {
		t.Fatal("legacy plaintext hash mismatch")
	}
}

func TestLegacyFixtureDirectReadAndWriteback(t *testing.T) {
	for _, name := range []string{crypto.ProviderLinuxSecret, crypto.ProviderLinuxMachine} {
		t.Run(name, func(t *testing.T) {
			svc, hashes := legacyFixture(t, name)
			root := filepath.Dir(svc.layout.ConfigDir)
			before := treeHashes(t, root)
			for i := 0; i < 3; i++ {
				if svc.Migration().NeedsMigration {
					t.Fatal("direct reuse incorrectly requires migration")
				}
				plan := svc.MigrationPlan()
				if !migrationPlanHasItem(plan.Items, "config", "reuse") {
					t.Fatal("missing reuse declaration")
				}
				summary, err := svc.Summary()
				if err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(summary)
				if bytes.Contains(data, []byte("fixture-server-password")) || bytes.Contains(data, []byte("ENC:")) {
					t.Fatal("ordinary GET leaked secret")
				}
				cfg, err := svc.RuntimeConfig()
				if err != nil {
					t.Fatal(err)
				}
				assertHash(t, cfg.Servers["srv_legacy"].Password, hashes["server_password"])
				assertHash(t, cfg.Keys["key_legacy"].PrivateKey, hashes["private_key"])
				assertHash(t, cfg.Proxies["prx_legacy"].Password, hashes["proxy_password"])
				assertHash(t, cfg.Settings.SyncPassword, hashes["sync_password"])
				assertHash(t, cfg.SyncProviders["sync_web"].Password, hashes["webdav_password"])
				assertHash(t, cfg.SyncProviders["sync_s3"].AccessKeyID, hashes["access_key_id"])
				assertHash(t, cfg.SyncProviders["sync_s3"].SecretAccessKey, hashes["secret_access_key"])
				assertHash(t, cfg.SyncProviders["sync_s3"].SessionToken, hashes["session_token"])
				if cfg.Settings.DefaultSyncProvider != "sync_web" || cfg.Settings.RecentLimit != 7 || cfg.Settings.ClearScreenOnConnect || len(cfg.Servers["srv_legacy"].Forwards) != 2 {
					t.Fatal("legacy fields/defaults changed")
				}
				if cfg.Servers["srv_legacy"].KnownHostsPath != filepath.Join(svc.layout.ConfigDir, "known_hosts") {
					t.Fatal("legacy trust path not reused")
				}
				state, err := crypto.LoadState(svc.layout)
				if err != nil {
					t.Fatal(err)
				}
				if err := crypto.ValidateState(state, svc.crypto); err != nil {
					t.Fatal(err)
				}
			}
			if after := treeHashes(t, root); !reflect.DeepEqual(before, after) {
				t.Fatal("read or preview mutated installation")
			}
			old, _ := os.ReadFile(svc.path)
			if _, err := svc.UpdateSetting("log_level", "debug"); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(svc.path)
			if err != nil {
				t.Fatal(err)
			}
			var disk Config
			if _, err := toml.Decode(string(raw), &disk); err != nil {
				t.Fatal(err)
			}
			var oldDisk Config
			_, _ = toml.Decode(string(old), &oldDisk)
			if disk.Settings.DefaultSyncProvider != "cloud" || !reflect.DeepEqual(disk.Servers["srv_legacy"].Forwards, oldDisk.Servers["srv_legacy"].Forwards) || disk.Servers["srv_legacy"].Password != oldDisk.Servers["srv_legacy"].Password || disk.Keys["key_legacy"].PrivateKey != oldDisk.Keys["key_legacy"].PrivateKey {
				t.Fatal("writeback lost fields or re-encrypted unchanged secrets")
			}
			if bytes.Contains(raw, []byte("schema_version")) || bytes.Contains(raw, []byte("updated_at")) {
				t.Fatal("API metadata persisted as format requirement")
			}
			after := treeHashes(t, root)
			for path, hash := range before {
				if path != filepath.Join("knot", "config.toml") && after[path] != hash {
					t.Fatalf("writeback changed ancillary file %s", path)
				}
			}
			if path := os.Getenv("KNOT_COMPAT_WRITEBACK_PATH"); path != "" && name == crypto.ProviderLinuxSecret {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestUnknownTOMLAndJSONArePreserved(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxSecret)
	file, err := os.OpenFile(svc.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("\n[future_feature]\nvalue = 'must survive'\n")
	_ = file.Close()
	jsonPath := filepath.Join(svc.layout.ConfigDir, "config.json")
	if err := os.WriteFile(jsonPath, []byte(`{"future":"preserve"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, filepath.Dir(svc.layout.ConfigDir))
	if _, err := svc.Summary(); err != nil {
		t.Fatal(err)
	}
	if !svc.Migration().JSONExists || svc.Migration().NeedsMigration {
		t.Fatal("coexistence status incorrect")
	}
	if _, err := svc.UpdateSetting("log_level", "debug"); !errors.Is(err, ErrValidation) {
		t.Fatalf("lossy write allowed: %v", err)
	}
	if _, err := svc.PlanImport(MigrationApplyRequest{SourcePath: jsonPath}); !errors.Is(err, ErrValidation) {
		t.Fatal("JSON import not rejected")
	}
	if !reflect.DeepEqual(before, treeHashes(t, filepath.Dir(svc.layout.ConfigDir))) {
		t.Fatal("unknown data changed")
	}
}

type failingCrypto struct{ crypto.Provider }

func (p failingCrypto) Encrypt([]byte) ([]byte, error) {
	return nil, errors.New("injected encryption failure")
}
func TestConfigFailuresPreserveOriginal(t *testing.T) {
	for _, failure := range []string{"encryption", "replace", "validation", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			svc, _ := legacyFixture(t, crypto.ProviderLinuxSecret)
			before, _ := os.ReadFile(svc.path)
			var err error
			switch failure {
			case "encryption":
				svc.crypto = failingCrypto{svc.crypto}
				_, err = svc.SetServerPassword("srv_legacy", "never-persist-sentinel")
			case "replace":
				svc.writeFile = func(string, []byte, os.FileMode) error { return errors.New("injected atomic replacement failure") }
				_, err = svc.UpdateSetting("log_level", "debug")
			case "validation":
				_, err = svc.UpdateServer("srv_legacy", ServerProfile{Alias: "prod", Host: "prod.example", Port: 70000, User: "fixture"})
			case "corrupt":
				if err := os.WriteFile(svc.path, []byte("[broken"), 0600); err != nil {
					t.Fatal(err)
				}
				before = []byte("[broken")
				_, err = svc.Summary()
			}
			if err == nil {
				t.Fatal("failure accepted")
			}
			after, _ := os.ReadFile(svc.path)
			if !bytes.Equal(before, after) {
				t.Fatal("failure changed original")
			}
			if entries, _ := filepath.Glob(filepath.Join(svc.layout.ConfigDir, "*.tmp-*")); len(entries) > 0 {
				t.Fatal("temporary files leaked")
			}
		})
	}
}
func TestConcurrentTOMLUpdatesAcrossServices(t *testing.T) {
	svc := newTestService(t)
	other := NewService(svc.layout, svc.crypto)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			writer := svc
			if i%2 == 0 {
				writer = other
			}
			_, err := writer.CreateServer(ServerProfile{ID: fmt.Sprintf("srv_%d", i), Alias: fmt.Sprintf("host%d", i), Host: "example.test", Port: 22, User: "fixture"})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	servers, err := svc.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 40 {
		t.Fatal("concurrent updates lost")
	}
}
func TestReferencedDeletionAndControls(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxSecret)
	for _, err := range []error{svc.DeleteKey("key_legacy"), svc.DeleteProxy("prx_legacy"), svc.DeleteServer("srv_jump")} {
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("referenced deletion allowed: %v", err)
		}
	}
	for _, alias := range []string{"bad\nname", "bad\x00name"} {
		if _, err := svc.CreateServer(ServerProfile{Alias: alias, Host: "example.test", Port: 22, User: "fixture"}); !errors.Is(err, ErrValidation) {
			t.Fatal("control character accepted")
		}
	}
}
func TestLegacyDefaultAliasRename(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxSecret)
	cfg, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	provider := cfg.SyncProviders["sync_web"]
	provider.Password = ""
	provider.Alias = "renamed"
	if _, err := svc.UpdateSyncProvider(provider.ID, provider); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(svc.path)
	if !strings.Contains(string(raw), `default_sync_provider = "renamed"`) {
		t.Fatal("alias default not updated")
	}
	cfg, err = svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Settings.DefaultSyncProvider != "sync_web" {
		t.Fatal("ID default was not restored")
	}
}

func TestLegacyPlaintextReadIsUnchangedAndIntentionalWriteEncrypts(t *testing.T) {
	svc := newTestService(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy", "plaintext.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Summary(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RuntimeConfig(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(svc.path)
	if !bytes.Equal(raw, after) {
		t.Fatal("plaintext read rewrote config")
	}
	if _, err := svc.UpdateSetting("log_level", "info"); err != nil {
		t.Fatal(err)
	}
	encrypted, _ := os.ReadFile(svc.path)
	for _, sentinel := range []string{"fixture-server-password", "fixture-proxy-password", "fixture-sync-password", "fixture-webdav-password", "fixture-access", "fixture-secret", "fixture-token", "BEGIN OPENSSH PRIVATE KEY"} {
		if bytes.Contains(encrypted, []byte(sentinel)) {
			t.Fatal("intentional write kept plaintext secret")
		}
	}
	cfg, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["srv_legacy"].Password != "fixture-server-password" {
		t.Fatal("encrypted plaintext upgrade did not reload")
	}
}
func TestLegacyDefaultProviderUnknownAliasWarnsWithoutWriting(t *testing.T) {
	svc, _ := legacyFixture(t, crypto.ProviderLinuxSecret)
	raw, err := os.ReadFile(svc.path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(`default_sync_provider = "cloud"`), []byte(`default_sync_provider = "sync_web"`))
	if err := os.WriteFile(svc.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	summary, err := svc.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Settings.DefaultSyncProvider.Value != "" || len(summary.Metadata.Warnings) != 1 || summary.Metadata.Warnings[0].Field != "default_sync_provider" {
		t.Fatal("unknown alias did not become an observable unset default")
	}
	if _, err := svc.RuntimeConfig(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(svc.path)
	if !bytes.Equal(raw, after) {
		t.Fatal("unknown alias rewrote original")
	}
}

func TestRecentLimitKeepsLegacyZeroDefaultAndRejectsFractions(t *testing.T) {
	svc := newTestService(t)
	value, err := svc.UpdateSetting("recent_limit", float64(0))
	if err != nil {
		t.Fatal(err)
	}
	if value.Value != 5 {
		t.Fatal("zero did not normalize before returning API result")
	}
	got, err := svc.Setting("recent_limit")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != value.Value {
		t.Fatal("write/read default disagrees")
	}
	before, _ := os.ReadFile(svc.path)
	for _, invalid := range []any{float64(2.5), float64(-1), float64(1e100), -1} {
		if _, err := svc.UpdateSetting("recent_limit", invalid); !errors.Is(err, ErrValidation) {
			t.Fatal("invalid recent limit accepted")
		}
	}
	after, _ := os.ReadFile(svc.path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid recent limit changed config")
	}
}
