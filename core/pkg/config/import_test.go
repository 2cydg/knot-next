package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const importFixture = `
[settings]
default_sync_provider = "cloud"
clear_screen_on_connect = false
[proxies.source_proxy]
id = "source_proxy"
alias = "proxy"
type = "socks5"
host = "source.proxy"
port = 1080
password = "import-only-proxy-sentinel"
[keys.source_key]
id = "source_key"
alias = "deploy"
[servers.source_jump]
id = "source_jump"
alias = "jump"
host = "source.jump"
port = 22
user = "fixture"
[servers.source_server]
id = "source_server"
alias = "prod"
host = "source.host"
port = 22
user = "fixture"
key_id = "source_key"
proxy_id = "source_proxy"
jump_host_ids = ["source_jump"]
password = "import-only-password-sentinel"
[[servers.source_server.forwards]]
type = "L"
local_port = 8080
remote_addr = "127.0.0.1:80"
[sync_providers.source_sync]
id = "source_sync"
alias = "cloud"
type = "webdav"
url = "https://dav.example.test"
`

func setupImport(t *testing.T) (*Service, string) {
	t.Helper()
	svc := newTestService(t)
	if _, err := svc.CreateProxy(ProxyProfile{ID: "target_proxy", Alias: "proxy", Type: "socks5", Host: "target.proxy", Port: 1080}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateKey(KeyMetadata{ID: "target_key", Alias: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateServer(ServerProfile{ID: "target_jump", Alias: "jump", Host: "target.jump", Port: 22, User: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSyncProvider(SyncProviderConfig{ID: "target_sync", Alias: "cloud", Type: "webdav", URL: "https://target.example.test"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "extra.toml")
	if err := os.WriteFile(path, []byte(importFixture), 0600); err != nil {
		t.Fatal(err)
	}
	return svc, path
}
func TestImportPoliciesRemapReferences(t *testing.T) {
	for _, mode := range []string{"fail_on_conflict", "skip_existing", "overwrite"} {
		t.Run(mode, func(t *testing.T) {
			svc, path := setupImport(t)
			before := treeHashes(t, filepath.Dir(svc.layout.ConfigDir))
			sourceBefore := treeHashes(t, filepath.Dir(path))
			plan, err := svc.PlanImport(MigrationApplyRequest{SourcePath: path, Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, treeHashes(t, filepath.Dir(svc.layout.ConfigDir))) || !reflect.DeepEqual(sourceBefore, treeHashes(t, filepath.Dir(path))) {
				t.Fatal("preview wrote files")
			}
			for resource, sourceID := range map[string]string{"keys": "source_key", "proxies": "source_proxy", "servers": "source_jump", "sync_providers": "source_sync"} {
				if plan.IDMappings[resource][sourceID] == sourceID {
					t.Fatal("alias collision did not map ID")
				}
			}
			_, err = svc.ApplyImport(MigrationApplyRequest{SourcePath: path, Mode: mode, SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision})
			if mode == "fail_on_conflict" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("conflicting apply succeeded: %v", err)
				}
				if !reflect.DeepEqual(before, treeHashes(t, filepath.Dir(svc.layout.ConfigDir))) {
					t.Fatal("conflicting apply changed files")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := svc.RuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			server := cfg.Servers["source_server"]
			if server.KeyID != "target_key" || server.ProxyID != "target_proxy" || len(server.JumpHostIDs) != 1 || server.JumpHostIDs[0] != "target_jump" || cfg.Settings.DefaultSyncProvider != "target_sync" || len(server.Forwards) != 1 {
				t.Fatal("import reference mapping failed")
			}
			expected := "target.proxy"
			if mode == "overwrite" {
				expected = "source.proxy"
			}
			if cfg.Proxies["target_proxy"].Host != expected {
				t.Fatal("policy not honored")
			}
			raw, _ := os.ReadFile(svc.path)
			if bytes.Contains(raw, []byte("import-only-password-sentinel")) || bytes.Contains(raw, []byte("import-only-proxy-sentinel")) {
				t.Fatal("import persisted plaintext")
			}
			if !reflect.DeepEqual(sourceBefore, treeHashes(t, filepath.Dir(path))) {
				t.Fatal("apply changed source")
			}
			backup, err := os.ReadFile(svc.path + ".import.bak")
			if err != nil || contentRevision(backup) != plan.TargetRevision {
				t.Fatal("backup did not preserve exact original")
			}
			// Repeating the exact request cannot replay against a changed target.
			if _, err := svc.ApplyImport(MigrationApplyRequest{SourcePath: path, Mode: mode, SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision}); !errors.Is(err, ErrConflict) {
				t.Fatal("stale apply was accepted")
			}
		})
	}
}
func TestImportDetectsSourceAndTargetChanges(t *testing.T) {
	for _, changed := range []string{"source", "target"} {
		t.Run(changed, func(t *testing.T) {
			svc, path := setupImport(t)
			plan, err := svc.PlanImport(MigrationApplyRequest{SourcePath: path, Mode: "overwrite"})
			if err != nil {
				t.Fatal(err)
			}
			if changed == "source" {
				if err := os.WriteFile(path, []byte(importFixture+"\n# edited\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := svc.UpdateSetting("log_level", "debug"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(svc.path)
			if _, err := svc.ApplyImport(MigrationApplyRequest{SourcePath: path, Mode: "overwrite", SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision}); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale preview accepted: %v", err)
			}
			after, _ := os.ReadFile(svc.path)
			if !bytes.Equal(before, after) {
				t.Fatal("revision conflict overwrote edit")
			}
		})
	}
}
func TestImportBackupFailureKeepsTarget(t *testing.T) {
	svc, path := setupImport(t)
	plan, err := svc.PlanImport(MigrationApplyRequest{SourcePath: path, Mode: "overwrite"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(svc.path+".import.bak", 0700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(svc.path)
	if _, err := svc.ApplyImport(MigrationApplyRequest{SourcePath: path, Mode: "overwrite", SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision}); err == nil {
		t.Fatal("backup failure ignored")
	}
	after, _ := os.ReadFile(svc.path)
	if !bytes.Equal(before, after) {
		t.Fatal("backup failure changed target")
	}
}
func TestImportIdentityCollisionMatrix(t *testing.T) {
	for _, mode := range []string{"fail_on_conflict", "skip_existing", "overwrite"} {
		for _, source := range []map[string]KeyMetadata{
			{"same": {ID: "same", Alias: "same-alias"}},
			{"same": {ID: "same", Alias: "new-alias"}},
			{"different": {ID: "different", Alias: "same-alias"}},
			{"same": {ID: "same", Alias: "other-alias"}},
		} {
			target := map[string]KeyMetadata{"same": {ID: "same", Alias: "same-alias"}, "other": {ID: "other", Alias: "other-alias"}}
			out, mapping, items, err := mergeResources(source, target, mode, "keys", func(v KeyMetadata) (string, string) { return v.ID, v.Alias }, func(v KeyMetadata, id string) KeyMetadata { v.ID = id; return v })
			if err != nil {
				t.Fatal(err)
			}
			ambiguous := source["same"].Alias == "other-alias"
			if mode == "fail_on_conflict" || ambiguous {
				if !hasMigrationConflict(items) {
					t.Fatal("conflict not reported")
				}
				continue
			}
			for id, v := range source {
				targetID := mapping[id]
				if targetID != "same" {
					t.Fatal("incorrect identity map")
				}
				if mode == "overwrite" && out[targetID].Alias != v.Alias {
					t.Fatal("overwrite lost alias")
				}
				if mode == "skip_existing" && out[targetID].Alias != "same-alias" {
					t.Fatal("skip changed resource")
				}
			}
		}
	}
}
