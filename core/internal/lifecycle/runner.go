package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	stdhttp "net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"knot-core/internal/fileutil"
	"knot-core/internal/logger"
	"knot-core/internal/paths"
	coreruntime "knot-core/internal/runtime"
	"knot-core/internal/transport"
	"log/slog"
)

const (
	// DefaultGracePeriod bounds the graceful phase of teardown: the HTTP server
	// stops accepting and in-flight requests are given this long to finish.
	DefaultGracePeriod = 10 * time.Second

	// DefaultWorkerTimeout bounds waiting for serve workers after the graceful
	// phase. Reaching it is reported as a shutdown error rather than ignored.
	DefaultWorkerTimeout = 5 * time.Second
)

// Cleanup releases one service-level resource during teardown.
type Cleanup func(ctx context.Context) error

// Env is handed to Config.Prepare once the single-instance lock is held.
type Env struct {
	// Layout is the resolved path layout, already ensured.
	Layout paths.Layout
	// InstanceID identifies this run in the discovery file.
	InstanceID string
	// Runtime receives the published discovery information.
	Runtime *coreruntime.Holder
	// Context is cancelled when teardown starts. Long-running workers created by
	// Prepare should stop when it is done.
	Context context.Context
	// RequestShutdown asks the runner to begin its single teardown flow. It is
	// safe to call repeatedly and from any goroutine.
	RequestShutdown func()
	// SetTokenPresent records whether authentication material actually exists,
	// so the discovery file reports the real state rather than a placeholder.
	SetTokenPresent func(present bool)
	// OnCleanup registers a teardown step. Steps run in reverse registration
	// order, both on a normal teardown and when startup aborts part-way, so a
	// partially started service releases exactly what it acquired.
	OnCleanup func(Cleanup)
}

// ServerConfig carries the HTTP server tuning the runner applies to the server
// it builds around the prepared handler.
type ServerConfig struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// Config holds runner configuration.
type Config struct {
	Layout       paths.Layout
	Port         int
	Version      string
	APIVersion   string
	StartedAt    time.Time
	TokenPresent bool

	// Runtime receives the discovery information published at startup.
	Runtime *coreruntime.Holder

	// GracePeriod and WorkerTimeout bound teardown. Zero selects the defaults.
	GracePeriod   time.Duration
	WorkerTimeout time.Duration

	// Server tunes the HTTP server the runner builds around the handler returned
	// by Prepare.
	Server ServerConfig

	// Prepare builds the HTTP handler. It runs after the instance lock is held
	// and before any listener exists, so the token, crypto state, and connection
	// pool are only ever touched by the lock holder.
	Prepare func(ctx context.Context, env Env) (stdhttp.Handler, error)

	// Listen is injectable so tests can force listener failures. Nil selects the
	// loopback-only listener.
	Listen func(port int) ([]net.Listener, []string, error)

	// WriteRuntimeInfo publishes the discovery file. It is injectable so tests
	// can force a publish failure and check that startup unwinds. Nil selects
	// the atomic on-disk writer.
	WriteRuntimeInfo func(path string, info coreruntime.Info) error

	// Logf receives lifecycle diagnostics. Nil selects the standard logger.
	Logf func(format string, args ...any)
}

// Runner manages the core service lifecycle.
//
// Shutdown is a single flow with two observable signals: RequestShutdown reports
// that teardown was asked for, and Done (or Wait) reports that teardown has
// finished. Every trigger — the authenticated API, SIGINT/SIGTERM, or a serve
// error — enters the same flow, so no trigger can close a shared channel twice
// or run the resources down twice.
type Runner struct {
	cfg        Config
	instanceID string

	instanceLock *fileutil.InstanceLock
	server       *stdhttp.Server
	listeners    []net.Listener
	port         int
	info         coreruntime.Info
	cleanups     []Cleanup

	serviceCtx    context.Context
	cancelService context.CancelFunc

	requestOnce sync.Once
	requestCh   chan struct{}

	doneOnce sync.Once
	doneCh   chan struct{}

	errMu       sync.Mutex
	shutdownErr error

	tokenMu      sync.Mutex
	tokenPresent bool

	workers sync.WaitGroup
}

// New creates a new lifecycle runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Version == "" {
		cfg.Version = "0.0.0"
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "v1"
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now().UTC()
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = DefaultGracePeriod
	}
	if cfg.WorkerTimeout <= 0 {
		cfg.WorkerTimeout = DefaultWorkerTimeout
	}
	if cfg.Listen == nil {
		cfg.Listen = transport.ListenLoopback
	}
	if cfg.WriteRuntimeInfo == nil {
		cfg.WriteRuntimeInfo = coreruntime.WriteInfo
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}

	instanceID, err := generateInstanceID()
	if err != nil {
		return nil, fmt.Errorf("generate instance ID: %w", err)
	}

	return &Runner{
		cfg:        cfg,
		instanceID: instanceID,
		requestCh:  make(chan struct{}),
		doneCh:     make(chan struct{}),
	}, nil
}

// Start acquires the instance lock, initializes shared state, and serves until a
// shutdown trigger arrives. A failure before serving releases everything it
// acquired and leaves the lock free for the next attempt.
func (r *Runner) Start(ctx context.Context) error {
	// The lock comes first: paths and flags were already resolved without
	// touching shared state, so a second instance is rejected before it can read
	// or rewrite the token, crypto state, or configuration.
	if err := os.MkdirAll(r.cfg.Layout.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("prepare runtime directory: %w", err)
	}
	lock, err := fileutil.AcquireInstanceLock(filepath.Join(r.cfg.Layout.RuntimeDir, ".instance.lock"))
	if err != nil {
		return fmt.Errorf("acquire instance lock: %w", err)
	}
	r.instanceLock = lock

	// Failures from here on unwind through cleanup, so nothing acquired is
	// leaked and the lock is released before the error is returned.
	if err := r.cfg.Layout.Ensure(); err != nil {
		r.cleanup()
		return fmt.Errorf("prepare paths: %w", err)
	}

	r.serviceCtx, r.cancelService = context.WithCancel(context.Background())
	r.tokenPresent = r.cfg.TokenPresent

	env := Env{
		Layout:          r.cfg.Layout,
		InstanceID:      r.instanceID,
		Runtime:         r.cfg.Runtime,
		Context:         r.serviceCtx,
		RequestShutdown: r.RequestShutdown,
		SetTokenPresent: r.setTokenPresent,
		OnCleanup:       r.registerCleanup,
	}
	if r.cfg.Prepare != nil {
		handler, err := r.cfg.Prepare(r.serviceCtx, env)
		if err != nil {
			r.abortStartup()
			return fmt.Errorf("prepare services: %w", err)
		}
		r.server = &stdhttp.Server{
			Handler:           handler,
			ReadHeaderTimeout: r.cfg.Server.ReadHeaderTimeout,
			ReadTimeout:       r.cfg.Server.ReadTimeout,
			WriteTimeout:      r.cfg.Server.WriteTimeout,
			IdleTimeout:       r.cfg.Server.IdleTimeout,
			MaxHeaderBytes:    r.cfg.Server.MaxHeaderBytes,
		}
	}

	listeners, addresses, err := r.cfg.Listen(r.cfg.Port)
	if err != nil {
		r.abortStartup()
		return fmt.Errorf("listen on loopback: %w", err)
	}
	r.listeners = listeners
	r.port = listenerPort(listeners[0], r.cfg.Port)

	// Discovery information is built from the real listener, not from the
	// requested port: a randomized port must be reported as the one in use.
	r.info = coreruntime.NewInfo(
		r.cfg.Version,
		r.cfg.APIVersion,
		r.instanceID,
		r.port,
		addresses,
		r.cfg.Layout,
		r.cfg.StartedAt,
		r.tokenAvailable(),
	)

	errCh := make(chan error, len(r.listeners))
	if r.server != nil {
		for _, ln := range r.listeners {
			r.workers.Add(1)
			go func(listener net.Listener) {
				defer r.workers.Done()
				if err := r.server.Serve(listener); err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
					select {
					case errCh <- err:
					default:
					}
				}
			}(ln)
		}
	}

	// Publish discovery only once the listeners are serving, so a client that
	// reads the file can reach the port and addresses it names.
	if err := r.cfg.WriteRuntimeInfo(r.cfg.Layout.RuntimePath, r.info); err != nil {
		if r.server != nil {
			_ = r.server.Close()
		}
		r.waitWorkers(r.cfg.WorkerTimeout)
		r.abortStartup()
		return fmt.Errorf("write runtime info: %w", err)
	}
	if r.cfg.Runtime != nil {
		r.cfg.Runtime.Set(r.info)
	}

	logger.Diagnostic(slog.LevelInfo, "core.ready", "instance_id", r.instanceID, "port", r.port, "log_path", r.cfg.Layout.LogPath)
	go r.supervise(ctx, errCh)
	return nil
}

// setTokenPresent records whether authentication material was initialized.
func (r *Runner) setTokenPresent(present bool) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	r.tokenPresent = present
}

func (r *Runner) tokenAvailable() bool {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	return r.tokenPresent
}

// supervise waits for the first shutdown trigger and then runs the single
// teardown flow to completion.
func (r *Runner) supervise(ctx context.Context, errCh <-chan error) {
	select {
	case <-ctx.Done():
	case err := <-errCh:
		r.recordError(fmt.Errorf("http server: %w", err))
	case <-r.requestCh:
	}
	r.teardown()
}

// Shutdown requests teardown and waits for it to finish, or for ctx to expire.
//
// Teardown has its own deadline (Config.GracePeriod); a caller's context bounds
// only how long this call waits for it. A later call therefore returns as soon
// as the flow it is waiting on finishes, and reports its own deadline rather
// than blocking behind an earlier caller's deadline.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.RequestShutdown()
	select {
	case <-r.doneCh:
		return r.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RequestShutdown asks the runner to begin its single teardown flow. It may be
// called repeatedly and concurrently: only the first call has an effect, and no
// caller owns a channel it could close twice.
func (r *Runner) RequestShutdown() {
	r.requestOnce.Do(func() {
		close(r.requestCh)
	})
}

// ShutdownRequested reports whether teardown has been asked for. It is closed as
// soon as a trigger arrives, before teardown finishes.
func (r *Runner) ShutdownRequested() <-chan struct{} {
	return r.requestCh
}

// Done reports that teardown has finished and all resources are released.
func (r *Runner) Done() <-chan struct{} {
	return r.doneCh
}

// Wait blocks until teardown is complete.
func (r *Runner) Wait() {
	<-r.doneCh
}

// Err reports the first error observed while serving or tearing down.
func (r *Runner) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.shutdownErr
}

// RuntimeInfo returns the discovery information published at startup.
func (r *Runner) RuntimeInfo() coreruntime.Info {
	return r.info
}

// Port returns the port the service actually listens on.
func (r *Runner) Port() int {
	return r.port
}

func (r *Runner) registerCleanup(fn Cleanup) {
	if fn == nil {
		return
	}
	r.cleanups = append(r.cleanups, fn)
}

// teardown runs exactly once, however many triggers fire.
func (r *Runner) teardown() {
	r.doneOnce.Do(func() {
		defer close(r.doneCh)
		r.runTeardown()
	})
}

func (r *Runner) runTeardown() {
	logger.Diagnostic(slog.LevelInfo, "core.stopping", "instance_id", r.instanceID)
	// Signal service-level workers first so pending connections and relays start
	// winding down while the HTTP server drains.
	if r.cancelService != nil {
		r.cancelService()
	}

	// Stop accepting new requests and give in-flight handlers a bounded chance
	// to finish. Hijacked connections are not tracked by Serve's own shutdown
	// and are closed explicitly by the registered cleanups below.
	if r.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), r.cfg.GracePeriod)
		err := r.server.Shutdown(ctx)
		cancel()
		if err != nil {
			r.recordError(fmt.Errorf("graceful http shutdown: %w", err))
			// A handler that ignores cancellation cannot be waited for. Force the
			// connections closed so teardown still completes.
			_ = r.server.Close()
		}
	}

	// Release services. Cleanups run in reverse registration order, each bounded
	// by the shared budget: a cleanup that ignores its context must not hold
	// teardown open, or the deadline would only be advisory.
	resourceCtx, cancelResources := context.WithTimeout(context.Background(), r.cfg.GracePeriod)
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		if err := runCleanup(resourceCtx, r.cleanups[i]); err != nil {
			r.recordError(fmt.Errorf("shutdown cleanup: %w", err))
		}
	}
	cancelResources()
	r.cleanups = nil

	r.waitWorkers(r.cfg.WorkerTimeout)

	// Only now are the runtime file and the instance lock released, so a new
	// instance can never start while the old one still has workers running.
	r.cleanup()
}

// runCleanup runs one cleanup step without letting a step that ignores its
// context hold teardown open.
//
// The cleanup still receives ctx, so a well-behaved one cancels its own work and
// returns promptly. When the budget runs out first the step is left to finish on
// its own and the expiry is reported: the release it performs is not complete,
// which the caller must be able to observe rather than read as a clean shutdown.
func runCleanup(ctx context.Context, fn Cleanup) error {
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("cleanup did not finish: %w", ctx.Err())
	}
}

// abortStartup unwinds a partial start: it cancels service work, runs whatever
// cleanups were registered before the failure, and releases the lock.
func (r *Runner) abortStartup() {
	logger.Diagnostic(slog.LevelError, "core.startup_aborted", "instance_id", r.instanceID)
	if r.cancelService != nil {
		r.cancelService()
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.GracePeriod)
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		if err := runCleanup(ctx, r.cleanups[i]); err != nil {
			r.recordError(fmt.Errorf("startup cleanup: %w", err))
		}
	}
	cancel()
	r.cleanups = nil
	r.cleanup()
}

// cleanup releases the resources the runner owns directly. It is idempotent so
// both the startup-abort path and teardown can call it.
func (r *Runner) cleanup() {
	for _, ln := range r.listeners {
		_ = ln.Close()
	}
	r.listeners = nil

	if r.cfg.Runtime != nil {
		r.cfg.Runtime.Clear()
	}

	// Remove runtime info (verify ownership first, so a file published by a
	// different instance is never deleted).
	if r.cfg.Layout.RuntimePath != "" {
		if err := coreruntime.ValidateOwnership(r.cfg.Layout.RuntimePath, r.instanceID); err == nil {
			_ = os.Remove(r.cfg.Layout.RuntimePath)
		}
	}

	if r.instanceLock != nil {
		_ = r.instanceLock.Release()
		r.instanceLock = nil
	}
}

// waitWorkers bounds how long teardown waits for serve workers to stop.
func (r *Runner) waitWorkers(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		r.workers.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		r.recordError(fmt.Errorf("serve workers did not stop within %s", timeout))
	}
}

// recordError keeps the first error, which is the one that caused teardown.
func (r *Runner) recordError(err error) {
	if err == nil {
		return
	}
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.shutdownErr == nil {
		r.shutdownErr = err
	}
	r.cfg.Logf("knot-core: %s", logger.Redact(err.Error()))
}

func listenerPort(ln net.Listener, fallback int) int {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return fallback
}

// generateInstanceID creates a unique instance identifier.
func generateInstanceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
