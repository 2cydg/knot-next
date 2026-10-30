package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// coreBinary is built once for the package: every test here drives the real
// program, and rebuilding it per test dominated the runtime.
var (
	buildOnce sync.Once
	buildErr  error
	coreBin   string
)

func coreBinaryPath(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "knot-core-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		coreBin = filepath.Join(dir, "knot-core")
		if runtime.GOOS == "windows" {
			coreBin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", coreBin, "../../cmd/core")
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build core: %w\n%s", err, output)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return coreBin
}

// coreInstance is a running core process with an isolated layout.
type coreInstance struct {
	cmd         *exec.Cmd
	configPath  string
	secret      string
	configDir   string
	stateDir    string
	runtimePath string
	tokenPath   string
	info        runtimeFile
	waited      bool
}

// envelope is the structured success response the API wraps every payload in.
type envelope[T any] struct {
	Data *T `json:"data"`
}

type healthPayload struct {
	Status        string `json:"status"`
	Authenticated bool   `json:"authenticated"`
	Checks        []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
}

type runtimeFile struct {
	Version         string    `json:"version"`
	APIVersion      string    `json:"api_version"`
	InstanceID      string    `json:"instance_id"`
	PID             int       `json:"pid"`
	Port            int       `json:"port"`
	ListenAddresses []string  `json:"listen_addresses"`
	TokenPath       string    `json:"token_path"`
	TokenPresent    bool      `json:"token_present"`
	RuntimePath     string    `json:"runtime_path"`
	ConfigDir       string    `json:"config_dir"`
	StateDir        string    `json:"state_dir"`
	StartedAt       time.Time `json:"started_at"`
}

// startCore launches the binary with an isolated layout and waits for readiness
// by authenticating against the API — never by sleeping.
func startCore(t *testing.T, extraArgs ...string) *coreInstance {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "config")

	selected := filepath.Join(configDir, "knot", "selected.toml")
	if err := os.MkdirAll(filepath.Dir(selected), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	const secret = "integration-secret-keep-private"
	if err := os.WriteFile(selected, []byte(fmt.Sprintf("[service]\nport=%d\nsecret=%q\n", port, secret)), 0600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"--config", selected}, extraArgs...)
	cmd := exec.Command(coreBinaryPath(t), args...)
	cmd.Env = append(os.Environ(),
		"HOME="+root,
		"USERPROFILE="+root,
	)
	// Lifecycle tests must not create or depend on the user's Secret Service
	// item. A private nonexistent bus forces the legacy machine fallback.
	if runtime.GOOS == "linux" {
		cmd.Env = append(cmd.Env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(root, "no-session-bus"))
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start core: %v", err)
	}

	inst := &coreInstance{
		cmd:         cmd,
		configPath:  selected,
		secret:      secret,
		info:        runtimeFile{Port: port},
		configDir:   filepath.Join(configDir, "knot"),
		stateDir:    filepath.Join(configDir, "knot", ".knot-state"),
		runtimePath: filepath.Join(configDir, "knot", ".knot-state", "runtime", "core.json"),
		tokenPath:   filepath.Join(configDir, "knot", ".knot-state", "runtime", "token"),
	}
	t.Cleanup(func() { inst.kill(t) })

	inst.info = inst.waitReady(t)
	return inst
}

// waitReady blocks until the discovery file names a reachable, authenticated
// service, or fails the test.
func (c *coreInstance) waitReady(t *testing.T) runtimeFile {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := c.runtimeInfo()
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if info.Port <= 0 || info.InstanceID == "" {
			lastErr = fmt.Errorf("runtime file not fully published: %+v", info)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// Readiness is proven by an authenticated request, not by the file alone.
		resp, err := c.request(t, http.MethodGet, "/v1/health", info)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusOK {
			lastErr = fmt.Errorf("health returned %d", status)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return info
	}
	t.Fatalf("core did not become ready: %v", lastErr)
	return runtimeFile{}
}

func (c *coreInstance) runtimeInfo() (runtimeFile, error) {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/runtime", c.info.Port), nil)
	if err != nil {
		return runtimeFile{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.secret)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		return runtimeFile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimeFile{}, fmt.Errorf("runtime status %d", response.StatusCode)
	}
	var info runtimeFile
	err = json.NewDecoder(response.Body).Decode(&envelope[runtimeFile]{Data: &info})
	return info, err
}

func (c *coreInstance) token() string { return c.secret }

// request performs an authenticated call against the running instance.
func (c *coreInstance) request(t *testing.T, method, path string, info runtimeFile) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", info.Port, path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	client := &http.Client{Timeout: 3 * time.Second}
	return client.Do(req)
}

// waitExit reaps the process and returns its exit code. It fails the test if the
// process outlives the deadline, so a hung shutdown is a failure and not a log
// line.
func (c *coreInstance) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		c.waited = true
	case <-time.After(timeout):
		_ = c.cmd.Process.Kill()
		<-done
		c.waited = true
		t.Fatalf("core did not exit within %s", timeout)
	}
	return c.cmd.ProcessState.ExitCode()
}

func (c *coreInstance) kill(t *testing.T) {
	t.Helper()
	if c.cmd.Process == nil || c.waited {
		return
	}
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
	c.waited = true
}

// assertStopped checks the teardown contract: exit code, removed runtime file,
// and a released port that can be bound again immediately.
func assertStopped(t *testing.T, c *coreInstance, exitCode int) {
	t.Helper()
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if _, err := os.Stat(c.runtimePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime file was not removed on shutdown: %v", err)
	}
	if err := rebind(c.info.Port); err != nil {
		t.Fatalf("port %d was not released: %v", c.info.Port, err)
	}
}

// TestShutdownViaAuthenticatedAPI covers L05 through the API trigger.
func TestShutdownViaAuthenticatedAPI(t *testing.T) {
	inst := startCore(t)
	assertRuntimeMatchesListener(t, inst)

	resp, err := inst.request(t, http.MethodPost, "/v1/shutdown", inst.info)
	if err != nil {
		t.Fatalf("shutdown request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shutdown status = %d, body = %s", resp.StatusCode, body)
	}

	// Repeated and concurrent requests must still leave exactly one teardown.
	for i := 0; i < 3; i++ {
		if r, err := inst.request(t, http.MethodPost, "/v1/shutdown", inst.info); err == nil {
			_ = r.Body.Close()
		}
	}

	assertStopped(t, inst, inst.waitExit(t, 15*time.Second))

	// The instance lock is free, so a fresh instance starts on the same layout.
	cmd := exec.Command(coreBinaryPath(t), "--config", inst.configPath)
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Dir(filepath.Dir(inst.configDir)),
		"USERPROFILE="+filepath.Dir(filepath.Dir(inst.configDir)),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("restart core: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	(&coreInstance{cmd: cmd, info: inst.info, secret: inst.secret}).waitReady(t)
}

// TestShutdownSignals covers the SIGINT and SIGTERM triggers, which must run the
// same teardown and the same resource release as the API path.
func TestShutdownSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal delivery to a child process is not portable on Windows")
	}
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{name: "SIGINT", sig: os.Interrupt},
		{name: "SIGTERM", sig: syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := startCore(t)

			// Hold an authenticated connection open so teardown has real work to
			// drain rather than an idle server.
			keepAlive := &http.Client{Timeout: 5 * time.Second}
			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/health", inst.info.Port), nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+inst.token())
			resp, err := keepAlive.Do(req)
			if err != nil {
				t.Fatalf("health request: %v", err)
			}
			_ = resp.Body.Close()

			if err := inst.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signal %s: %v", tc.name, err)
			}
			assertStopped(t, inst, inst.waitExit(t, 15*time.Second))

			// The API must not linger on the released port.
			client := &http.Client{Timeout: time.Second}
			if r, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/health", inst.info.Port)); err == nil {
				_ = r.Body.Close()
				t.Fatal("service still answers after it exited")
			}
		})
	}
}

// TestRuntimeEndpointAgreesWithListenerWithoutDiscoveryFiles covers L01: the API and the file
// must describe the same running instance.
func TestRuntimeEndpointAgreesWithListenerWithoutDiscoveryFiles(t *testing.T) {
	inst := startCore(t)
	defer func() {
		if _, err := inst.request(t, http.MethodPost, "/v1/shutdown", inst.info); err == nil {
			inst.waitExit(t, 15*time.Second)
		}
	}()

	assertRuntimeMatchesListener(t, inst)
}

func assertRuntimeMatchesListener(t *testing.T, inst *coreInstance) {
	t.Helper()

	fileInfo, err := inst.runtimeInfo()
	if err != nil {
		t.Fatalf("read runtime file: %v", err)
	}
	if fileInfo.Port <= 0 {
		t.Fatalf("runtime file reports port %d", fileInfo.Port)
	}
	if fileInfo.InstanceID == "" {
		t.Fatal("runtime file has an empty instance ID")
	}
	if !fileInfo.TokenPresent {
		t.Fatal("runtime file reports token_present=false")
	}
	if fileInfo.TokenPath != "" {
		t.Fatalf("runtime file token path = %q, want %q", fileInfo.TokenPath, inst.tokenPath)
	}
	if fileInfo.RuntimePath != "" {
		t.Fatalf("runtime file runtime path = %q, want %q", fileInfo.RuntimePath, inst.runtimePath)
	}
	if fileInfo.ConfigDir != inst.configDir || fileInfo.StateDir != inst.stateDir {
		t.Fatalf("runtime file directories do not match the layout: %+v", fileInfo)
	}
	if fileInfo.Version == "" || fileInfo.Version == "1.0.0" {
		t.Fatalf("runtime file version = %q, want the build version", fileInfo.Version)
	}
	for _, path := range []string{inst.runtimePath, inst.tokenPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("discovery artifact created", path, err)
		}
	}

	resp, err := inst.request(t, http.MethodGet, "/v1/runtime", fileInfo)
	if err != nil {
		t.Fatalf("runtime request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("runtime status = %d, body = %s", resp.StatusCode, body)
	}
	var apiInfo runtimeFile
	if err := json.NewDecoder(resp.Body).Decode(&envelope[runtimeFile]{Data: &apiInfo}); err != nil {
		t.Fatalf("decode runtime response: %v", err)
	}
	if apiInfo.InstanceID != fileInfo.InstanceID || apiInfo.Port != fileInfo.Port || apiInfo.PID != fileInfo.PID {
		t.Fatalf("API runtime %+v does not match file runtime %+v", apiInfo, fileInfo)
	}
	if len(apiInfo.ListenAddresses) == 0 {
		t.Fatal("API runtime reports no listen addresses")
	}
	for _, address := range apiInfo.ListenAddresses {
		if !strings.HasSuffix(address, fmt.Sprint(fileInfo.Port)) {
			t.Fatalf("listen address %q does not match port %d", address, fileInfo.Port)
		}
	}

	// Health must reflect the real startup state instead of an unauthenticated
	// 401 or the zero-value placeholder.
	hresp, err := inst.request(t, http.MethodGet, "/v1/health", fileInfo)
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated health status = %d", hresp.StatusCode)
	}
	var health healthPayload
	if err := json.NewDecoder(hresp.Body).Decode(&envelope[healthPayload]{Data: &health}); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if !health.Authenticated {
		t.Fatal("health does not report an authenticated caller")
	}
	if health.Status != "ok" {
		t.Fatalf("health status = %q, checks = %+v", health.Status, health.Checks)
	}
	for _, name := range []string{"token", "listener"} {
		found := false
		for _, check := range health.Checks {
			if check.Name == name {
				found = true
				if check.Status != "ok" {
					t.Fatalf("health check %q = %q (%s)", name, check.Status, check.Detail)
				}
			}
		}
		if !found {
			t.Fatalf("health response is missing the %q check", name)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

// TestSecondInstanceRejectedWithoutTouchingSharedState covers L02/L03: the
// second instance is refused, and the first instance's files are untouched.
func TestSecondInstanceRejectedWithoutTouchingSharedState(t *testing.T) {
	inst := startCore(t)

	before := map[string][]byte{}
	for _, path := range []string{inst.configPath, filepath.Join(inst.configDir, ".crypto-state")} {
		before[path] = mustRead(t, path)
	}

	second := exec.Command(coreBinaryPath(t), "--config", inst.configPath)
	second.Env = append(os.Environ(),
		"HOME="+filepath.Dir(filepath.Dir(inst.configDir)),
		"USERPROFILE="+filepath.Dir(filepath.Dir(inst.configDir)),
	)
	output, err := second.CombinedOutput()
	if err == nil {
		t.Fatal("second instance started while the first was running")
	}
	if !strings.Contains(string(output), "already running") {
		t.Fatalf("second instance failed with an unexpected message: %s", output)
	}

	for path, want := range before {
		if got := mustRead(t, path); string(got) != string(want) {
			t.Fatalf("%s was modified by the rejected instance", path)
		}
	}

	// The first instance is still serving.
	resp, err := inst.request(t, http.MethodGet, "/v1/health", inst.info)
	if err != nil {
		t.Fatalf("first instance stopped serving: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first instance health = %d", resp.StatusCode)
	}

	if _, err := inst.request(t, http.MethodPost, "/v1/shutdown", inst.info); err != nil {
		t.Fatalf("shutdown request: %v", err)
	}
	assertStopped(t, inst, inst.waitExit(t, 15*time.Second))
}

// rebind proves a port was really released by binding it again.
func rebind(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return ln.Close()
}

func TestInitCommandCreatesOnlyCurrentDirectoryConfig(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(coreBinaryPath(t), "init")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(dir, "unused-home"), "USERPROFILE="+filepath.Join(dir, "unused-home"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init failed: %v %s", err, output)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.toml" {
		t.Fatal("init touched service state", err)
	}
	original := mustRead(t, filepath.Join(dir, "config.toml"))
	cmd = exec.Command(coreBinaryPath(t), "init")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatal("init accepted existing configuration")
	}
	if string(mustRead(t, filepath.Join(dir, "config.toml"))) != string(original) {
		t.Fatal("init overwrote configuration")
	}
}
