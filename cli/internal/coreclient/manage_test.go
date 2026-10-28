package coreclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteDTOsExcludeViewAndSecretFields(t *testing.T) {
	values := []any{
		ServerInput(Server{ID: "s", Alias: "server", PasswordSet: true}),
		ProxyInput(Proxy{ID: "p", Alias: "proxy", PasswordSet: true}),
		KeyWrite{ID: "k", Alias: "key"},
		ProviderInput(SyncProvider{ID: "sync", Alias: "sync", PasswordSet: true, AccessKeyIDSet: true, Default: true}),
	}
	for _, v := range values {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"password", "private_key", "access_key_id", "secret_access_key", "session_token", "password_set", "last_used", "default", "private_key_set", "forwards"} {
			if _, ok := fields[key]; ok {
				t.Fatalf("write DTO leaked %s", key)
			}
		}
	}
}

func TestProxyDraftPasswordOmissionAndExplicitClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse", true: "clear"}[clear], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/proxy-tests" || r.Header.Get("Authorization") != "Bearer token" {
					t.Error("wrong proxy request")
				}
				var body struct {
					Draft map[string]json.RawMessage `json:"draft"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				p, exists := body.Draft["password"]
				if exists != clear || (clear && string(p) != `""`) {
					t.Error("password omission changed")
				}
				_, _ = w.Write([]byte(`{"data":{"passed":true,"scope":"endpoint","stages":[{"name":"endpoint","passed":true,"duration_ms":1}]}}`))
			}))
			defer server.Close()
			c, err := New(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			d := &ProxyDraft{Type: "http", Host: "127.0.0.1", Port: 8080}
			if clear {
				v := ""
				d.Password = &v
			}
			out, err := c.TestProxy(context.Background(), ProxyTestRequest{ProxyID: "p", Draft: d})
			if err != nil || !out.Passed || out.Scope != "endpoint" || len(out.Stages) != 1 {
				t.Fatal("proxy response", err)
			}
		})
	}
}

func TestForwardCreateAndControlUseReturnedConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		state := "starting"
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/forwards":
			if body["server_id"] != "server" || body["connection_id"] != "" {
				t.Error("frontend chose a connection")
			}
			w.WriteHeader(202)
		case r.Method == "POST" && r.URL.Path == "/v1/forwards/forward_actual/control":
			if body["op"] == "start" {
				state = "active"
			} else if body["op"] == "stop" {
				state = "stopped"
			} else {
				t.Error("wrong control")
			}
		case r.Method == "DELETE" && r.URL.Path == "/v1/forwards/forward_actual":
			state = "deleted"
		default:
			t.Error("wrong forward operation", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Forward{ID: "forward_actual", ServerID: "server", ConnectionID: "conn_actual", Type: "dynamic", Bind: "127.0.0.1:0", BoundAddress: "127.0.0.1:32768", Origin: "temporary", State: state}})
	}))
	defer server.Close()
	c, err := New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	v, err := c.CreateForward(ctx, ForwardRequest{ServerID: "server", Type: "dynamic", Bind: "127.0.0.1:0"})
	if err != nil || v.ID != "forward_actual" || v.ConnectionID != "conn_actual" || v.State != "starting" || v.BoundAddress != "127.0.0.1:32768" {
		t.Fatal("forward response lost", err)
	}
	for _, op := range []string{"start", "stop", "delete"} {
		v, err = c.ControlForward(ctx, v.ID, op)
		if err != nil || v.ConnectionID != "conn_actual" {
			t.Fatal("control lost resource binding", err)
		}
	}
	if v.State != "deleted" {
		t.Fatal("delete was not queried from response")
	}
}

// Called by the real-core integration fixture. All state changes go through the
// same typed client used by the TUI; no core packages or config files are used.
func exerciseManagement(t *testing.T, ctx context.Context, c *Client) {
	t.Helper()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	proxy, err := c.WriteProxy(ctx, true, ProxyWrite{Alias: "corp", Type: "http", Host: "127.0.0.1", Port: 8080, Username: "ops"})
	check(err)
	check(c.Password(ctx, Proxies, proxy.ID, "proxy-secret", false))
	proxyView, err := c.Proxy(ctx, proxy.ID)
	check(err)
	if !proxyView.PasswordSet {
		t.Fatal("proxy password not set")
	}
	key, err := c.WriteKey(ctx, true, KeyWrite{Alias: "identity"})
	check(err)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	check(err)
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(private), []byte("passphrase"), x509.PEMCipherAES256)
	check(err)
	path := filepath.Join(t.TempDir(), "id_rsa")
	check(os.WriteFile(path, pem.EncodeToMemory(block), 0600))
	if code := Code(c.KeyMaterial(ctx, key.ID, KeyMaterialWrite{SourcePath: path}, false)); code != "PASSPHRASE_REQUIRED" {
		t.Fatal("missing real passphrase challenge", code)
	}
	if code := Code(c.KeyMaterial(ctx, key.ID, KeyMaterialWrite{SourcePath: path, Passphrase: "wrong"}, false)); code != "INVALID_PRIVATE_KEY" {
		t.Fatal("wrong passphrase accepted", code)
	}
	check(c.KeyMaterial(ctx, key.ID, KeyMaterialWrite{SourcePath: path, Passphrase: "passphrase"}, false))
	check(os.Remove(path))
	keyView, err := c.Key(ctx, key.ID)
	check(err)
	if !keyView.PrivateKeySet || !keyView.Encrypted || keyView.Fingerprint == "" || keyView.SourcePath != "" {
		t.Fatal("real key import metadata")
	}
	server, err := c.WriteServer(ctx, true, ServerWrite{Alias: "managed", Host: "127.0.0.1", Port: 22, User: "ops", AuthMethod: "key", KeyID: key.ID, ProxyID: proxy.ID, KnownHostsPath: "/tmp/fixture-known-hosts", Tags: []string{"Production"}})
	check(err)
	if code := Code(c.DeleteResource(ctx, Keys, key.ID)); code != "CONFLICT" {
		t.Fatal("referenced key delete", code)
	}
	if code := Code(c.DeleteResource(ctx, Proxies, proxy.ID)); code != "CONFLICT" {
		t.Fatal("referenced proxy delete", code)
	}
	defs := []ForwardDefinition{{Type: "local", Bind: "127.0.0.1:0", Target: "localhost:80", Enabled: true}, {Type: "dynamic", Bind: "127.0.0.1:0", Enabled: false}}
	check(c.PutForwardDefinitions(ctx, server.ID, defs))
	actual, err := c.ForwardDefinitions(ctx, server.ID)
	check(err)
	if len(actual) != 2 || actual[0].ID == "" || actual[1].Enabled {
		t.Fatal("saved forwarding definitions")
	}
	input := ServerInput(server)
	input.Alias = "managed-renamed"
	_, err = c.WriteServer(ctx, false, input)
	check(err)
	actual, err = c.ForwardDefinitions(ctx, server.ID)
	check(err)
	if len(actual) != 2 {
		t.Fatal("ordinary update lost forwards")
	}
	serverView, err := c.Server(ctx, server.ID)
	check(err)
	if serverView.KnownHostsPath != "/tmp/fixture-known-hosts" || serverView.LastUsed != nil {
		t.Fatal("ordinary metadata changed")
	}
	_, err = c.CreateForward(ctx, ForwardRequest{ServerID: server.ID, Type: "dynamic", Bind: "127.0.0.1:0"})
	if Code(err) != "CONFLICT" {
		t.Fatal("temporary forward created without SSH connection", Code(err))
	}
	running, err := c.Forwards(ctx, server.ID)
	check(err)
	connections, err := c.Connections(ctx, server.ID)
	check(err)
	if len(running) != 0 || len(connections) != 0 {
		t.Fatal("configuration fabricated runtime state")
	}
	check(c.SyncPassword(ctx, "sync-password", false))
	provider, err := c.WriteProvider(ctx, true, ProviderWrite{Alias: "backup", Type: "webdav", URL: "https://example.invalid/config", Username: "ops"})
	check(err)
	check(c.Password(ctx, Providers, provider.ID, "webdav-secret", false))
	check(c.DefaultProvider(ctx, provider.ID, false))
	_, err = c.WriteProvider(ctx, false, ProviderWrite{ID: provider.ID, Alias: provider.Alias, Type: "s3", Bucket: "backups", Key: "config.toml.enc", Region: "auto", Endpoint: "https://s3.example.invalid", PathStyle: true})
	check(err)
	check(c.S3Credentials(ctx, provider.ID, S3CredentialsWrite{AccessKeyID: "access", SecretAccessKey: "secret", SessionToken: "session"}, false))
	view, err := c.Provider(ctx, provider.ID)
	check(err)
	if view.PasswordSet || !view.AccessKeyIDSet || !view.SecretAccessKeySet || !view.SessionTokenSet || !view.Default || view.URL != "" {
		t.Fatal("provider switch did not clean old fields")
	}
	check(c.S3Credentials(ctx, provider.ID, S3CredentialsWrite{}, true))
	check(c.DefaultProvider(ctx, provider.ID, true))
	check(c.KeyMaterial(ctx, key.ID, KeyMaterialWrite{}, true))
	keyView, err = c.Key(ctx, key.ID)
	check(err)
	if keyView.PrivateKeySet || keyView.Fingerprint != "" || keyView.Type != "" {
		t.Fatal("key clear did not clear metadata")
	}
	check(c.Password(ctx, Proxies, proxy.ID, "", true))
	check(c.SyncPassword(ctx, "", true))
	for _, item := range []struct {
		Kind Resource
		ID   string
	}{{Servers, server.ID}, {Keys, key.ID}, {Proxies, proxy.ID}, {Providers, provider.ID}} {
		check(c.DeleteResource(ctx, item.Kind, item.ID))
	}
}
