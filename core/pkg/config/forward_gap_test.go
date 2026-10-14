package config

import (
	"bytes"
	"os"
	"testing"
)

func TestLegacyForwardMappingPersistenceAndBackup(t *testing.T) {
	s := newTestService(t)
	if _, err := s.CreateServer(ServerProfile{ID: "srv", Alias: "web", Host: "localhost", User: "fixture", Forwards: []ForwardConfig{{Type: "L", LocalPort: 12345, RemoteAddr: "localhost:80"}, {Type: "D", LocalPort: 12346, RemoteAddr: "legacy-unused-target"}}}); err != nil {
		t.Fatal(err)
	}
	defs, err := s.GetForwards("srv")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetForwards("srv")
	if defs[0].ID != again[0].ID || defs[0].Bind != "127.0.0.1:12345" || defs[0].Type != "local" || !defs[0].Enabled || defs[1].Target != "" {
		t.Fatal(defs)
	}
	before, _ := os.ReadFile(s.path)
	defs[0].Bind = "[::1]:12345"
	defs[1].Enabled = false
	if _, err = s.PutForwards("srv", defs); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(s.path + ".pre-forwards.bak")
	if err != nil || !bytes.Equal(backup, before) {
		t.Fatal("pre-extension backup missing")
	}
	if _, err = s.UpdateServer("srv", ServerProfile{Alias: "renamed", Host: "localhost", User: "fixture"}); err != nil {
		t.Fatal(err)
	}
	loaded, _ := s.GetForwards("srv")
	if loaded[0].ID != defs[0].ID || loaded[0].Bind != defs[0].Bind || loaded[1].Enabled {
		t.Fatal("ordinary save lost extensions")
	}
	restarted := NewService(s.layout, s.crypto)
	persisted, reloadErr := restarted.GetForwards("srv")
	if reloadErr != nil || len(persisted) != len(loaded) || persisted[0].Bind != loaded[0].Bind || persisted[1].Enabled {
		t.Fatal("fresh service lost saved definitions", persisted, reloadErr)
	}
	before, _ = os.ReadFile(s.path)
	bad := append([]ForwardDefinition(nil), defs...)
	bad[0].Target = "broken"
	if _, err = s.PutForwards("srv", bad); err == nil {
		t.Fatal("invalid definition accepted")
	}
	after, _ := os.ReadFile(s.path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed group save changed configuration")
	}
}
func TestProviderTypeSwitchClearsHiddenSecretsAtomically(t *testing.T) {
	s := newTestService(t)
	if _, err := s.CreateSyncProvider(SyncProviderConfig{ID: "p", Alias: "sync", Type: "webdav", URL: "https://example.invalid", Username: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSyncProviderPassword("p", "artificial-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSyncProvider("p", SyncProviderConfig{Alias: "sync", Type: "s3", Bucket: "bucket", Key: "config", Region: "region"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SyncProviders["p"].Password != "" {
		t.Fatal("old WebDAV secret retained")
	}
	if _, err := s.SetSyncProviderS3Credentials("p", "artificial-access", "artificial-secret", "artificial-token"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.path)
	if _, err := s.UpdateSyncProvider("p", SyncProviderConfig{Alias: "sync", Type: "webdav"}); err == nil {
		t.Fatal("invalid update accepted")
	}
	after, _ := os.ReadFile(s.path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid switch cleared old config")
	}
	if _, err := s.UpdateSyncProvider("p", SyncProviderConfig{Alias: "sync", Type: "webdav", URL: "https://example.invalid"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.RuntimeConfig()
	p := cfg.SyncProviders["p"]
	if p.AccessKeyID != "" || p.SecretAccessKey != "" || p.SessionToken != "" {
		t.Fatal("hidden S3 secrets retained")
	}
}
