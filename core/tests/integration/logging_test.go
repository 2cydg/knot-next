package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessFileLogUsesRuntimePathAndClosesOnShutdown(t *testing.T) {
	instance := startCore(t)
	raw, err := os.ReadFile(instance.runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	var discovery struct {
		LogPath    string `json:"log_path"`
		InstanceID string `json:"instance_id"`
	}
	if err = json.Unmarshal(raw, &discovery); err != nil || discovery.LogPath == "" {
		t.Fatalf("missing log discovery %s %v", raw, err)
	}
	logData, err := os.ReadFile(discovery.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logData, []byte("core.ready")) || !bytes.Contains(logData, []byte(discovery.InstanceID)) {
		t.Fatalf("log lacks real readiness/instance: %s", logData)
	}
	token, err := os.ReadFile(instance.tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	client := &sftpAPIClient{baseURL: fmt.Sprintf("http://127.0.0.1:%d", instance.info.Port), token: string(bytes.TrimSpace(token)), http: &http.Client{Timeout: 3e9}}
	var profile struct {
		ID string `json:"id"`
	}
	client.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"alias": "log-test", "host": "127.0.0.1", "port": 1, "user": "test", "auth_method": "password"}, http.StatusCreated, &profile)
	secret := "PASSWORD_PROCESS_SENTINEL_631"
	client.call(t, http.MethodPut, "/v1/secrets/servers/"+profile.ID+"/password", map[string]any{"password": secret}, http.StatusOK, nil)
	// Config and migration errors exercise the management diagnostics without
	// logging plaintext input or the source document.
	client.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"alias": "invalid", "password": secret}, http.StatusBadRequest, nil)
	source := filepath.Join(t.TempDir(), "broken.toml")
	if err = os.WriteFile(source, []byte("invalid TOML "+secret), 0600); err != nil {
		t.Fatal(err)
	}
	client.call(t, http.MethodGet, "/v1/config/migration/plan?source_path="+source, nil, http.StatusBadRequest, nil)
	client.call(t, http.MethodPost, "/v1/shutdown", nil, http.StatusOK, nil)
	if code := instance.waitExit(t, 10*time.Second); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	logData, err = os.ReadFile(discovery.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range [][]byte{bytes.TrimSpace(token), []byte(secret)} {
		if bytes.Contains(logData, sentinel) {
			t.Fatal("process log exposed secret")
		}
	}
	for _, message := range []string{"core.starting", "core.ready", "API request failed", "core.stopping", "core.stopped"} {
		if !bytes.Contains(logData, []byte(message)) {
			t.Fatalf("missing lifecycle diagnostic %s", message)
		}
	}
	// Rename after exit also verifies there is no retained Windows file handle.
	if err = os.Rename(discovery.LogPath, discovery.LogPath+".checked"); err != nil {
		t.Fatal(err)
	}
}
