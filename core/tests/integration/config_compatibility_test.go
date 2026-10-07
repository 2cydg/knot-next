package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	apihttp "knot-core/internal/api/http"
	"knot-core/internal/auth"
	"knot-core/internal/paths"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

type keyAPI struct {
	client  *sftpAPIClient
	remote  *sshserver.Server
	layout  paths.Layout
	cfg     *config.Service
	private any
}

func newKeyAPI(t *testing.T, legacy bool) *keyAPI {
	t.Helper()
	return newKeyAPIWithKey(t, legacy, nil)
}

func newKeyAPIWithKey(t *testing.T, legacy bool, private any) *keyAPI {
	t.Helper()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "knot"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if legacy {
		seed := sha256.Sum256([]byte("Knot legacy fixture artificial private key"))
		private = ed25519.NewKeyFromSeed(seed[:])
	} else if private == nil {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		private = key
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	remote := sshserver.New(t, sshserver.Config{User: "fixture", SFTPRoot: t.TempDir(), PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
			return nil, fmt.Errorf("test key rejected")
		}
		return nil, nil
	}})
	remote.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, SendExitStatus: true, Writes: []sshserver.ScriptedWrite{{Data: []byte("legacy-key-authenticated")}}})
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(255 - i)
	}
	provider := crypto.NewKeyProvider(crypto.ProviderLinuxSecret, rawKey, nil)
	cfg := config.NewService(layout, provider)
	if legacy {
		dir := filepath.Join("..", "..", "pkg", "config", "testdata", "legacy", crypto.ProviderLinuxSecret)
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
			dest := layout.ConfigDir
			if entry.Name() == "state.json" {
				dest = layout.StateDir
			}
			if err := os.WriteFile(filepath.Join(dest, entry.Name()), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		// The copied installation is pointed at the controlled remote before the
		// read-only baseline is captured. Secret/trust formats remain legacy formats.
		var disk map[string]any
		raw, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := toml.Decode(string(raw), &disk); err != nil {
			t.Fatal(err)
		}
		profile := disk["servers"].(map[string]any)["srv_legacy"].(map[string]any)
		profile["host"] = remote.Host()
		profile["port"] = int64(remote.Port())
		profile["proxy_id"] = ""
		profile["jump_host_ids"] = []string{}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(disk); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(layout.ConfigDir, "config.toml"), buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := cfg.CreateKey(config.KeyMetadata{ID: "key_legacy", Alias: "deploy"}); err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "artificial", []byte("attempt-only-passphrase"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.SetKeyPrivateWithPassphrase("key_legacy", string(pem.EncodeToMemory(block)), "", "attempt-only-passphrase"); err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.CreateServer(config.ServerProfile{ID: "srv_legacy", Alias: "prod", Host: remote.Host(), Port: remote.Port(), User: "fixture", AuthMethod: "key", KeyID: "key_legacy"}); err != nil {
			t.Fatal(err)
		}
	}
	hosts := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(remote.Addr()))}, remote.HostKey()) + "\n"
	if err := os.WriteFile(filepath.Join(layout.ConfigDir, "known_hosts"), []byte(hosts), 0600); err != nil {
		t.Fatal(err)
	}
	pool := sshpool.NewPool()
	sessions := session.NewService()
	sessions.UseConfig(cfg)
	sessions.UsePool(pool)
	files := sftp.NewService(filepath.Join(root, "sftp"))
	files.UseConfig(cfg)
	files.UsePool(pool)
	files.UseSession(sessions)
	service := core.New("key-compatibility-test", time.Now())
	service.UseConfig(cfg)
	service.UseSecret(secret.NewService(cfg, provider))
	service.UseSession(sessions)
	service.UseSFTP(files)
	service.UseSSHPool(pool)
	tracker := apihttp.NewConnTracker()
	api := apihttp.NewServer(service, nil, auth.NewVerifier("key-test-token"), auth.NewOriginChecker(nil), apihttp.WithConnTracker(tracker))
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		tracker.CloseAll()
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	client := &sftpAPIClient{baseURL: server.URL, token: "key-test-token", serverRef: "srv_legacy", http: &http.Client{Timeout: 5 * time.Second}}
	t.Cleanup(client.http.CloseIdleConnections)
	return &keyAPI{client: client, remote: remote, layout: layout, cfg: cfg, private: private}
}
func configFiles(t *testing.T, layout paths.Layout) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{layout.ConfigDir, layout.StateDir} {
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
			sum := sha256.Sum256(raw)
			out[filepath.Join(dir, entry.Name())] = fmt.Sprintf("%x", sum)
		}
	}
	return out
}
func TestLegacyInstallationConnectsWithoutApply(t *testing.T) {
	fixture := newKeyAPI(t, true)
	client := fixture.client
	before := configFiles(t, fixture.layout)
	for _, route := range []string{"/v1/config", "/v1/config/migration", "/v1/config/migration/plan", "/v1/config/servers", "/v1/secrets"} {
		client.call(t, http.MethodGet, route, nil, http.StatusOK, nil)
	}
	var result struct {
		State         string `json:"state"`
		Stdout        string `json:"stdout"`
		FrameworkCode string `json:"framework_code"`
	}
	client.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "prod", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.State != "completed" || result.Stdout != "legacy-key-authenticated" {
		t.Fatalf("legacy key/trust did not authenticate: %+v", result)
	}
	if !reflect.DeepEqual(before, configFiles(t, fixture.layout)) {
		t.Fatal("read/connect mutated legacy installation")
	}
	// Trust remains enforced after the key changes in the file, even if a pool
	// connection was used before; a new SFTP session uses the known-hosts policy.
	client.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": "prod", "host_key_policy": "strict"}, http.StatusCreated, nil)
}
func waitKeyState(t *testing.T, c *sftpAPIClient, route, id, wanted string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var v struct {
			State          string `json:"state"`
			FrameworkError string `json:"framework_error"`
		}
		c.call(t, http.MethodGet, route+"/"+id, nil, http.StatusOK, &v)
		if v.State == wanted {
			return
		}
		if v.State == "failed" {
			t.Fatalf("key session failed: %+v", v)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session did not reach %s", wanted)
}
func TestEncryptedPrivateKeyChallengesThroughHTTP(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(private), []byte("attempt-only-passphrase"), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	rawPEM := pem.EncodeToMemory(block)
	for _, format := range []string{"openssh", "pem-inline", "pem-source"} {
		for _, route := range []string{"/v1/sessions", "/v1/sftp"} {
			t.Run(format+route, func(t *testing.T) {
				var key any
				if format != "openssh" {
					key = private
				}
				fixture := newKeyAPIWithKey(t, false, key)
				c := fixture.client
				if format == "pem-inline" {
					c.call(t, http.MethodPut, "/v1/secrets/keys/key_legacy/private", map[string]any{"private_key": string(rawPEM), "passphrase": "attempt-only-passphrase"}, http.StatusOK, nil)
				} else if format == "pem-source" {
					path := filepath.Join(t.TempDir(), "id_rsa")
					if err := os.WriteFile(path, rawPEM, 0600); err != nil {
						t.Fatal(err)
					}
					c.call(t, http.MethodPut, "/v1/secrets/keys/key_legacy/private", map[string]any{"source_path": path}, http.StatusOK, nil)
				}
				before := configFiles(t, fixture.layout)
				var resource struct {
					ID string `json:"id"`
				}
				c.call(t, http.MethodPost, route, map[string]any{"server_ref": "prod", "host_key_policy": "strict", "allow_auth_retry": true}, http.StatusCreated, &resource)
				waitKeyState(t, c, route, resource.ID, "auth_pending")
				var challenge struct {
					PassphraseRequired bool   `json:"passphrase_required"`
					FailedMethod       string `json:"failed_method"`
					RetryCount         int    `json:"retry_count"`
				}
				challengeRoute := route + "/" + resource.ID + "/challenges/auth"
				c.call(t, http.MethodGet, challengeRoute, nil, http.StatusOK, &challenge)
				if !challenge.PassphraseRequired || challenge.FailedMethod != "key" {
					t.Fatal("missing passphrase did not produce a typed key challenge")
				}
				c.call(t, http.MethodPost, challengeRoute, map[string]any{"passphrase": "wrong-passphrase"}, http.StatusOK, nil)
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					c.call(t, http.MethodGet, challengeRoute, nil, http.StatusOK, &challenge)
					if challenge.RetryCount >= 2 {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if challenge.RetryCount != 2 {
					t.Fatal("wrong passphrase did not raise another challenge")
				}
				c.call(t, http.MethodPost, challengeRoute, map[string]any{"passphrase": "attempt-only-passphrase", "remember": true}, http.StatusOK, nil)
				wanted := "connected"
				if route == "/v1/sftp" {
					wanted = "open"
				}
				waitKeyState(t, c, route, resource.ID, wanted)
				c.call(t, http.MethodDelete, route+"/"+resource.ID, nil, http.StatusOK, nil)
				if !reflect.DeepEqual(before, configFiles(t, fixture.layout)) {
					t.Fatal("passphrase-only remember mutated persistent files")
				}
			})
		}
	}
}
func TestEncryptedPrivateKeyExecAndSourceChanges(t *testing.T) {
	fixture := newKeyAPI(t, false)
	c := fixture.client
	var result struct {
		State         string `json:"state"`
		FrameworkCode string `json:"framework_code"`
	}
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "prod", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.FrameworkCode != "passphrase_required" {
		t.Fatalf("exec did not distinguish passphrase: %+v", result)
	}
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "prod", "command": "test", "host_key_policy": "strict", "passphrase": "attempt-only-passphrase"}, http.StatusOK, &result)
	if result.State != "completed" {
		t.Fatalf("passphrase did not reach exec signer: %+v", result)
	}
	path := filepath.Join(t.TempDir(), "source.key")
	block, err := ssh.MarshalPrivateKey(fixture.private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	c.call(t, http.MethodPut, "/v1/secrets/keys/key_legacy/private", map[string]any{"source_path": path}, http.StatusOK, nil)
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "prod", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.State != "completed" {
		t.Fatal("SourcePath did not authenticate")
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err = ssh.MarshalPrivateKey(other, "other fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "prod", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.State != "failed" || result.FrameworkCode != "authentication_failed" {
		t.Fatal("changed SourcePath reused cached signer/client")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "prod", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.State != "failed" {
		t.Fatal("missing SourcePath reused cached client")
	}
	// No ordinary response exposes the passphrase or private key.
	req, _ := http.NewRequest(http.MethodGet, c.baseURL+"/v1/config", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("attempt-only-passphrase")) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("ordinary config leaked key secrets")
	}
}

func TestLegacyHashedAndExplicitTrustRemainEnforced(t *testing.T) {
	fixture := newKeyAPI(t, true)
	c := fixture.client
	explicit := filepath.Join(t.TempDir(), "custom-known-hosts")
	hosts, err := os.ReadFile(filepath.Join(fixture.layout.ConfigDir, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicit, hosts, 0600); err != nil {
		t.Fatal(err)
	}
	c.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"id": "srv_explicit", "alias": "explicit", "host": fixture.remote.Host(), "port": fixture.remote.Port(), "user": "fixture", "auth_method": "key", "key_id": "key_legacy", "known_hosts_path": explicit}, http.StatusCreated, nil)
	var result struct {
		State         string `json:"state"`
		FrameworkCode string `json:"framework_code"`
	}
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "explicit", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.State != "completed" {
		t.Fatal("explicit legacy trust path was not honored")
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(other)
	if err != nil {
		t.Fatal(err)
	}
	changed := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(fixture.remote.Addr()))}, signer.PublicKey()) + "\n"
	if err := os.WriteFile(explicit, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	// A distinct resource ensures a new handshake instead of testing revocation
	// of a transport whose host key was already accepted before this file edit.
	c.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"id": "srv_changed", "alias": "changed", "host": fixture.remote.Host(), "port": fixture.remote.Port(), "user": "fixture", "auth_method": "key", "key_id": "key_legacy", "known_hosts_path": explicit}, http.StatusCreated, nil)
	c.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": "changed", "command": "test", "host_key_policy": "strict"}, http.StatusOK, &result)
	if result.State != "failed" || result.FrameworkCode != "host_key_verification_failed" {
		t.Fatalf("changed legacy host key accepted: %+v", result)
	}
	raw, _ := os.ReadFile(explicit)
	if !bytes.Equal(raw, []byte(changed)) {
		t.Fatal("strict verification rewrote trust file")
	}
}
func TestExplicitTOMLImportThroughHTTP(t *testing.T) {
	fixture := newKeyAPI(t, false)
	c := fixture.client
	source := filepath.Join(t.TempDir(), "extra.toml")
	raw := []byte("[servers.extra]\nid='extra'\nalias='extra'\nhost='extra.example'\nport=22\nuser='fixture'\npassword='import-secret-sentinel'\n")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		SourceRevision string `json:"source_revision"`
		TargetRevision string `json:"target_revision"`
	}
	c.call(t, http.MethodGet, "/v1/config/migration/plan?source_path="+url.QueryEscape(source), nil, http.StatusOK, &plan)
	if plan.SourceRevision == "" || plan.TargetRevision == "" {
		t.Fatal("HTTP preview omitted revisions")
	}
	request := map[string]any{"source_path": source, "source_revision": plan.SourceRevision, "target_revision": plan.TargetRevision}
	c.call(t, http.MethodPost, "/v1/config/migration/apply", request, http.StatusOK, nil)
	c.call(t, http.MethodPost, "/v1/config/migration/apply", request, http.StatusConflict, nil)
	sourceAfter, _ := os.ReadFile(source)
	if !bytes.Equal(raw, sourceAfter) {
		t.Fatal("HTTP import changed source")
	}
	target, _ := os.ReadFile(filepath.Join(fixture.layout.ConfigDir, "config.toml"))
	if bytes.Contains(target, []byte("import-secret-sentinel")) {
		t.Fatal("HTTP import persisted plaintext")
	}
}
