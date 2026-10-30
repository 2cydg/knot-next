package corestart

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knot-cli/internal/preferences"
)

func TestCoreBinaryDetachedStartupSurvivesCallerCancellation(t *testing.T) {
	bin := os.Getenv("KNOT_TEST_CORE_BIN")
	if bin == "" {
		t.Skip("set KNOT_TEST_CORE_BIN for process integration")
	}
	dir := t.TempDir()
	configFile := filepath.Join(dir, "core configuration.toml")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	secret := "isolated-process-secret"
	if err := os.WriteFile(configFile, []byte(fmt.Sprintf("[service]\nport=%d\nsecret=%q\n", port, secret)), 0600); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(configFile)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(dir, "missing-bus"))
	selected := preferences.Core{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Secret: secret, AutoStart: true, Executable: bin, ConfigFile: configFile}
	t.Cleanup(func() {
		request, _ := http.NewRequest(http.MethodPost, selected.URL+"/v1/shutdown", nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		if response, err := (&http.Client{Timeout: time.Second}).Do(request); err == nil {
			response.Body.Close()
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err != nil {
				return
			} else {
				connection.Close()
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("detached test core did not stop")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	coordinator := New(Dependencies{})
	if err := coordinator.Ensure(ctx, selected); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	check, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := Probe(check, selected); err != nil {
		t.Fatal("caller cancellation killed detached core", err)
	}
	after, _ := os.ReadFile(configFile)
	if string(after) != string(original) {
		t.Fatal("startup changed selected configuration")
	}
	for _, name := range []string{"core.json", "token"} {
		if _, err := os.Stat(filepath.Join(dir, ".knot-state", "runtime", name)); !os.IsNotExist(err) {
			t.Fatal("legacy artifact generated", name, err)
		}
	}
	// Starting from another coordinator reuses the already-ready instance.
	if err := New(Dependencies{Start: func(preferences.Core) (Process, error) {
		t.Error("duplicate launch")
		return nil, fmt.Errorf("unexpected start")
	}}).Ensure(check, selected); err != nil {
		t.Fatal(err)
	}
}
