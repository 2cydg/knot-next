package config

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/ssh"
	"knot-core/internal/keyutil"
	"knot-core/internal/paths"
	"knot-core/pkg/crypto"
)

func encryptedPEMFixture(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// Traditional PEM is a supported legacy input, despite its old encryption.
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), []byte("pem-attempt-passphrase"), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), key
}

func TestReviewEncryptedPEMImportRoundTrip(t *testing.T) {
	raw, _ := encryptedPEMFixture(t)
	for _, sourcePath := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline", true: "source_path"}[sourcePath], func(t *testing.T) {
			source := newTestService(t)
			if _, err := source.CreateKey(KeyMetadata{ID: "key", Alias: "deploy"}); err != nil {
				t.Fatal(err)
			}
			inline, path := string(raw), ""
			if sourcePath {
				path = filepath.Join(t.TempDir(), "id_rsa")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				inline = ""
			}
			if _, err := source.SetKeyPrivateWithPassphrase("key", inline, path, "pem-attempt-passphrase"); err != nil {
				t.Fatal(err)
			}
			target := newTestService(t)
			target.importProvider = func(paths.Layout) (crypto.Provider, error) { return source.crypto, nil }
			before := treeHashes(t, filepath.Dir(source.layout.ConfigDir))
			plan, err := target.PlanImport(MigrationApplyRequest{SourcePath: source.path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := target.ApplyImport(MigrationApplyRequest{SourcePath: source.path, SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision}); err != nil {
				t.Fatal(err)
			}
			view, err := target.GetKey("key")
			if err != nil || !view.Encrypted || !view.PrivateKeySet || view.Type != "rsa" || view.Length != 2048 || view.Fingerprint != "" {
				t.Fatalf("locked PEM metadata/hints lost: %+v, %v", view, err)
			}
			cfg, err := target.RuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			material := []byte(cfg.Keys["key"].PrivateKey)
			if sourcePath {
				material, err = os.ReadFile(cfg.Keys["key"].SourcePath)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(material, raw) || cfg.Keys["key"].Passphrase != "" {
				t.Fatal("original PEM changed or passphrase retained")
			}
			if _, err := keyutil.Signer(material, ""); !errors.Is(err, keyutil.ErrPassphraseRequired) {
				t.Fatal("missing passphrase is not distinguishable")
			}
			if _, err := keyutil.Signer(material, "wrong"); !errors.Is(err, keyutil.ErrInvalidPrivateKey) {
				t.Fatal("wrong passphrase accepted")
			}
			if _, err := keyutil.Signer(material, "pem-attempt-passphrase"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, treeHashes(t, filepath.Dir(source.layout.ConfigDir))) {
				t.Fatal("import changed source installation")
			}
		})
	}
}

func TestReviewEncryptedPEMSourceRegistration(t *testing.T) {
	raw, _ := encryptedPEMFixture(t)
	svc := newTestService(t)
	path := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	view, err := svc.CreateKey(KeyMetadata{ID: "key", Alias: "deploy", SourcePath: path})
	if err != nil || !view.Encrypted || !view.PrivateKeySet || view.Fingerprint != "" || view.Type != "" || view.Length != 0 {
		t.Fatalf("locked PEM cannot be registered without metadata: %+v, %v", view, err)
	}
	if _, err := svc.SetKeyPrivate("key", "", path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(svc.path)
	if _, err := svc.SetKeyPrivateWithPassphrase("key", "", path, "wrong"); !errors.Is(err, keyutil.ErrInvalidPrivateKey) {
		t.Fatal("wrong validation passphrase accepted")
	}
	after, _ := os.ReadFile(svc.path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed validation changed configuration")
	}
	view, err = svc.SetKeyPrivateWithPassphrase("key", "", path, "pem-attempt-passphrase")
	if err != nil || view.Type != "rsa" || view.Length != 2048 || view.Fingerprint == "" || !view.Encrypted {
		t.Fatal("explicit validation did not derive metadata")
	}
	if _, err := svc.SetKeyPrivate("key", string(raw), ""); !errors.Is(err, keyutil.ErrPassphraseRequired) {
		t.Fatal("unverified inline PEM accepted")
	}
	block, _ := pem.Decode(raw)
	for _, mutate := range []func(*pem.Block){
		func(b *pem.Block) { b.Type = "PUBLIC KEY" },
		func(b *pem.Block) { b.Headers["DEK-Info"] = "AES-256-CBC,broken" },
		func(b *pem.Block) { b.Bytes = []byte("broken") },
	} {
		copy := &pem.Block{Type: block.Type, Bytes: block.Bytes, Headers: map[string]string{}}
		for k, v := range block.Headers {
			copy.Headers[k] = v
		}
		mutate(copy)
		if err := os.WriteFile(path, pem.EncodeToMemory(copy), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SetKeyPrivate("key", "", path); !errors.Is(err, keyutil.ErrInvalidPrivateKey) {
			t.Fatal("malformed encrypted envelope accepted")
		}
	}
}

func TestReviewUnencryptedKeyIgnoresPassphrase(t *testing.T) {
	_, key := encryptedPEMFixture(t)
	openssh, err := ssh.MarshalPrivateKey(key, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{pem.EncodeToMemory(openssh), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})} {
		svc := newTestService(t)
		if _, err := svc.CreateKey(KeyMetadata{ID: "key", Alias: "deploy"}); err != nil {
			t.Fatal(err)
		}
		view, err := svc.SetKeyPrivateWithPassphrase("key", string(raw), "", "unnecessary-passphrase")
		if err != nil || view.Encrypted || view.Fingerprint == "" {
			t.Fatal("unnecessary passphrase rejected or metadata incorrect")
		}
		if _, err := keyutil.Signer(raw, "unnecessary-passphrase"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewEmptySecretsAndS3Pairs(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateServer(ServerProfile{ID: "server", Alias: "prod", Host: "example.test", Port: 22, User: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateProxy(ProxyProfile{ID: "proxy", Alias: "corp", Type: "socks5", Host: "example.test", Port: 1080}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSyncProvider(SyncProviderConfig{ID: "web", Alias: "web", Type: "webdav", URL: "https://example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSyncProvider(SyncProviderConfig{ID: "s3", Alias: "s3", Type: "s3", Bucket: "bucket", Key: "prefix", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first-set-secret", ""} {
		server, err := svc.SetServerPassword("server", value)
		if err != nil || server.PasswordSet != (value != "") {
			t.Fatal("server presence differs from value", err)
		}
		proxy, err := svc.SetProxyPassword("proxy", value)
		if err != nil || proxy.PasswordSet != (value != "") {
			t.Fatal("proxy presence differs from value", err)
		}
		settings, err := svc.SetSyncPassword(value)
		if err != nil || settings.SyncPasswordSet != (value != "") {
			t.Fatal("sync presence differs from value", err)
		}
		web, err := svc.SetSyncProviderPassword("web", value)
		if err != nil || web.PasswordSet != (value != "") {
			t.Fatal("webdav presence differs from value", err)
		}
		s3, err := svc.SetSyncProviderS3Credentials("s3", value, value, value)
		if err != nil || s3.AccessKeyIDSet != (value != "") || s3.SecretAccessKeySet != (value != "") || s3.SessionTokenSet != (value != "") {
			t.Fatal("s3 presence differs from value", err)
		}
	}
	before, err := os.ReadFile(svc.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"", "secret"}, {"access", ""}} {
		if _, err := svc.SetSyncProviderS3Credentials("s3", pair[0], pair[1], ""); !errors.Is(err, ErrValidation) {
			t.Fatal("incomplete s3 pair accepted", err)
		}
		after, _ := os.ReadFile(svc.path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid s3 pair changed file")
		}
	}
	var disk diskConfig
	if _, err := toml.Decode(string(before), &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Servers["server"].Password != "" || disk.Proxies["proxy"].Password != "" || disk.Settings.SyncPassword != "" || disk.SyncProviders["web"].Password != "" || disk.SyncProviders["s3"].AccessKeyID != "" || disk.SyncProviders["s3"].SecretAccessKey != "" {
		t.Fatal("empty values persisted as nonempty ciphertext")
	}
}

func TestReviewRuntimeConfigCannotSerializeSecrets(t *testing.T) {
	cfg := RuntimeConfig{Settings: Settings{SyncPassword: "settings-secret"}, Servers: map[string]ServerProfile{"s": {Password: "server-secret"}}, Proxies: map[string]ProxyProfile{"p": {Password: "proxy-secret"}}, Keys: map[string]KeyMetadata{"k": {PrivateKey: "private-secret", Passphrase: "passphrase-secret"}}, SyncProviders: map[string]SyncProviderConfig{"c": {SecretAccessKey: "s3-secret"}}}
	raw, err := json.Marshal(cfg)
	if err != nil || string(raw) != "{}" {
		t.Fatal("runtime config is serializable", err)
	}
	raw, err = json.Marshal(cfg.Keys["k"])
	if err != nil || bytes.Contains(raw, []byte("passphrase-secret")) {
		t.Fatal("key passphrase is serializable", err)
	}
}

func TestReviewUnknownDefaultAliasImportWarns(t *testing.T) {
	source := newTestService(t)
	raw := []byte("[settings]\ndefault_sync_provider = \"deleted-alias\"\n")
	if err := os.WriteFile(source.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, filepath.Dir(source.layout.ConfigDir))
	if err := source.Initialize(); err != nil {
		t.Fatal(err)
	}
	metadata, err := source.Metadata()
	if err != nil || len(metadata.Warnings) != 1 {
		t.Fatal("missing config warning", err)
	}
	target := newTestService(t)
	plan, err := target.PlanImport(MigrationApplyRequest{SourcePath: source.path})
	if err != nil || len(plan.Warnings) != 1 || plan.Warnings[0].Field != "default_sync_provider" {
		t.Fatal("import failed or omitted warning", err)
	}
	summary, err := target.ApplyImport(MigrationApplyRequest{SourcePath: source.path, SourceRevision: plan.SourceRevision, TargetRevision: plan.TargetRevision})
	if err != nil || summary.Settings.DefaultSyncProvider.Value != "" {
		t.Fatal("import did not normalize missing default", err)
	}
	if !reflect.DeepEqual(before, treeHashes(t, filepath.Dir(source.layout.ConfigDir))) {
		t.Fatal("read or import changed source")
	}
	if _, err := source.UpdateSetting("log_level", "info"); err != nil {
		t.Fatal(err)
	}
	metadata, err = source.Metadata()
	if err != nil || len(metadata.Warnings) != 0 {
		t.Fatal("warning survived corrected disk setting", err)
	}
	if _, err := source.UpdateSetting("default_sync_provider", "missing-id"); !errors.Is(err, ErrValidation) {
		t.Fatal("API accepts a nonexistent default", err)
	}
}
