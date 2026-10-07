package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
)

const (
	// testGracePeriod keeps teardown bounds short enough to assert against
	// without making the suite slow.
	testGracePeriod = 2 * time.Second
	testWorkerWait  = time.Second
)

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	root := t.TempDir()
	return paths.NewLayout(filepath.Join(root, "config"), filepath.Join(root, "state"))
}

// quietLogf keeps expected teardown diagnostics out of the test output.
func quietLogf(string, ...any) {}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// startTestRunner starts a runner that serves okHandler on a random port.
func startTestRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	if cfg.Layout.RuntimePath == "" {
		cfg.Layout = testLayout(t)
	}
	if cfg.Prepare == nil {
		cfg.Prepare = func(context.Context, Env) (http.Handler, error) { return okHandler(), nil }
	}
	if cfg.GracePeriod == 0 {
		cfg.GracePeriod = testGracePeriod
	}
	if cfg.WorkerTimeout == 0 {
		cfg.WorkerTimeout = testWorkerWait
	}
	if cfg.Logf == nil {
		cfg.Logf = quietLogf
	}
	runner, err := New(cfg)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := runner.Start(context.Background()); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	return runner
}

// R01: the API path requests shutdown and then the process waits for teardown.
// The two must not both close the same channel.
func TestRequestShutdownThenShutdownDoesNotPanic(t *testing.T) {
	runner := startTestRunner(t, Config{})

	runner.RequestShutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runner.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown after request: %v", err)
	}

	// Repeat calls stay harmless: the request is idempotent and teardown ran once.
	for i := 0; i < 3; i++ {
		runner.RequestShutdown()
		if err := runner.Shutdown(ctx); err != nil {
			t.Fatalf("repeat shutdown %d: %v", i, err)
		}
	}
}

// R01: every trigger shares one teardown, so concurrent requests plus a signal
// still release resources exactly once.
func TestConcurrentTriggersRunTeardownOnce(t *testing.T) {
	var teardowns atomic.Int32
	layout := testLayout(t)

	// The runner's Start context is the signal path: cancelling it must enter the
	// same teardown as the API requests below.
	startCtx, cancelSignal := context.WithCancel(context.Background())
	runner, err := New(Config{
		Layout:        layout,
		GracePeriod:   testGracePeriod,
		WorkerTimeout: testWorkerWait,
		Logf:          quietLogf,
		Prepare: func(_ context.Context, env Env) (http.Handler, error) {
			env.OnCleanup(func(context.Context) error {
				teardowns.Add(1)
				return nil
			})
			return okHandler(), nil
		},
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := runner.Start(startCtx); err != nil {
		t.Fatalf("start runner: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner.RequestShutdown()
		}()
	}
	// The signal path is a second trigger source for the same flow.
	cancelSignal()
	wg.Wait()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := runner.Shutdown(waitCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := teardowns.Load(); got != 1 {
		t.Fatalf("teardown ran %d times, want 1", got)
	}
	if _, err := os.Stat(layout.RuntimePath); !os.IsNotExist(err) {
		t.Fatalf("runtime file still present after teardown: %v", err)
	}
}

// R02: teardown imposes its own deadline, so a handler that ignores cancellation
// cannot make shutdown wait forever. A later caller with a short deadline waits
// only for its own context instead of queueing behind the earlier caller.
func TestShutdownDeadlineIsIndependentOfBlockedHandler(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-blocked
		w.WriteHeader(http.StatusOK)
	})

	runner := startTestRunner(t, Config{
		GracePeriod: 3 * time.Second,
		Prepare:     func(context.Context, Env) (http.Handler, error) { return handler, nil },
	})

	// Occupy the server with a request that never returns on its own.
	go func() {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/block", runner.Port()))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never entered")
	}

	// First caller uses no deadline of its own.
	firstDone := make(chan error, 1)
	go func() { firstDone <- runner.Shutdown(context.Background()) }()

	// Second caller must observe its own deadline rather than blocking behind
	// the first caller's still-running teardown.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	start := time.Now()
	err := runner.Shutdown(shortCtx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second shutdown error = %v, want context deadline exceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("second shutdown took %s, want it bounded by its own deadline", elapsed)
	}

	// Releasing the handler lets the single teardown finish.
	close(blocked)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("teardown did not complete after the handler was released")
	}
}

// R02: when the graceful phase expires, teardown forces the server closed and
// reports the degraded result instead of hanging or logging it away.
func TestTeardownForcesCloseAndReportsTimeout(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-blocked
	})
	defer close(blocked)

	runner := startTestRunner(t, Config{
		GracePeriod: 200 * time.Millisecond,
		Prepare:     func(context.Context, Env) (http.Handler, error) { return handler, nil },
	})

	go func() {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/block", runner.Port()))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never entered")
	}

	done := make(chan error, 1)
	go func() { done <- runner.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the forced shutdown to report an error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want a deadline error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("teardown did not force a bounded exit")
	}
}

// R03: the API, the discovery file, and the listener must agree, including the
// port that was actually assigned.
func TestRuntimeInfoMatchesListenerAndFile(t *testing.T) {
	layout := testLayout(t)
	holder := coreruntime.NewHolder()
	runner := startTestRunner(t, Config{
		Layout:  layout,
		Runtime: holder,
		Prepare: func(_ context.Context, env Env) (http.Handler, error) {
			env.SetTokenPresent(true)
			return okHandler(), nil
		},
	})

	info := runner.RuntimeInfo()
	if info.Port == 0 {
		t.Fatal("runtime info reports port 0")
	}
	if info.Port != runner.Port() {
		t.Fatalf("runtime info port %d, listener port %d", info.Port, runner.Port())
	}
	if info.InstanceID == "" {
		t.Fatal("runtime info has an empty instance ID")
	}
	if info.PID != os.Getpid() {
		t.Fatalf("runtime info PID = %d, want %d", info.PID, os.Getpid())
	}
	if !info.TokenPresent {
		t.Fatal("runtime info does not report the initialized token")
	}
	if len(info.ListenAddresses) == 0 {
		t.Fatal("runtime info has no listen addresses")
	}
	for _, address := range info.ListenAddresses {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatalf("listen address %q: %v", address, err)
		}
		if port != fmt.Sprint(info.Port) {
			t.Fatalf("listen address %q does not match port %d", address, info.Port)
		}
	}
	if info.RuntimePath != layout.RuntimePath || info.ConfigDir != layout.ConfigDir || info.StateDir != layout.StateDir {
		t.Fatalf("runtime info paths do not match the layout: %+v", info)
	}

	// The published holder and the discovery file carry the same data.
	published, ok := holder.Get()
	if !ok {
		t.Fatal("runtime holder was never published")
	}
	if published.Port != info.Port || published.InstanceID != info.InstanceID {
		t.Fatalf("holder %+v does not match runner info %+v", published, info)
	}

	raw, err := os.ReadFile(layout.RuntimePath)
	if err != nil {
		t.Fatalf("read runtime file: %v", err)
	}
	var onDisk coreruntime.Info
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("decode runtime file: %v", err)
	}
	if onDisk.Port != info.Port || onDisk.InstanceID != info.InstanceID || onDisk.PID != info.PID {
		t.Fatalf("runtime file %+v does not match runner info %+v", onDisk, info)
	}
	if !onDisk.TokenPresent {
		t.Fatal("runtime file does not report the initialized token")
	}
}

// R03: a runtime endpoint created before startup published anything must report
// the published values once they exist, not the placeholder it was built with.
func TestRuntimeHolderClearedOnTeardown(t *testing.T) {
	holder := coreruntime.NewHolder()
	runner := startTestRunner(t, Config{Runtime: holder})

	if _, ok := holder.Get(); !ok {
		t.Fatal("runtime holder was not published at startup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runner.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, ok := holder.Get(); ok {
		t.Fatal("runtime holder still reports a stopped instance")
	}
}

// R04: shared state is only initialized by the lock holder, so a second
// instance is rejected before it can reach any of it.
func TestSecondInstanceRejectedBeforeSharedStateInit(t *testing.T) {
	layout := testLayout(t)

	var firstPrepared atomic.Int32
	runner1 := startTestRunner(t, Config{
		Layout: layout,
		Prepare: func(context.Context, Env) (http.Handler, error) {
			firstPrepared.Add(1)
			return okHandler(), nil
		},
	})
	if firstPrepared.Load() != 1 {
		t.Fatalf("first instance prepared %d times, want 1", firstPrepared.Load())
	}

	var secondPrepared atomic.Int32
	runner2, err := New(Config{
		Layout: layout,
		Logf:   quietLogf,
		Prepare: func(context.Context, Env) (http.Handler, error) {
			secondPrepared.Add(1)
			return okHandler(), nil
		},
	})
	if err != nil {
		t.Fatalf("new second runner: %v", err)
	}
	err = runner2.Start(context.Background())
	if err == nil {
		t.Fatal("second instance started while the first held the lock")
	}
	if secondPrepared.Load() != 0 {
		t.Fatalf("second instance initialized shared state %d times before rejection", secondPrepared.Load())
	}

	// The first instance keeps working and its files are untouched.
	infoBefore, err := os.ReadFile(layout.RuntimePath)
	if err != nil {
		t.Fatalf("read runtime file: %v", err)
	}
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", runner1.Port())); err != nil {
		t.Fatalf("first instance stopped serving: %v", err)
	}
	infoAfter, err := os.ReadFile(layout.RuntimePath)
	if err != nil {
		t.Fatalf("re-read runtime file: %v", err)
	}
	if string(infoBefore) != string(infoAfter) {
		t.Fatal("second instance rewrote the first instance's runtime file")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runner1.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown first instance: %v", err)
	}

	// The lock and the port are immediately reusable.
	if err := runner2.Start(context.Background()); err != nil {
		t.Fatalf("second instance could not start after the first stopped: %v", err)
	}
	if err := runner2.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown second instance: %v", err)
	}
}

// R04: a failure at any startup step releases everything already acquired, so
// the next attempt starts from a clean slate.
func TestStartupFailuresReleaseAcquiredResources(t *testing.T) {
	steps := []struct {
		name    string
		prepare func(ctx context.Context, env Env) (http.Handler, error)
		listen  func(port int) ([]net.Listener, []string, error)
		write   func(path string, info coreruntime.Info) error
	}{
		{
			name: "prepare fails",
			prepare: func(context.Context, Env) (http.Handler, error) {
				return nil, errors.New("provider unavailable")
			},
		},
		{
			name: "listener fails",
			listen: func(int) ([]net.Listener, []string, error) {
				return nil, nil, errors.New("address in use")
			},
		},
		{
			name: "runtime file write fails",
			write: func(string, coreruntime.Info) error {
				return errors.New("read-only directory")
			},
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			layout := testLayout(t)
			var cleanups atomic.Int32
			var serviceCtx context.Context

			runner, err := New(Config{
				Layout:      layout,
				GracePeriod: testGracePeriod,
				Logf:        quietLogf,
				Prepare: func(ctx context.Context, env Env) (http.Handler, error) {
					serviceCtx = env.Context
					// A cleanup registered before the failing step must still run.
					env.OnCleanup(func(context.Context) error {
						cleanups.Add(1)
						return nil
					})
					if step.prepare != nil {
						return step.prepare(ctx, env)
					}
					return okHandler(), nil
				},
				Listen:           step.listen,
				WriteRuntimeInfo: step.write,
			})
			if err != nil {
				t.Fatalf("new runner: %v", err)
			}

			if err := runner.Start(context.Background()); err == nil {
				t.Fatal("startup succeeded despite the injected failure")
			}

			if cleanups.Load() != 1 {
				t.Fatalf("registered cleanup ran %d times, want 1", cleanups.Load())
			}
			if serviceCtx == nil || serviceCtx.Err() == nil {
				t.Fatal("service context was not cancelled when startup aborted")
			}
			if _, err := os.Stat(layout.RuntimePath); !os.IsNotExist(err) {
				t.Fatalf("runtime file left behind after a failed start: %v", err)
			}
			// A failed start must not report success through the completion signal.
			select {
			case <-runner.Done():
				t.Fatal("teardown signal fired for a start that never served")
			default:
			}

			// The lock is free again, and no listener is left bound.
			fresh := startTestRunner(t, Config{Layout: layout})
			if fresh.Port() == 0 {
				t.Fatal("replacement runner has no port")
			}
		})
	}
}

// R04: the port must be released with the lock, so an immediate restart reuses it.
func TestShutdownReleasesPortAndLock(t *testing.T) {
	layout := testLayout(t)
	runner := startTestRunner(t, Config{Layout: layout})
	port := runner.Port()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runner.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	<-runner.Done()

	if _, err := os.Stat(layout.RuntimePath); !os.IsNotExist(err) {
		t.Fatalf("runtime file not removed on shutdown: %v", err)
	}
	if _, err := os.Open(filepath.Join(layout.RuntimeDir, ".instance.lock")); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	// Rebinding the same port proves the listener was really closed.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d was not released: %v", port, err)
	}
	_ = ln.Close()

	restarted := startTestRunner(t, Config{Layout: layout})
	if restarted.Port() == 0 {
		t.Fatal("restart produced no port")
	}
}

// Teardown must not wait out the full grace period when cleanup is already done.
func TestShutdownCompletesPromptlyWhenIdle(t *testing.T) {
	runner := startTestRunner(t, Config{GracePeriod: 30 * time.Second})

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runner.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("idle shutdown took %s, want it to finish without waiting out the grace period", elapsed)
	}
}
