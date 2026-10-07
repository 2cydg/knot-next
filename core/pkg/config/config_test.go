package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	return NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
}

func TestSyncProviderSecretsRoundTripAndRuntimeDecrypt(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateSyncProvider(SyncProviderConfig{
		ID:       "sync_s3",
		Alias:    "backup",
		Type:     SyncProviderS3,
		Bucket:   "bucket",
		Key:      "configs/knot.json",
		Region:   "auto",
		Endpoint: "https://s3.example.com",
	}); err != nil {
		t.Fatalf("create sync provider: %v", err)
	}
	view, err := svc.SetSyncProviderS3Credentials("sync_s3", "ak", "sk", "token")
	if err != nil {
		t.Fatalf("set s3 credentials: %v", err)
	}
	if !view.AccessKeyIDSet || !view.SecretAccessKeySet || !view.SessionTokenSet {
		t.Fatalf("secret flags not set: %+v", view)
	}
	runtimeCfg, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatalf("runtime config: %v", err)
	}
	provider := runtimeCfg.SyncProviders["sync_s3"]
	if provider.AccessKeyID != "ak" || provider.SecretAccessKey != "sk" || provider.SessionToken != "token" {
		t.Fatalf("runtime secrets not decrypted: %+v", provider)
	}
}

func TestServerListPageFiltersAndSorts(t *testing.T) {
	svc := newTestService(t)
	for _, server := range []ServerProfile{
		{ID: "srv_b", Alias: "beta", Host: "10.0.0.2", Port: 22, User: "root", AuthMethod: AuthMethodAgent, Tags: []string{"prod"}},
		{ID: "srv_a", Alias: "alpha", Host: "10.0.0.1", Port: 22, User: "root", AuthMethod: AuthMethodKey, KeyID: "key_a", Tags: []string{"prod", "db"}},
	} {
		if server.KeyID != "" {
			if _, err := svc.CreateKey(KeyMetadata{ID: server.KeyID, Alias: "deploy"}); err != nil {
				t.Fatalf("create key: %v", err)
			}
		}
		if _, err := svc.CreateServer(server); err != nil {
			t.Fatalf("create server %s: %v", server.ID, err)
		}
	}
	page, err := svc.ListServersPage(ServerListOptions{Tag: []string{"prod"}, AuthMethod: AuthMethodKey, Limit: 1})
	if err != nil {
		t.Fatalf("list servers page: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Alias != "alpha" {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestValidateDetectsJumpCycle(t *testing.T) {
	cfg := defaultConfig()
	cfg.Servers["a"] = ServerProfile{ID: "a", Alias: "a", Host: "a.example", Port: 22, User: "root", AuthMethod: AuthMethodAgent, JumpHostIDs: []string{"b"}}
	cfg.Servers["b"] = ServerProfile{ID: "b", Alias: "b", Host: "b.example", Port: 22, User: "root", AuthMethod: AuthMethodAgent, JumpHostIDs: []string{"a"}}
	result := validate(cfg)
	if result.Valid {
		t.Fatal("validate succeeded, want cycle error")
	}
}

func TestMigrationPlanAndApplyLegacyConfig(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "knot-core"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	legacyDir := filepath.Join(root, "knot")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacy := `
[settings]
default_sync_provider = "cloud"

[servers.srv_legacy]
id = "srv_legacy"
alias = "prod"
host = "prod.example"
port = 22
user = "root"
auth_method = "agent"

[sync_providers.sync_legacy]
id = "sync_legacy"
alias = "cloud"
type = "webdav"
url = "https://dav.example.com"
username = "me"
password = "ENC:` + mustLegacyEncodedSecret(t, layout, "old") + `"
`
	if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	svc := NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
	svc.importProvider = func(l paths.Layout) (crypto.Provider, error) {
		return crypto.NewLocalProvider(filepath.Join(l.ConfigDir, "secret.key"))
	}
	plan := svc.MigrationPlan()
	if len(plan.Items) == 0 || plan.Migration.ConflictDetected {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	summary, err := svc.ApplyMigration("skip_existing")
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if len(summary.Servers) != 1 || summary.Servers[0].Alias != "prod" {
		t.Fatalf("server not migrated: %+v", summary.Servers)
	}
	if len(summary.SyncProviders) != 1 || !summary.SyncProviders[0].PasswordSet || !summary.SyncProviders[0].Default {
		t.Fatalf("sync provider not migrated: %+v", summary.SyncProviders)
	}
}

func TestApplyMigrationReencryptsLegacySecretsWithCurrentProvider(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "knot-core"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	legacyLayout := paths.NewLayout(filepath.Join(root, "knot"), filepath.Join(root, "legacy-state"))
	if err := legacyLayout.Ensure(); err != nil {
		t.Fatalf("ensure legacy layout: %v", err)
	}
	legacyProvider, err := crypto.NewLocalProvider(filepath.Join(legacyLayout.ConfigDir, "secret.key"))
	if err != nil {
		t.Fatalf("new legacy provider: %v", err)
	}
	legacyCipher, err := legacyProvider.Encrypt([]byte("legacy-secret"))
	if err != nil {
		t.Fatalf("encrypt legacy secret: %v", err)
	}
	legacy := `
[servers.srv_legacy]
id = "srv_legacy"
alias = "prod"
host = "prod.example"
port = 22
user = "root"
auth_method = "password"
password = "ENC:` + base64.StdEncoding.EncodeToString(legacyCipher) + `"
`
	if err := os.WriteFile(filepath.Join(legacyLayout.ConfigDir, "config.toml"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	currentProvider := crypto.NewStaticProvider([]byte("current-key"))
	svc := NewService(layout, currentProvider)
	svc.importProvider = func(l paths.Layout) (crypto.Provider, error) { return legacyProvider, nil }
	if _, err := svc.ApplyMigration("skip_existing"); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if !bytes.Contains(raw, []byte(`password = "ENC:`)) {
		t.Fatalf("migrated config missing encrypted password: %s", raw)
	}
	if bytes.Contains(raw, []byte("legacy-secret")) {
		t.Fatalf("migrated config leaked plaintext: %s", raw)
	}
	if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString([]byte("legacy-secret")))) {
		t.Fatalf("migrated config leaked plaintext encoding: %s", raw)
	}
	runtimeCfg, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatalf("runtime config: %v", err)
	}
	if got := runtimeCfg.Servers["srv_legacy"].Password; got != "legacy-secret" {
		t.Fatalf("runtime password = %q, want legacy-secret", got)
	}
}

func TestMigrationPlanPreservesCurrentConfigConflictSignal(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "knot-core"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	legacyDir := filepath.Join(root, "knot")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"), []byte(`
[servers.srv_legacy]
id = "srv_legacy"
alias = "prod"
host = "prod.example"
port = 22
user = "root"
auth_method = "agent"
`), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	svc := NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
	if _, err := svc.CreateServer(ServerProfile{ID: "srv_current", Alias: "current", Host: "127.0.0.1", Port: 22, User: "root", AuthMethod: AuthMethodAgent}); err != nil {
		t.Fatalf("create current server: %v", err)
	}
	plan := svc.MigrationPlan()
	if !plan.Migration.ConflictDetected {
		t.Fatalf("conflict signal lost: %+v", plan)
	}
	if plan.SourceRevision == "" || plan.TargetRevision == "" {
		t.Fatal("preview missing revisions")
	}
}

func migrationPlanHasItem(items []MigrationItem, resource string, action string) bool {
	for _, item := range items {
		if item.Resource == resource && item.Action == action {
			return true
		}
	}
	return false
}

func mustLegacyEncodedSecret(t *testing.T, currentLayout paths.Layout, plaintext string) string {
	t.Helper()
	legacyLayout := paths.NewLayout(filepath.Join(filepath.Dir(currentLayout.ConfigDir), "knot"), filepath.Join(filepath.Dir(currentLayout.StateDir), "legacy-state"))
	if err := legacyLayout.Ensure(); err != nil {
		t.Fatalf("ensure legacy layout: %v", err)
	}
	provider, err := crypto.NewLocalProvider(filepath.Join(legacyLayout.ConfigDir, "secret.key"))
	if err != nil {
		t.Fatalf("new legacy provider: %v", err)
	}
	ciphertext, err := provider.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatalf("encrypt legacy secret: %v", err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext)
}
