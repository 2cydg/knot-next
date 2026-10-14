package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidForwardBindsHaveFieldDetailsAndDoNotPersist(t *testing.T) {
	s := newTestService(t)
	_, err := s.CreateServer(ServerProfile{ID: "srv", Alias: "fixture", Host: "localhost", User: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.path)
	for _, bind := range []string{"[::1::2]:8080", "[not-an-ip]:8080", "[gg:::]:80"} {
		_, err = s.PutForwards("srv", []ForwardDefinition{{Type: "dynamic", Bind: bind}})
		var failure *ValidationFailure
		if !errors.As(err, &failure) || len(failure.Errors) != 1 || failure.Errors[0].Field != "forwards[0].bind" {
			t.Fatal(bind, err)
		}
		after, _ := os.ReadFile(s.path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid bind persisted")
		}
	}
	for _, bind := range []string{"127.0.0.1:0", "[::1]:8080", "localhost:8080", "[fe80::1%eth0]:8080"} {
		if err = ValidateForward(ForwardDefinition{Type: "dynamic", Bind: bind}); err != nil {
			t.Fatal(bind, err)
		}
	}
}

func TestStoredHostnameBindCanBeReadAndExplicitlyMigrated(t *testing.T) {
	s := newTestService(t)
	raw := []byte(`[servers.srv]
id = "srv"
alias = "fixture"
host = "127.0.0.1"
port = 22
user = "fixture"
[[servers.srv.forwards]]
type = "D"
bind = "myhost:8080"
`)
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	defs, err := s.GetForwards("srv")
	if err != nil || len(defs) != 1 || defs[0].Bind != "myhost:8080" {
		t.Fatal("read-only legacy bind was rewritten", defs, err)
	}
	if _, err := s.UpdateServer("srv", ServerProfile{Alias: "renamed", Host: "127.0.0.1", User: "fixture"}); !errors.Is(err, ErrValidation) {
		t.Fatal("hostname bind escaped final config validation", err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("failed write changed legacy configuration", err)
	}
	defs[0].Bind = "127.0.0.1:8080"
	if _, err := s.PutForwards("srv", defs); err != nil {
		t.Fatal("explicit bind migration failed", err)
	}
	if _, err := s.UpdateServer("srv", ServerProfile{Alias: "renamed", Host: "127.0.0.1", User: "fixture"}); err != nil {
		t.Fatal("migrated config still refuses writes", err)
	}
}
func TestTOMLPathImportBecomesBoundedInlineOriginalBytes(t *testing.T) {
	s := newTestService(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.ReplaceAll(pem.EncodeToMemory(block), []byte("\n"), []byte("\r\n"))
	keyPath := filepath.Join(t.TempDir(), "source.key")
	if err = os.WriteFile(keyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "import.toml")
	if err = os.WriteFile(cfgPath, []byte(fmt.Sprintf("[keys.path_key]\nid='path_key'\nalias='path'\nsource_path=%q\n", keyPath)), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanImport(MigrationApplyRequest{SourcePath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyImport(MigrationApplyRequest{SourcePath: cfgPath, SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keys["path_key"].SourcePath != "" || !bytes.Equal([]byte(cfg.Keys["path_key"].PrivateKey), raw) {
		t.Fatal("import kept live reference or rewrote bytes")
	}
	persisted, _ := os.ReadFile(s.path)
	if bytes.Contains(persisted, raw) || !bytes.Contains(persisted, []byte("ENC:")) {
		t.Fatal("material not encrypted")
	}
	if err = os.WriteFile(keyPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlanImport(MigrationApplyRequest{SourcePath: cfgPath}); !errors.Is(err, ErrValidation) {
		t.Fatal("empty path material accepted", err)
	}
	if err = os.WriteFile(keyPath, bytes.Repeat([]byte("x"), maxKeyBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlanImport(MigrationApplyRequest{SourcePath: cfgPath}); !errors.Is(err, ErrValidation) {
		t.Fatal("unbounded key import", err)
	}
}
