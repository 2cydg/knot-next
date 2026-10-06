package http

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knot-core/internal/api/response"
	"knot-core/internal/auth"
	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
	"knot-core/pkg/config"
	"knot-core/pkg/core"
	"knot-core/pkg/crypto"
	"knot-core/pkg/secret"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
)

func waitForSFTPTransfer(t *testing.T, server *Server, sessionID string, transferID string) sftp.Transfer {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req := authenticatedRequest(http.MethodGet, "/v1/sftp/"+sessionID+"/transfers/"+transferID)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("transfer status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data sftp.Transfer `json:"data"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode transfer: %v", err)
		}
		switch body.Data.State {
		case "completed", "failed", "partial_failed", "canceled":
			return body.Data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for transfer %s/%s", sessionID, transferID)
	return sftp.Transfer{}
}

func TestServerRequiresToken(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "AUTH_REQUIRED" {
		t.Fatalf("error code = %q, want AUTH_REQUIRED", body.Error.Code)
	}
}

func TestServerRejectsInvalidToken(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "AUTH_FAILED" {
		t.Fatalf("error code = %q, want AUTH_FAILED", body.Error.Code)
	}
}

func TestServerReturnsVersionEnvelope(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Data core.VersionInfo `json:"data"`
		Meta struct {
			Timestamp time.Time `json:"timestamp"`
		} `json:"meta"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Data.APIVersion != core.APIVersion {
		t.Fatalf("api version = %q, want %q", body.Data.APIVersion, core.APIVersion)
	}
	if body.Data.Compiler == "" {
		t.Fatal("missing compiler metadata")
	}
	if body.Meta.Timestamp.IsZero() {
		t.Fatal("missing response timestamp")
	}
}

func TestServerReturnsCoreEndpoints(t *testing.T) {
	tests := []string{
		"/v1/health",
		"/v1/capabilities",
		"/v1/runtime",
		"/v1/status",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			server := newTestServer(t)
			req := authenticatedRequest(http.MethodGet, path)
			rec := httptest.NewRecorder()

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			var body struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(body.Data) == 0 {
				t.Fatal("missing response data")
			}
		})
	}
}

func TestServerHealthReportsChecks(t *testing.T) {
	server := newStatefulTestServer(t)
	req := authenticatedRequest(http.MethodGet, "/v1/health")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data core.Health `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.Data.Status != "ok" {
		t.Fatalf("health status = %q, want ok: %+v", body.Data.Status, body.Data.Checks)
	}
	assertHealthCheck(t, body.Data.Checks, "token", "ok")
	assertHealthCheck(t, body.Data.Checks, "runtime_file", "ok")
	assertHealthCheck(t, body.Data.Checks, "config", "ok")
	assertHealthCheck(t, body.Data.Checks, "crypto", "ok")
	assertHealthCheck(t, body.Data.Checks, "ssh_pool", "ok")
	assertHealthCheck(t, body.Data.Checks, "listener", "ok")
}

func TestServerHealthReportsDegradedConfig(t *testing.T) {
	startedAt := time.Unix(1000, 0).UTC()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.ConfigDir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	runtimeInfo := coreruntime.NewInfo(core.DefaultVersion, core.APIVersion, 17898, []string{"127.0.0.1:17898"}, layout, startedAt, true)
	if err := coreruntime.WriteInfo(layout.RuntimePath, runtimeInfo); err != nil {
		t.Fatalf("write runtime info: %v", err)
	}
	provider := crypto.NewStaticProvider([]byte("test-key"))
	configService := config.NewService(layout, provider)
	coreService := core.New(core.DefaultVersion, startedAt)
	coreService.UseConfig(configService)
	coreService.UseSecret(secret.NewService(configService, provider))
	coreService.UseSSHPool(sshpool.NewPool())
	server := NewServer(coreService, runtimeInfo, auth.NewVerifier("test-token"), auth.NewOriginChecker(nil))

	req := authenticatedRequest(http.MethodGet, "/v1/health")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data core.Health `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.Data.Status != "degraded" {
		t.Fatalf("health status = %q, want degraded", body.Data.Status)
	}
	assertHealthCheck(t, body.Data.Checks, "token", "ok")
	assertHealthCheck(t, body.Data.Checks, "runtime_file", "ok")
	assertHealthCheck(t, body.Data.Checks, "config", "failed")
	assertHealthCheck(t, body.Data.Checks, "crypto", "ok")
	assertHealthCheck(t, body.Data.Checks, "ssh_pool", "ok")
	assertHealthCheck(t, body.Data.Checks, "listener", "ok")
}

func TestServerStatusIncludesRuntimeCounts(t *testing.T) {
	server := newStatefulTestServer(t)

	createSession := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"local","term":"xterm"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, createSession)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var createdSession struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&createdSession); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	t.Cleanup(func() {
		req := authenticatedRequest(http.MethodDelete, "/v1/sessions/"+createdSession.Data.ID)
		server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	})

	createSFTP := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"local"}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, createSFTP)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create sftp status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var createdSFTP struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&createdSFTP); err != nil {
		t.Fatalf("decode created sftp session: %v", err)
	}
	t.Cleanup(func() {
		req := authenticatedRequest(http.MethodDelete, "/v1/sftp/"+createdSFTP.Data.ID)
		server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	})

	statusReq := authenticatedRequest(http.MethodGet, "/v1/status")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, statusReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data core.Status `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body.Data.ActiveSessions != 1 {
		t.Fatalf("active sessions = %d, want 1", body.Data.ActiveSessions)
	}
	if body.Data.ActiveSFTPSessions != 1 {
		t.Fatalf("active sftp sessions = %d, want 1", body.Data.ActiveSFTPSessions)
	}
	if body.Data.RunningTransfers != 0 {
		t.Fatalf("running transfers = %d, want 0", body.Data.RunningTransfers)
	}
	if body.Data.SSHPool.Count != 0 {
		t.Fatalf("ssh pool count = %d, want 0 in local test backend", body.Data.SSHPool.Count)
	}
}

func TestServerStatusWithMissingServices(t *testing.T) {
	server := newTestServer(t)
	req := authenticatedRequest(http.MethodGet, "/v1/status")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data core.Status `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body.Data.ActiveSessions != 0 || body.Data.ActiveSFTPSessions != 0 || body.Data.RunningTransfers != 0 {
		t.Fatalf("unexpected runtime counts: %+v", body.Data)
	}
	if body.Data.SSHPool.Count != 0 || len(body.Data.SSHPool.Entries) != 0 {
		t.Fatalf("unexpected ssh pool status: %+v", body.Data.SSHPool)
	}
}

func TestServerReturnsMethodNotAllowed(t *testing.T) {
	server := newTestServer(t)
	req := authenticatedRequest(http.MethodPost, "/v1/version")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow header = %q, want GET", allow)
	}
}

func TestServerReturnsJSONNotFoundOutsideV1(t *testing.T) {
	server := newTestServer(t)
	req := authenticatedRequest(http.MethodGet, "/missing")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
}

func TestServerRejectsUnexpectedOrigin(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestServerHandlesAllowedCORSPreflightWithoutToken(t *testing.T) {
	server := newTestServerWithOrigins(t, []string{"http://app.localhost"})
	req := httptest.NewRequest(http.MethodOptions, "/v1/version", nil)
	req.Header.Set("Origin", "http://app.localhost")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "http://app.localhost" {
		t.Fatalf("Access-Control-Allow-Origin = %q", origin)
	}
}

func TestServerRejectsUnexpectedCORSPreflight(t *testing.T) {
	server := newTestServerWithOrigins(t, []string{"http://app.localhost"})
	req := httptest.NewRequest(http.MethodOptions, "/v1/version", nil)
	req.Header.Set("Origin", "http://evil.localhost")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestServerConfigAndSecretAPIsRedactSecrets(t *testing.T) {
	server := newStatefulTestServer(t)

	create := authenticatedJSONRequest(http.MethodPost, "/v1/config/servers", `{
		"id":"srv_test",
		"alias":"web",
		"host":"127.0.0.1",
		"port":22,
		"user":"root",
		"auth_method":"password"
	}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	setSecret := authenticatedJSONRequest(http.MethodPut, "/v1/secrets/servers/srv_test/password", `{"password":"super-secret"}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, setSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("super-secret")) {
		t.Fatal("secret response leaked plaintext password")
	}

	getConfig := authenticatedRequest(http.MethodGet, "/v1/config/servers/srv_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, getConfig)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			PasswordSet bool   `json:"password_set"`
			Password    string `json:"password"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Data.PasswordSet {
		t.Fatal("password_set = false, want true")
	}
	if body.Data.Password != "" {
		t.Fatalf("password was exposed: %q", body.Data.Password)
	}
}

func TestServerConfigAPIRejectsSecretFields(t *testing.T) {
	server := newStatefulTestServer(t)
	req := authenticatedJSONRequest(http.MethodPost, "/v1/config/servers", `{
		"id":"srv_test",
		"alias":"web",
		"host":"127.0.0.1",
		"port":22,
		"user":"root",
		"password":"do-not-store"
	}`)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("do-not-store")) {
		t.Fatal("validation response leaked plaintext password")
	}
}

func TestServerConfigAPIRejectsKeyPrivateMaterial(t *testing.T) {
	server := newStatefulTestServer(t)
	req := authenticatedJSONRequest(http.MethodPost, "/v1/config/keys", `{
		"id":"key_test",
		"alias":"deploy",
		"type":"ed25519",
		"private_key":"PRIVATE KEY"
	}`)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("validation response leaked private key")
	}
}

func TestServerConfigMigrationPlan(t *testing.T) {
	server := newStatefulTestServer(t)
	req := authenticatedRequest(http.MethodGet, "/v1/config/migration/plan")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Data config.MigrationPlan `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Data.Items) == 0 {
		t.Fatal("migration plan should include at least one item")
	}
}

func TestServerConfigServersPagination(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/config/servers", `{
		"id":"srv_page",
		"alias":"paged",
		"host":"127.0.0.1",
		"port":22,
		"user":"root",
		"auth_method":"agent",
		"tags":["prod"]
	}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req := authenticatedRequest(http.MethodGet, "/v1/config/servers?page=true&tag=prod&limit=1")
	rec = httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data config.Page[config.ServerProfileView] `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Data.Total == 0 || len(body.Data.Items) != 1 {
		t.Fatalf("unexpected page: %+v", body.Data)
	}
}

func TestServerSyncProviderSecretAPIsRedactSecrets(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/config/sync-providers", `{
		"id":"sync_s3",
		"alias":"backup",
		"type":"s3",
		"bucket":"bucket",
		"key":"config.json",
		"region":"auto",
		"endpoint":"https://s3.example.com"
	}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	set := authenticatedJSONRequest(http.MethodPut, "/v1/secrets/sync-providers/sync_s3/s3-credentials", `{
		"access_key_id":"ak",
		"secret_access_key":"sk",
		"session_token":"token"
	}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, set)
	if rec.Code != http.StatusOK {
		t.Fatalf("set status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"ak"`)) || bytes.Contains(rec.Body.Bytes(), []byte(`"sk"`)) || bytes.Contains(rec.Body.Bytes(), []byte(`"token"`)) {
		t.Fatalf("secret response leaked plaintext: %s", rec.Body.String())
	}
}

func TestServerSessionLifecycle(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{
		"server_ref":"web",
		"term":"xterm-256color",
		"rows":24,
		"cols":80
	}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Data.ID == "" || created.Data.AttachURL == "" {
		t.Fatalf("missing session urls: %+v", created.Data)
	}

	control := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/"+created.Data.ID+"/control", `{"type":"resize","rows":40,"cols":120}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, control)
	if rec.Code != http.StatusOK {
		t.Fatalf("control status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var controlled struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&controlled); err != nil {
		t.Fatalf("decode control: %v", err)
	}
	if controlled.Data.Rows != 40 || controlled.Data.Cols != 120 {
		t.Fatalf("resize = %dx%d, want 40x120", controlled.Data.Rows, controlled.Data.Cols)
	}

	req := authenticatedRequest(http.MethodGet, "/v1/sessions/"+created.Data.ID+"/challenges/host-key")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServerSessionExecValidation(t *testing.T) {
	server := newStatefulTestServer(t)
	req := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/exec", `{"server_ref":"web","command":"uptime"}`)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data session.Exec `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Data.State != "completed" || body.Data.FrameworkError != "" {
		t.Fatalf("unexpected exec result: %+v", body.Data)
	}
}

func TestServerSessionRejectsControlAfterDisconnect(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	req := authenticatedRequest(http.MethodDelete, "/v1/sessions/"+created.Data.ID)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}

	control := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/"+created.Data.ID+"/control", `{"type":"resize","rows":30,"cols":100}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, control)
	if rec.Code != http.StatusConflict {
		t.Fatalf("control status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionAttachWebSocketStartsWithoutEcho(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sessions/"+created.Data.ID+"/attach")
	defer conn.Close()

	opcode, got, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if opcode != wsOpcodeText {
		t.Fatalf("opcode = %d, want text", opcode)
	}
	if !bytes.Contains(got, []byte("session.snapshot")) {
		t.Fatalf("payload = %s, want snapshot event", got)
	}
	opcode, got, err = readServerFrame(reader)
	if err != nil {
		t.Fatalf("read attach frame: %v", err)
	}
	if opcode != wsOpcodeText {
		t.Fatalf("opcode = %d, want text", opcode)
	}
	if !bytes.Contains(got, []byte("session.attached")) {
		t.Fatalf("payload = %s, want attach event", got)
	}
	if _, err := conn.Write(maskedClientFrame(wsOpcodeBinary, []byte("secret input"))); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}
}

func TestSessionAttachRejectsClosedSession(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	req := authenticatedRequest(http.MethodDelete, "/v1/sessions/"+created.Data.ID)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sessions/"+created.Data.ID+"/attach")
	defer conn.Close()
	opcode, got, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read attach error: %v", err)
	}
	if opcode != wsOpcodeText {
		t.Fatalf("opcode = %d, want text", opcode)
	}
	if !bytes.Contains(got, []byte("ATTACH_FAILED")) {
		t.Fatalf("payload = %s, want attach failed", got)
	}
}

func TestSessionEventsWebSocketReceivesStateChanges(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sessions/"+created.Data.ID+"/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	control := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/"+created.Data.ID+"/control", `{"type":"resize","rows":33,"cols":111}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, control)
	if rec.Code != http.StatusOK {
		t.Fatalf("control status = %d, body = %s", rec.Code, rec.Body.String())
	}

	opcode, got, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if opcode != wsOpcodeText {
		t.Fatalf("opcode = %d, want text", opcode)
	}
	if !bytes.Contains(got, []byte("session.resized")) {
		t.Fatalf("event = %s, want resize event", got)
	}
}

func TestSessionEventsWebSocketRespondsToPing(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sessions/"+created.Data.ID+"/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	payload := []byte("alive")
	if _, err := conn.Write(maskedClientFrame(wsOpcodePing, payload)); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	opcode, got, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != wsOpcodePong {
		t.Fatalf("opcode = %d, want pong", opcode)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("pong payload = %q, want %q", got, payload)
	}
}

func TestGlobalEventsWebSocketReceivesSessionEvents(t *testing.T) {
	server := newStatefulTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/events")
	defer conn.Close()

	_, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snapshot struct {
		Type       string `json:"type"`
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.Type != "core.snapshot" || snapshot.APIVersion != "v1" {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	create := authenticatedJSONRequest(http.MethodPost, "/v1/sessions", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data session.Resource `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	event := readGlobalEventOfType(t, reader, "session.created")
	if event.Resource != "session" || event.ResourceID != created.Data.ID {
		t.Fatalf("created event = %+v", event)
	}

	closeReq := authenticatedRequest(http.MethodDelete, "/v1/sessions/"+created.Data.ID)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, closeReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}
	event = readGlobalEventOfType(t, reader, "session.closed")
	if event.Resource != "session" || event.ResourceID != created.Data.ID {
		t.Fatalf("closed event = %+v", event)
	}
}

func TestServerSFTPLifecycleAndFileOperations(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Data.Backend != "local-sandbox" {
		t.Fatalf("backend = %q, want local-sandbox", created.Data.Backend)
	}

	mkdir := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/dirs", `{"path":"/var/log","recursive":true}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, mkdir)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mkdir status = %d, body = %s", rec.Code, rec.Body.String())
	}

	localFile := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(localFile, []byte("hello sftp"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	uploadBody := `{"source":` + strconvQuote(localFile) + `,"target":"/var/log/app.log"}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", uploadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var uploaded struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&uploaded); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	uploaded.Data = waitForSFTPTransfer(t, server, created.Data.ID, uploaded.Data.ID)
	if uploaded.Data.State != "completed" || uploaded.Data.BytesCopied != int64(len("hello sftp")) {
		t.Fatalf("upload = %+v", uploaded.Data)
	}

	list := authenticatedRequest(http.MethodGet, "/v1/sftp/"+created.Data.ID+"/files?path=/var/log")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, list)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("app.log")) {
		t.Fatalf("list response missing app.log: %s", rec.Body.String())
	}

	rename := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/rename", `{"old_path":"/var/log/app.log","new_path":"/var/log/app-renamed.log"}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, rename)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, body = %s", rec.Code, rec.Body.String())
	}

	downloadTarget := filepath.Join(t.TempDir(), "downloaded.log")
	downloadBody := `{"source":"/var/log/app-renamed.log","target":` + strconvQuote(downloadTarget) + `}`
	download := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/download", downloadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, download)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("download status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var downloaded struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&downloaded); err != nil {
		t.Fatalf("decode download: %v", err)
	}
	downloaded.Data = waitForSFTPTransfer(t, server, created.Data.ID, downloaded.Data.ID)
	got, err := os.ReadFile(downloadTarget)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if string(got) != "hello sftp" {
		t.Fatalf("download content = %q", got)
	}

	remove := authenticatedRequest(http.MethodDelete, "/v1/sftp/"+created.Data.ID+"/files?path=/var/log/app-renamed.log")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, remove)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServerSFTPGetFilesSupportsStatQuery(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	localFile := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(localFile, []byte("stat me"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", `{"source":`+strconvQuote(localFile)+`,"target":"/file.txt"}`)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var transfer struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&transfer); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	_ = waitForSFTPTransfer(t, server, created.Data.ID, transfer.Data.ID)

	req := authenticatedRequest(http.MethodGet, "/v1/sftp/"+created.Data.ID+"/files?path=/file.txt&stat=true")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"name":"file.txt"`)) {
		t.Fatalf("stat response = %s", rec.Body.String())
	}
}

func TestWriteSFTPErrorMapsPermissionDeniedToForbidden(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSFTPError(rec, response.RiskRemoteRead, "sftp/test/files", errors.New("permission denied"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestServerSFTPCleansTraversalInsideSandbox(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	localFile := filepath.Join(t.TempDir(), "escape.txt")
	if err := os.WriteFile(localFile, []byte("inside sandbox"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	uploadBody := `{"source":` + strconvQuote(localFile) + `,"target":"/../../escape.txt"}`
	req := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", uploadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var uploaded struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&uploaded); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	_ = waitForSFTPTransfer(t, server, created.Data.ID, uploaded.Data.ID)
	if _, err := os.Stat(filepath.Join(filepath.Dir(created.Data.Root), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("path traversal wrote outside session root: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(created.Data.Root, "escape.txt")); err != nil || string(got) != "inside sandbox" {
		t.Fatalf("sandbox file = %q, err = %v", got, err)
	}
}

func TestServerSFTPRejectsSymlinkEscape(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(created.Data.Root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	req := authenticatedRequest(http.MethodGet, "/v1/sftp/"+created.Data.ID+"/files?path=/link")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServerSFTPRejectsRemoveDirForFile(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	localFile := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(localFile, []byte("file"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	uploadBody := `{"source":` + strconvQuote(localFile) + `,"target":"/file.txt"}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", uploadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var uploaded struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&uploaded); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	_ = waitForSFTPTransfer(t, server, created.Data.ID, uploaded.Data.ID)

	remove := authenticatedRequest(http.MethodDelete, "/v1/sftp/"+created.Data.ID+"/dirs?path=/file.txt")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, remove)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("remove status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServerSFTPTransferResourceRoutes(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	localFile := filepath.Join(t.TempDir(), "transfer.txt")
	if err := os.WriteFile(localFile, []byte("transfer"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	body := `{"source":` + strconvQuote(localFile) + `,"target":"/transfer.txt","overwrite":true}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", body)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var started struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	final := waitForSFTPTransfer(t, server, created.Data.ID, started.Data.ID)
	if final.State != "completed" {
		t.Fatalf("final transfer = %+v", final)
	}

	req := authenticatedRequest(http.MethodGet, "/v1/sftp/"+created.Data.ID+"/transfers")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list transfers status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(started.Data.ID)) {
		t.Fatalf("transfer list missing %q: %s", started.Data.ID, rec.Body.String())
	}
}

func TestServerSFTPCancelMissingTransferRoute(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	req := authenticatedRequest(http.MethodDelete, "/v1/sftp/"+created.Data.ID+"/transfers/transfer_missing")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServerSFTPMatchesAndBatchUpload(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	srcRoot := t.TempDir()
	a := filepath.Join(srcRoot, "a.log")
	b := filepath.Join(srcRoot, "b.log")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}
	body := `{"sources":[` + strconvQuote(a) + `,` + strconvQuote(b) + `],"target":"/logs/","overwrite":true}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/batch-upload", body)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("batch upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var started struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatalf("decode batch upload: %v", err)
	}
	final := waitForSFTPTransfer(t, server, created.Data.ID, started.Data.ID)
	if final.State != "completed" {
		t.Fatalf("batch transfer = %+v", final)
	}

	req := authenticatedRequest(http.MethodGet, "/v1/sftp/"+created.Data.ID+"/matches?pattern=/logs/*.log")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("matches status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("a.log")) || !bytes.Contains(rec.Body.Bytes(), []byte("b.log")) {
		t.Fatalf("matches response = %s", rec.Body.String())
	}
}

func TestServerSFTPTransferEvents(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	localFile := filepath.Join(t.TempDir(), "event.txt")
	if err := os.WriteFile(localFile, []byte("events"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	uploadBody := `{"source":` + strconvQuote(localFile) + `,"target":"/event.txt"}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", uploadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	_, got, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if !bytes.Contains(got, []byte("sftp.transfer.")) {
		t.Fatalf("event = %s, want transfer event", got)
	}
}

func TestGlobalEventsWebSocketReceivesSFTPTransferSummary(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	localFile := filepath.Join(t.TempDir(), "upload.txt")
	if err := os.WriteFile(localFile, []byte("events"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	body := `{"source":` + strconvQuote(localFile) + `,"target":"/remote.txt","overwrite":true}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", body)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	event := readGlobalEventOfType(t, reader, "sftp.transfer.completed")
	if event.Resource != "sftp_transfer" || event.ResourceID == "" {
		t.Fatalf("transfer event = %+v", event)
	}
	if event.Data["session_id"] != created.Data.ID {
		t.Fatalf("event session_id = %#v, want %q", event.Data["session_id"], created.Data.ID)
	}
}

func TestGlobalEventsWebSocketPreservesSFTPTransferOrder(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	localFile := filepath.Join(t.TempDir(), "upload.txt")
	if err := os.WriteFile(localFile, []byte("ordered events"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	body := `{"source":` + strconvQuote(localFile) + `,"target":"/ordered.txt","overwrite":true}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", body)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	started := readGlobalEventOfType(t, reader, "sftp.transfer.started")
	completed := readGlobalEventOfType(t, reader, "sftp.transfer.completed")
	if started.ResourceID == "" || started.ResourceID != completed.ResourceID {
		t.Fatalf("ordered transfer ids = %q, %q", started.ResourceID, completed.ResourceID)
	}
}

func TestServerSFTPTransferEventsCloseSessionDoesNotPanic(t *testing.T) {
	server := newStatefulTestServer(t)
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	closeReq := authenticatedRequest(http.MethodDelete, "/v1/sftp/"+created.Data.ID)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, closeReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, _, err := readServerFrame(reader); err == nil {
		t.Fatal("expected websocket to close after sftp session close")
	}
}

func TestClearConnectionsPublishesEvent(t *testing.T) {
	server := newStatefulTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	req := authenticatedRequest(http.MethodPost, "/v1/connections/clear")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Closed int `json:"closed"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode clear: %v", err)
	}
	if body.Data.Closed != 0 {
		t.Fatalf("closed = %d, want 0 for local test backend", body.Data.Closed)
	}
	event := readGlobalEventOfType(t, reader, "core.connections_cleared")
	if event.Resource != "connections" {
		t.Fatalf("clear event = %+v", event)
	}
}

func TestShutdownCallsInjectedFunction(t *testing.T) {
	server := newStatefulTestServer(t)
	called := make(chan struct{}, 1)
	server.core.UseShutdown(func() {
		called <- struct{}{}
	})

	req := authenticatedRequest(http.MethodPost, "/v1/shutdown")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("shutdown status = %d, body = %s", rec.Code, rec.Body.String())
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
}

func newTestServer(t testing.TB) *Server {
	t.Helper()
	return newTestServerWithOrigins(t, nil)
}

func newTestServerWithOrigins(t testing.TB, origins []string) *Server {
	t.Helper()
	startedAt := time.Unix(1000, 0).UTC()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	runtimeInfo := coreruntime.NewInfo(core.DefaultVersion, core.APIVersion, 17898, []string{"127.0.0.1:17898"}, layout, startedAt, true)
	if err := coreruntime.WriteInfo(layout.RuntimePath, runtimeInfo); err != nil {
		t.Fatalf("write runtime info: %v", err)
	}
	return NewServer(core.New(core.DefaultVersion, startedAt), runtimeInfo, auth.NewVerifier("test-token"), auth.NewOriginChecker(origins))
}

func newStatefulTestServer(t *testing.T) *Server {
	t.Helper()
	startedAt := time.Unix(1000, 0).UTC()
	root := t.TempDir()
	layout := paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
	provider := crypto.NewStaticProvider([]byte("test-key"))
	configService := config.NewService(layout, provider)
	sharedPool := sshpool.NewPool()
	coreService := core.New(core.DefaultVersion, startedAt)
	coreService.UseConfig(configService)
	coreService.UseSecret(secret.NewService(configService, provider))
	sessionService := session.NewService()
	sessionService.UseConfig(configService)
	sessionService.UsePool(sharedPool)
	sessionService.UseLocalTestBackend()
	coreService.UseSession(sessionService)
	sftpService := sftp.NewService(filepath.Join(root, "sftp"))
	sftpService.UseConfig(configService)
	sftpService.UseSession(sessionService)
	sftpService.UsePool(sharedPool)
	sftpService.UseLocalTestBackend()
	coreService.UseSFTP(sftpService)
	coreService.UseSSHPool(sharedPool)
	runtimeInfo := coreruntime.NewInfo(core.DefaultVersion, core.APIVersion, 17898, []string{"127.0.0.1:17898"}, layout, startedAt, true)
	if err := coreruntime.WriteInfo(layout.RuntimePath, runtimeInfo); err != nil {
		t.Fatalf("write runtime info: %v", err)
	}
	return NewServer(coreService, runtimeInfo, auth.NewVerifier("test-token"), auth.NewOriginChecker(nil))
}

func assertHealthCheck(t *testing.T, checks []core.HealthCheck, name string, status string) {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			if check.Status != status {
				t.Fatalf("check %q status = %q, want %q", name, check.Status, status)
			}
			return
		}
	}
	t.Fatalf("missing health check %q in %+v", name, checks)
}

func authenticatedRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

func authenticatedJSONRequest(method, path string, body string) *http.Request {
	req := authenticatedRequest(method, path)
	req.Body = io.NopCloser(bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func openTestWebSocket(t *testing.T, ts *httptest.Server, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial websocket server: %v", err)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	request := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + ts.Listener.Addr().String() + "\r\n" +
		"Authorization: Bearer test-token\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("handshake status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	return conn, reader
}

func maskedClientFrame(opcode int, payload []byte) []byte {
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x80 | byte(opcode), 0x80 | byte(len(payload))}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

func readServerFrame(reader *bufio.Reader) (int, []byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	second, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode := int(first & 0x0f)
	length := uint64(second & 0x7f)
	switch length {
	case 126:
		b1, err := reader.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		b2, err := reader.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		length = uint64(b1)<<8 | uint64(b2)
	case 127:
		var n uint64
		for range 8 {
			b, err := reader.ReadByte()
			if err != nil {
				return 0, nil, err
			}
			n = n<<8 | uint64(b)
		}
		length = n
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

func readGlobalEventOfType(t *testing.T, reader *bufio.Reader, eventType string) core.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read global event: %v", err)
		}
		if opcode != wsOpcodeText {
			continue
		}
		var event core.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode global event %s: %v", payload, err)
		}
		if event.Type == eventType {
			return event
		}
	}
	t.Fatalf("timed out waiting for event %q", eventType)
	return core.Event{}
}
