//go:build linux

package crypto

import (
	"bytes"
	"errors"
	"knot-core/internal/paths"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// These boundary regressions are adapted from knot/pkg/crypto/linux_test.go,
// e0b4d51: the machine-only v1 key is intentionally excluded.
func TestLegacyLinuxKeyBoundaries(t *testing.T) {
	salt := bytes.Repeat([]byte{7}, 32)
	machine := "fixture-machine"
	provider := newKeyProvider(ProviderLinuxMachine, DeriveKey(linuxFallbackKeyMaterial(machine), salt))
	weak, err := EncryptWithKey([]byte("fixture"), DeriveKey(machine, salt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Decrypt(weak); err == nil {
		t.Fatal("weak machine-only derivation accepted")
	}
	ss := newKeyProvider(ProviderLinuxSecret, bytes.Repeat([]byte{9}, 32))
	ciphertext, err := ss.Encrypt([]byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Decrypt(ciphertext); err == nil {
		t.Fatal("pinned Secret Service key decrypted using machine fallback")
	}
	if !reflect.DeepEqual(ssItemAttributes, map[string]string{"service": "knot", "account": "knot-master-key"}) {
		t.Fatal("legacy credential identity changed")
	}
}
func TestExistingLinuxMaterialsRemainReadOnly(t *testing.T) {
	oldRead, oldCreate, oldMachine, oldUID := getSecretServiceKeyFunc, getOrCreateSecretServiceKeyFunc, machineIDFunc, uidFunc
	defer func() {
		getSecretServiceKeyFunc = oldRead
		getOrCreateSecretServiceKeyFunc = oldCreate
		machineIDFunc = oldMachine
		uidFunc = oldUID
	}()
	machineIDFunc = func() (string, error) { return "fixture-machine", nil }
	uidFunc = func() int { return 1000 }
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	getSecretServiceKeyFunc = func(timeout time.Duration) ([]byte, error) {
		if timeout != ssInitialTimeout {
			t.Fatal("unbounded lookup")
		}
		return key, nil
	}
	getOrCreateSecretServiceKeyFunc = func(time.Duration) ([]byte, error) {
		t.Fatal("existing installation attempted key creation")
		return nil, nil
	}
	for _, name := range []string{ProviderLinuxSecret, ProviderLinuxMachine} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join("..", "config", "testdata", "legacy", name)
			for _, file := range []string{".salt", ".crypto-state", "config.toml"} {
				raw, err := os.ReadFile(filepath.Join(source, file))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, file), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			layout := paths.NewLayout(dir, dir)
			before := cryptoFiles(t, dir)
			for _, open := range []func(paths.Layout) (Provider, error){OpenExistingProvider, NewDefaultProvider} {
				if _, err := open(layout); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(before, cryptoFiles(t, dir)) {
				t.Fatal("existing material changed")
			}
			if err := os.Remove(filepath.Join(dir, ".salt")); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDefaultProvider(layout); err == nil {
				t.Fatal("missing salt repaired")
			}
			if _, err := os.Stat(filepath.Join(dir, ".salt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing salt created")
			}
		})
	}
	dir := t.TempDir()
	layout := paths.NewLayout(dir, dir)
	if _, err := OpenExistingProvider(layout); err == nil {
		t.Fatal("empty material accepted")
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
		t.Fatal("read-only open created material")
	}
}
func TestPinnedLinuxProviderFailureDoesNotReplaceMaterials(t *testing.T) {
	oldRead, oldCreate := getSecretServiceKeyFunc, getOrCreateSecretServiceKeyFunc
	defer func() { getSecretServiceKeyFunc = oldRead; getOrCreateSecretServiceKeyFunc = oldCreate }()
	key := bytes.Repeat([]byte{5}, 32)
	dir := t.TempDir()
	layout := paths.NewLayout(dir, dir)
	if _, err := GetSalt(layout); err != nil {
		t.Fatal(err)
	}
	if err := PersistState(layout, newKeyProvider(ProviderLinuxSecret, key)); err != nil {
		t.Fatal(err)
	}
	before := cryptoFiles(t, dir)
	getOrCreateSecretServiceKeyFunc = func(time.Duration) ([]byte, error) { t.Fatal("attempted replacement key"); return nil, nil }
	for _, lookup := range []func(time.Duration) ([]byte, error){func(time.Duration) ([]byte, error) { return nil, errors.New("locked") }, func(time.Duration) ([]byte, error) { return bytes.Repeat([]byte{6}, 32), nil }} {
		getSecretServiceKeyFunc = lookup
		if _, err := NewDefaultProvider(layout); err == nil {
			t.Fatal("unavailable/wrong key accepted")
		}
		if !reflect.DeepEqual(before, cryptoFiles(t, dir)) {
			t.Fatal("failed open changed material")
		}
	}
}
func cryptoFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = string(raw)
	}
	return out
}
