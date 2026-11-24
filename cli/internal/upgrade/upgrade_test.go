package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"knot-cli/internal/corestart"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func TestMain(m *testing.M) {
	if code, handled := RunHelper(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if len(os.Args) == 4 && os.Args[1] == "--test-apply" {
		stage, backup := os.Args[2], os.Args[3]
		console := commandIO{tr: i18n.New(i18n.English), out: os.Stdout, diagnostics: os.Stderr}
		os.Exit(console.installPrepared(transaction{dir: filepath.Dir(stage), stage: stage, backup: backup}, "v2.0.0"))
	}
	if len(os.Args) == 2 && os.Getenv("KNOT_UPGRADE_TEST_CORE") != "" {
		switch os.Args[1] {
		case "--version":
			fmt.Println("knot-core", os.Getenv("KNOT_UPGRADE_TEST_CORE"))
			return
		case "--test-core":
			serveTestCore()
			return
		}
	}
	os.Exit(m.Run())
}

func serveTestCore() {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	done := make(chan struct{})
	mux := http.NewServeMux()
	var probes atomic.Int32
	mux.HandleFunc("GET /v1/runtime", func(w http.ResponseWriter, _ *http.Request) {
		instance := "test"
		if probes.Add(1) > 1 && os.Getenv("KNOT_UPGRADE_TEST_REPLACED") == "1" {
			instance = "new"
		}
		fmt.Fprintf(w, `{"data":{"api_version":"v1","instance_id":%q,"pid":%d}}`, instance, os.Getpid())
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"shutdown":true}}`)
		w.(http.Flusher).Flush()
		close(done)
	})
	go http.Serve(listener, mux)
	fmt.Println("http://" + listener.Addr().String())
	<-done
	// Model asynchronous teardown after the HTTP response.
	time.Sleep(200 * time.Millisecond)
}

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestUpgradeConfirmationAndInstallation(t *testing.T) {
	for _, tc := range []struct {
		name, current, latest string
		confirm, corrupt      bool
		replaced              bool
		want                  int
	}{
		{name: "current", current: "1.0.0", latest: "v1.0.0"},
		{name: "declined", current: "1.0.0", latest: "v1.1.0"},
		{name: "corrupt", current: "1.0.0", latest: "v1.1.0", confirm: true, corrupt: true, want: entry.ExitFailure},
		{name: "instance changed", current: "1.0.0", latest: "v1.1.0", confirm: true, replaced: true, want: entry.ExitFailure},
		{name: "success", current: "1.0.0", latest: "v1.1.0", confirm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tc.name == "success" {
				t.Skip("the production helper waits for its parent CLI process to exit")
			}
			home, dir := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("KNOT_UPGRADE_TEST_CORE", tc.current)
			t.Setenv("KNOT_UPGRADE_TEST_REPLACED", "")
			if tc.replaced {
				t.Setenv("KNOT_UPGRADE_TEST_REPLACED", "1")
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			core := filepath.Join(dir, binaryNames[0])
			if err := copyBinary(binary, core); err != nil {
				t.Fatal(err)
			}
			cli := filepath.Join(dir, binaryNames[1])
			if err := os.WriteFile(cli, []byte("old CLI"), 0755); err != nil {
				t.Fatal(err)
			}
			pair, err := inspectInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}

			var stopped chan error
			if tc.current != strings.TrimPrefix(tc.latest, "v") {
				stopped = startTestCore(t, core)
			}
			cliAsset, coreAsset := assetName("cli"), assetName("core")
			checksums := fmt.Sprintf("%x  %s\n%x  %s\n", sha256.Sum256([]byte("new CLI")), cliAsset, sha256.Sum256([]byte("new Core")), coreAsset)
			metadata, err := json.Marshal(map[string]any{
				"tag_name": tc.latest, "assets": []map[string]string{{"name": "checksums.txt"}, {"name": coreAsset}, {"name": cliAsset}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var confirmed atomic.Bool
			var assets []string
			releases := source{api: "https://test.invalid/latest", downloads: "https://test.invalid/releases", client: &http.Client{
				Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
					name := filepath.Base(req.URL.Path)
					values := map[string]string{"latest": string(metadata), "checksums.txt": checksums, cliAsset: "new CLI", coreAsset: "new Core"}
					if name != "latest" {
						if req.URL.Path != "/releases/download/"+tc.latest+"/"+name || !confirmed.Load() {
							t.Errorf("download before confirmation or unpinned download: %s", req.URL.Path)
						}
						assets = append(assets, name)
						select {
						case <-stopped:
							t.Error("Core stopped before the assets were downloaded")
						default:
						}
					}
					if tc.corrupt && name == cliAsset {
						values[name] = "corrupted CLI"
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(values[name]))}, nil
				}),
			}}
			var out, diagnostics bytes.Buffer
			console := commandIO{tr: i18n.New(i18n.Chinese), out: &out, diagnostics: &diagnostics}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			code := console.run(ctx, pair, tc.current, releases, func(context.Context) (bool, error) {
				if !strings.Contains(out.String(), "并中断进行中的传输") {
					t.Errorf("core warning missing: %q", out.String())
				}
				confirmed.Store(true)
				return tc.confirm, nil
			})
			if code != tc.want {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &out, &diagnostics)
			}
			if tc.confirm != (len(assets) == 3) {
				t.Fatalf("downloads=%v", assets)
			}
			if _, err := os.Stat(filepath.Join(dir, corestart.InstallLockName)); !os.IsNotExist(err) {
				t.Fatalf("upgrade lock not released: %v", err)
			}
			if tc.name != "success" {
				if err := pair.checkUnchanged(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-stopped:
					t.Fatal("Core was stopped after confirmation was declined or the upgrade failed")
				default:
				}
				if tc.name == "current" && (confirmed.Load() || !strings.Contains(out.String(), "已是最新版本")) {
					t.Fatalf("unexpected up-to-date output: %q", &out)
				}
				return
			}
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Core did not exit")
			}
			assertContents(t, core, "new Core")
			assertContents(t, cli, "new CLI")
			backups, err := filepath.Glob(filepath.Join(dir, backupPrefix+"*"))
			if err != nil || len(backups) != 1 {
				t.Fatalf("backups=%v err=%v", backups, err)
			}
			assertContents(t, filepath.Join(backups[0], binaryNames[1]), "old CLI")
		})
	}
}

func startTestCore(t *testing.T, path string) chan error {
	t.Helper()
	cmd := exec.Command(path, "--test-core")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()
	if err := preferences.Save(preferences.Settings{Core: &preferences.Core{URL: strings.TrimSpace(line)}}); err != nil {
		t.Fatal(err)
	}
	return stopped
}

func TestPairReplacementRestoresCoreIfCLIReplacementFails(t *testing.T) {
	dir := t.TempDir()
	for _, name := range binaryNames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old "+name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	pair, err := inspectInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pair.prepare()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.cleanup()
	if _, err := pair.prepare(); !os.IsExist(err) {
		t.Fatalf("concurrent upgrade acquired the installation lock: %v", err)
	}
	if err := tx.backUp(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tx.stage, binaryNames[0]), []byte("new Core"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := tx.replace(); err == nil {
		t.Fatal("replacement succeeded without a staged CLI")
	}
	for _, name := range binaryNames {
		assertContents(t, filepath.Join(dir, name), "old "+name)
		assertContents(t, filepath.Join(tx.backup, name), "old "+name)
	}
}

func TestInstallReplacesRunningCLI(t *testing.T) {
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	core, cli := filepath.Join(dir, binaryNames[0]), filepath.Join(dir, binaryNames[1])
	if err := copyBinary(binary, cli); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core, []byte("old Core"), 0755); err != nil {
		t.Fatal(err)
	}
	pair, err := inspectInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pair.prepare()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.cleanup()
	if err := tx.backUp(); err != nil {
		t.Fatal(err)
	}
	for _, name := range binaryNames {
		if err := os.WriteFile(filepath.Join(tx.stage, name), []byte("new "+name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "--test-apply", tx.stage, tx.backup)
	cmd.WaitDelay = 35 * time.Second
	result, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(result, []byte("upgraded to v2.0.0")) {
		t.Fatalf("self-replacement: %s, %v", result, err)
	}
	for _, name := range binaryNames {
		assertContents(t, filepath.Join(dir, name), "new "+name)
	}
}

func assertContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s: got %q, err=%v, want %q", path, data, err, want)
	}
}
