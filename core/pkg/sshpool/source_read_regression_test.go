package sshpool

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"knot-core/internal/keyutil"
	"knot-core/pkg/config"
)

func TestLegacyKeyAuthenticationAndIdentityShareBoundedReads(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(block)
	if err := os.WriteFile("key", raw, 0600); err != nil {
		t.Fatal(err)
	}
	server := config.ServerProfile{ID: "srv", AuthMethod: config.AuthMethodKey, KeyID: "key"}
	cfg := config.RuntimeConfig{Keys: map[string]config.KeyMetadata{"key": {ID: "key", SourcePath: "key"}}}
	methods, closer, err := authMethodsContext(context.Background(), server, cfg, DialOptions{})
	if err != nil || len(methods) != 1 || closer != nil {
		t.Fatal("valid legacy relative path rejected", err)
	}
	if sourceKeyDigest("key") != sourceKeyDigest(filepath.Join(dir, "key")) {
		t.Fatal("legacy relative digest changed")
	}
	for _, tc := range []struct {
		path string
		want error
	}{
		{dir, keyutil.ErrSourceNotRegular},
		{filepath.Join(dir, "oversized"), keyutil.ErrSourceTooLarge},
	} {
		if errors.Is(tc.want, keyutil.ErrSourceTooLarge) {
			if err := os.WriteFile(tc.path, bytes.Repeat([]byte("x"), keyutil.MaxSourceBytes+1), 0600); err != nil {
				t.Fatal(err)
			}
		}
		cfg.Keys["key"] = config.KeyMetadata{ID: "key", SourcePath: tc.path}
		if _, _, err := authMethodsContext(context.Background(), server, cfg, DialOptions{}); !errors.Is(err, tc.want) {
			t.Fatal("authentication bypassed source validation", err)
		}
		if sourceKeyDigest(tc.path) != "unreadable" {
			t.Fatal("identity bypassed source validation")
		}
	}
}
