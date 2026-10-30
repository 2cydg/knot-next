package crypto

import (
	"os"
	"path/filepath"
	"testing"

	"knot-core/internal/paths"
)

func TestSaltInitializationChecksSelectedConfigFile(t *testing.T) {
	dir := t.TempDir()
	selected := filepath.Join(dir, "selected.toml")
	if err := os.WriteFile(selected, []byte("password='ENC:existing-ciphertext'"), 0600); err != nil {
		t.Fatal(err)
	}
	layout := paths.NewLayout(dir, t.TempDir())
	layout.ConfigFile = selected
	if allowSaltInitialization(layout, true) {
		t.Fatal("salt initialization ignored encrypted selected configuration")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("password=''"), 0600); err != nil {
		t.Fatal(err)
	}
	if allowSaltInitialization(layout, true) {
		t.Fatal("default config overrode selected file")
	}
	if err := os.WriteFile(selected, []byte("password=''"), 0600); err != nil {
		t.Fatal(err)
	}
	if !allowSaltInitialization(layout, true) {
		t.Fatal("new selected config cannot initialize salt")
	}
}
