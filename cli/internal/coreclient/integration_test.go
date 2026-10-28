package coreclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run with KNOT_TEST_CORE_BIN pointing at a freshly built knot-core. The
// independent CLI uses only the public protocol and isolated temporary state.
func TestCoreBinaryDiscoveryConfigAndEvents(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute-state", true: "relative-state"}[relative], func(t *testing.T) {
			testCoreBinaryDiscoveryConfigAndEvents(t, relative)
		})
	}
}

func testCoreBinaryDiscoveryConfigAndEvents(t *testing.T, relative bool) {
	bin := os.Getenv("KNOT_TEST_CORE_BIN")
	if bin == "" {
		t.Skip("set KNOT_TEST_CORE_BIN for real core integration")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bin, "--port", "0")
	command.Dir = dir
	stateBase := filepath.Join(dir, "state")
	if relative {
		stateBase = "state"
	}
	command.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+stateBase, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(dir, "missing-bus"))
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("core did not stop")
		}
	})
	path := filepath.Join(dir, "state", "knot", "runtime", "core.json")
	var s Snapshot
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		s, err = Load(ctx, path)
		if err == nil {
			break
		}
		select {
		case <-time.After(40 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("core startup timeout")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.Client.Close()
	if s.Runtime.InstanceID == "" || s.Version.Version == "" || !s.Health.Authenticated || len(s.Config.Servers) != 0 {
		t.Fatal("invalid real initial state")
	}
	stream, err := s.Client.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	select {
	case item := <-stream.Items:
		if item.Err != nil || item.Event.Type != "core.snapshot" {
			t.Fatal(item)
		}
	case <-ctx.Done():
		t.Fatal("missing core snapshot")
	}
	// Seed isolated state through the same public API future CRUD will use.
	body := []byte(`{"alias":"fixture","host":"127.0.0.1","port":22,"user":"test","auth_method":"agent","tags":["生产"]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.Client.base+"/v1/config/servers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Client.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data Server `json:"data"`
	}
	decodeErr := json.NewDecoder(res.Body).Decode(&response)
	res.Body.Close()
	if decodeErr != nil || res.StatusCode != 201 || response.Data.ID == "" {
		t.Fatalf("fixture create status=%d err=%v", res.StatusCode, decodeErr)
	}
	fresh, err := Load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Client.Close()
	if len(fresh.Config.Servers) != 1 || fresh.Config.Servers[0].ID != response.Data.ID || fresh.Config.Servers[0].Alias != "fixture" {
		t.Fatal("public config not reflected")
	}
	resolved, err := fresh.Client.Resolve(ctx, "fixture")
	if err != nil || resolved.ID != response.Data.ID {
		t.Fatal("persistent resolution failed", err)
	}
	exerciseManagement(t, ctx, fresh.Client)
}
