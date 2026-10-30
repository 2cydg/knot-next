package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"knot-core/pkg/config"
)

func TestInitWritesConfigWithoutStartingServices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	var output bytes.Buffer
	if err := initializeConfig(path, &output); err != nil {
		t.Fatal(err)
	}
	var disk struct{ Service config.ServiceSettings }
	if _, err := toml.DecodeFile(path, &disk); err != nil {
		t.Fatal(err)
	}
	secret := disk.Service.Secret
	lower, upper, digit := false, false, false
	for _, ch := range secret {
		switch {
		case ch >= 'a' && ch <= 'z':
			lower = true
		case ch >= 'A' && ch <= 'Z':
			upper = true
		case ch >= '0' && ch <= '9':
			digit = true
		default:
			t.Fatal("invalid Secret character")
		}
	}
	if disk.Service.Port != 17898 || len(secret) != 32 || !lower || !upper || !digit {
		t.Fatal("invalid initial settings")
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("Secret leaked to output")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("init created service state", err)
	}
	original, _ := os.ReadFile(path)
	if err := initializeConfig(path, &output); err == nil {
		t.Fatal("init overwrote existing config")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) {
		t.Fatal("existing config changed")
	}
	other, err := randomSecret()
	if err != nil || other == secret {
		t.Fatal("Secret not randomly generated", err)
	}
}
