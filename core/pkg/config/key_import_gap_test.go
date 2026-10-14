package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"golang.org/x/crypto/ssh"
	"knot-core/pkg/crypto"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type gatedEncrypt struct {
	crypto.Provider
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *gatedEncrypt) Encrypt(raw []byte) ([]byte, error) {
	if p.armed.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.release
	}
	return p.Provider.Encrypt(raw)
}
func TestKeyImportPreservesCRLFAndConcurrentAlias(t *testing.T) {
	s := newTestService(t)
	if _, err := s.CreateKey(KeyMetadata{ID: "key", Alias: "initial"}); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.ReplaceAll(pem.EncodeToMemory(block), []byte("\n"), []byte("\r\n"))
	source := filepath.Join(t.TempDir(), "private.key")
	if err = os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	provider := &gatedEncrypt{Provider: s.crypto, entered: make(chan struct{}), release: make(chan struct{})}
	provider.armed.Store(true)
	s.crypto = provider
	done := make(chan error, 1)
	go func() { _, err := s.SetKeyPrivate("key", "", source); done <- err }()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("import did not reach encryption")
	}
	if _, err = s.UpdateKey("key", KeyMetadata{Alias: "renamed"}); err != nil {
		close(provider.release)
		t.Fatal(err)
	}
	close(provider.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	key := cfg.Keys["key"]
	if key.Alias != "renamed" || key.SourcePath != "" || !bytes.Equal([]byte(key.PrivateKey), raw) {
		t.Fatal("import overwrote alias, kept path, or rewrote original bytes")
	}
	if _, err = s.CreateKey(KeyMetadata{Alias: "bypass", SourcePath: source}); err == nil {
		t.Fatal("ordinary metadata accepted material")
	}
	if _, err = s.SetKeyPrivate("key", "", t.TempDir()); err == nil {
		t.Fatal("non-regular source accepted")
	}
	if _, err = s.SetKeyPrivate("key", "", "relative.key"); err == nil {
		t.Fatal("relative source accepted")
	}
}
