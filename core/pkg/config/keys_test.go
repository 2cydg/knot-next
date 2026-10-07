package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"golang.org/x/crypto/ssh"
	"knot-core/internal/keyutil"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateKeyValidationAndMetadata(t *testing.T) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name string
		key  any
		bits int
	}{{"ed25519", ed, 256}, {"rsa", rsaKey, 2048}} {
		t.Run(entry.name, func(t *testing.T) {
			svc := newTestService(t)
			view, err := svc.CreateKey(KeyMetadata{ID: "key", Alias: "deploy", Type: "invented", Length: 123})
			if err != nil {
				t.Fatal(err)
			}
			if view.Type != "" || view.Length != 0 {
				t.Fatal("client supplied metadata trusted")
			}
			block, err := ssh.MarshalPrivateKey(entry.key, "artificial key")
			if err != nil {
				t.Fatal(err)
			}
			private := string(pem.EncodeToMemory(block))
			view, err = svc.SetKeyPrivate("key", private, "")
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(entry.key)
			if err != nil {
				t.Fatal(err)
			}
			if view.Type != entry.name || view.Length != entry.bits || view.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) || view.Encrypted || !view.PrivateKeySet {
				t.Fatal("real key metadata incorrect")
			}
			raw, _ := os.ReadFile(svc.path)
			if bytes.Contains(raw, []byte(private)) || bytes.Contains(raw, []byte("BEGIN OPENSSH PRIVATE KEY")) {
				t.Fatal("private key persisted plaintext")
			}
			before := append([]byte(nil), raw...)
			for _, invalid := range []string{"sentinel-invalid-private", string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "-----BEGIN PRIVATE KEY-----\nbroken\n-----END PRIVATE KEY-----"} {
				if _, err := svc.SetKeyPrivate("key", invalid, ""); !errors.Is(err, ErrValidation) {
					t.Fatal("invalid private key accepted")
				}
				after, _ := os.ReadFile(svc.path)
				if !bytes.Equal(before, after) {
					t.Fatal("invalid key write changed config")
				}
			}
		})
	}
}
func TestPassphraseAndSourcePathRemainTransient(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateKey(KeyMetadata{ID: "key", Alias: "deploy"}); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "artificial", []byte("attempt-only-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(block)
	if _, err := svc.SetKeyPrivate("key", string(raw), ""); !errors.Is(err, keyutil.ErrPassphraseRequired) {
		t.Fatal("unverified encrypted payload accepted without passphrase")
	}
	view, err := svc.SetKeyPrivateWithPassphrase("key", string(raw), "", "attempt-only-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Encrypted {
		t.Fatal("encrypted key metadata missing")
	}
	cfg, err := svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyutil.Signer([]byte(cfg.Keys["key"].PrivateKey), ""); !errors.Is(err, keyutil.ErrPassphraseRequired) {
		t.Fatal("missing passphrase not typed")
	}
	if _, err := svc.SetKeyPrivateWithPassphrase("key", string(raw), "", "wrong"); !errors.Is(err, ErrValidation) {
		t.Fatal("wrong passphrase accepted")
	}
	if _, err := svc.SetKeyPrivateWithPassphrase("key", string(raw), "", "attempt-only-passphrase"); err != nil {
		t.Fatal(err)
	}
	configRaw, _ := os.ReadFile(svc.path)
	if bytes.Contains(configRaw, []byte("attempt-only-passphrase")) {
		t.Fatal("passphrase persisted")
	}
	path := filepath.Join(t.TempDir(), "private.key")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetKeyPrivate("key", "", path); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keys["key"].PrivateKey != "" || cfg.Keys["key"].SourcePath != path {
		t.Fatal("SourcePath precedence incorrect")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetKeyPrivate("key", "", path); !errors.Is(err, ErrValidation) {
		t.Fatal("missing source accepted")
	}
}

func TestKeyMetadataUpdatePreservesSourcePath(t *testing.T) {
	svc := newTestService(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateKey(KeyMetadata{ID: "key", Alias: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetKeyPrivate("key", "", path); err != nil {
		t.Fatal(err)
	}
	view, err := svc.UpdateKey("key", KeyMetadata{Alias: "renamed", Type: "invented", Length: 1})
	if err != nil {
		t.Fatal(err)
	}
	if view.SourcePath != path || !view.PrivateKeySet || view.Type != "ed25519" || view.Length != 256 {
		t.Fatal("metadata update lost material or trusted client type")
	}
}
