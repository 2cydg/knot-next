package corestart

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/preferences"
)

type controlledProcess struct{ done chan struct{} }

func (p controlledProcess) Done() <-chan struct{} { return p.done }
func testSettings() preferences.Core {
	return preferences.Core{URL: preferences.DefaultCoreURL, Secret: "private", AutoStart: true, Executable: "core with spaces", ConfigFile: "unreadable core.toml"}
}
func TestProbeFailuresNeverRestartHealthyOrAmbiguousTargets(t *testing.T) {
	for _, code := range []string{"", "UNAUTHORIZED", "PROTOCOL_ERROR", "API_UNSUPPORTED", "TIMEOUT", "OFFLINE", "UNREACHABLE"} {
		t.Run(code, func(t *testing.T) {
			var starts atomic.Int32
			coordinator := New(Dependencies{Probe: func(context.Context, preferences.Core) error {
				if code == "" {
					return nil
				}
				return coreclient.Failure(code)
			}, Check: func(string) error { return nil }, Start: func(preferences.Core) (Process, error) {
				starts.Add(1)
				return controlledProcess{make(chan struct{})}, nil
			}, Wait: 5 * time.Millisecond, Interval: time.Millisecond})
			settings := testSettings()
			settings.AutoStart = code != "UNREACHABLE"
			err := coordinator.Ensure(context.Background(), settings)
			if coreclient.Code(err) != code && !(err == nil && code == "") {
				t.Fatal("unexpected probe result", err)
			}
			if starts.Load() != 0 {
				t.Fatal("unexpected start")
			}
		})
	}
}
func TestConcurrentRequestsShareOneStartAndCompetitorCanWin(t *testing.T) {
	var available atomic.Bool
	var starts atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	close(exited)
	coordinator := New(Dependencies{Check: func(string) error { return nil }, Probe: func(context.Context, preferences.Core) error {
		if available.Load() {
			return nil
		}
		return coreclient.Failure("UNREACHABLE")
	}, Start: func(preferences.Core) (Process, error) {
		starts.Add(1)
		close(started)
		<-release
		return controlledProcess{exited}, nil
	}, Wait: time.Second, Interval: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() { defer workers.Done(); results <- coordinator.Ensure(ctx, testSettings()) }()
	}
	<-started
	available.Store(true)
	close(release)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 {
		t.Fatal("duplicate process start", starts.Load())
	}
	// A crash after a successful attempt may be polled, but never restarted in a loop.
	available.Store(false)
	if err := coordinator.Ensure(ctx, testSettings()); coreclient.Code(err) != "UNREACHABLE" {
		t.Fatal("past success hid new outage", err)
	}
	if starts.Load() != 1 {
		t.Fatal("restarted crashed service")
	}
}
func TestStartExitTimeoutAndCancellation(t *testing.T) {
	for _, mode := range []string{"exit", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var starts atomic.Int32
			done := make(chan struct{})
			if mode == "exit" {
				close(done)
			}
			coordinator := New(Dependencies{Check: func(string) error { return nil }, Probe: func(context.Context, preferences.Core) error { return coreclient.Failure("UNREACHABLE") }, Start: func(preferences.Core) (Process, error) { starts.Add(1); return controlledProcess{done}, nil }, Wait: 10 * time.Millisecond, Interval: time.Millisecond})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			err := coordinator.Ensure(ctx, testSettings())
			want := map[string]string{"exit": "CORE_START_FAILED", "timeout": "OFFLINE", "cancel": "CANCELED"}[mode]
			if coreclient.Code(err) != want {
				t.Fatal(mode, err)
			}
			if mode != "cancel" {
				_ = coordinator.Ensure(ctx, testSettings())
				if starts.Load() != 1 {
					t.Fatal("repeated start after failure")
				}
			}
		})
	}
}
func TestResolvePathsDoesNotReadConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	selected := testSettings()
	selected.Executable = "~/bin path/core"
	selected.ConfigFile = "missing/config.toml"
	resolved, err := ResolvePaths(selected, home)
	if err != nil || resolved.Executable != filepath.Join(home, "bin path", "core") || resolved.ConfigFile != filepath.Join(home, "missing", "config.toml") {
		t.Fatal("paths not fixed to launch directory", err)
	}
	// Even a directory at the config path is passed untouched to core.
	if err := os.MkdirAll(resolved.ConfigFile, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePaths(selected, home); err != nil {
		t.Fatal("client inspected core configuration", err)
	}
}
func TestProbeValidatesVersionAndOptionalSecret(t *testing.T) {
	for _, body := range []string{`{"data":{"version":"core","api_version":"v1"}}`, `{"data":{"version":"core","api_version":"v2"}}`, `{"data":{}}`, `hello`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Error("empty Secret sent authentication header")
			}
			if r.URL.Path == "/v1/version" {
				_, _ = w.Write([]byte(body))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"instance_id":"instance","api_version":"v1"}}`))
		}))
		err := Probe(context.Background(), preferences.Core{URL: server.URL})
		server.Close()
		want := "PROTOCOL_ERROR"
		if body == `{"data":{"version":"core","api_version":"v1"}}` {
			want = ""
		}
		if body == `{"data":{"version":"core","api_version":"v2"}}` {
			want = "API_UNSUPPORTED"
		}
		if (want == "" && err != nil) || (want != "" && coreclient.Code(err) != want) {
			t.Fatal("unexpected protocol result", err)
		}
	}
}
