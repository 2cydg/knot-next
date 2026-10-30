package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

func TestServiceSettingsDefaultsAndInvalidConfiguration(t *testing.T) {
	layout := paths.NewLayout(t.TempDir(), t.TempDir())
	defaults, err := LoadService(layout, false)
	if err != nil || defaults.Port != 17898 || defaults.Secret != "" {
		t.Fatal("invalid defaults", err)
	}
	if _, err := LoadService(layout, true); err == nil {
		t.Fatal("explicit missing configuration accepted")
	}
	for _, body := range []string{"[", "[service]\nport=0", "[service]\nport=65536", "[service]\nport='secret-not-an-int'", "[service]\nsecret='white space'"} {
		if err := os.WriteFile(layout.ConfigurationFile(), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadService(layout, true); err == nil || strings.Contains(err.Error(), "secret-not-an-int") {
			t.Fatal("invalid or unredacted startup error", err)
		}
	}
}

func TestExplicitConfigPersistsBusinessWritesAndHidesStartupSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected config.toml")
	original := "[service]\nport=17901\nsecret='startup-secret-keep-private'\nallowed_origins=['http://client.test']\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := paths.WithConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := LoadService(layout, true)
	if err != nil || startup.Port != 17901 || startup.Secret != "startup-secret-keep-private" {
		t.Fatal("startup fields not loaded", err)
	}
	service := NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
	if err := service.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateServer(ServerProfile{Alias: "test", Host: "localhost", Port: 22, User: "test", AuthMethod: AuthMethodAgent}); err != nil {
		t.Fatal(err)
	}
	summary, err := service.Summary()
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(summary)
	if strings.Contains(string(wire), startup.Secret) || summary.Metadata.ConfigPath != path {
		t.Fatal("startup Secret leaked or metadata path incorrect")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), startup.Secret) || !strings.Contains(string(raw), "[servers.") {
		t.Fatal("selected file not preserved", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("business writes used the default file")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("wrong configuration lock", err)
	}
	after, err := LoadService(layout, true)
	if err != nil || after.Secret != startup.Secret || after.Port != startup.Port || len(after.AllowedOrigins) != 1 {
		t.Fatal("business writes lost startup settings", err)
	}
}

func TestExplicitConfigBootstrapAndRecoveryUseActualFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected.toml")
	if err := os.WriteFile(path, []byte("[service]\nport=17898\nsecret='private'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := paths.WithConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := crypto.NewStaticProvider([]byte("test-key"))
	state, err := crypto.NewState(provider)
	if err != nil {
		t.Fatal(err)
	}
	stateRaw, err := crypto.MarshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	revised := []byte("[service]\nport=17902\nsecret='recovered'\n")
	if err := commitBootstrap(layout, revised, stateRaw); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(revised) {
		t.Fatal("bootstrap recovered wrong config", err)
	}
	if _, err := os.Stat(path + ".bootstrap.bak"); err != nil {
		t.Fatal("wrong backup location", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("bootstrap created default config")
	}
}

func TestLegacyConfigWritesRetainUsableServiceDefaults(t *testing.T) {
	for _, exists := range []bool{false, true} {
		layout := paths.NewLayout(t.TempDir(), t.TempDir())
		if exists {
			if err := os.WriteFile(layout.ConfigurationFile(), []byte("[settings]\nlog_level='error'\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		service := NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
		if _, err := service.CreateServer(ServerProfile{Alias: "test", Host: "localhost", Port: 22, User: "test", AuthMethod: AuthMethodAgent}); err != nil {
			t.Fatal(err)
		}
		startup, err := LoadService(layout, true)
		if err != nil || startup.Port != 17898 || startup.Secret != "" {
			t.Fatal("business save broke service defaults", err)
		}
	}
}
