package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"knot-core/internal/keyutil"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"

	"golang.org/x/crypto/ssh"
)

const maxExecOutput = 512 * 1024

// result is committed before done closes. Shutdown snapshots the operation,
// so a subsequent history prune cannot erase the cleanup result it awaits.
type execOperation struct {
	done   chan struct{}
	result Exec
}

var (
	errExecShutdown    = errors.New("exec service is shutting down")
	execCleanupGrace   = 5 * time.Second
	execSignalGrace    = 100 * time.Millisecond
	execChannelTimeout = 15 * time.Second
)

// UseContext binds exec to the process lifecycle. It must be called during
// service setup, before serving requests. Cancellation is permanent.
func (s *Service) UseContext(ctx context.Context) {
	context.AfterFunc(ctx, s.CancelExec)
}

// CancelExec stops accepting exec requests and cancels active operations.
func (s *Service) CancelExec() { s.execCancel(errExecShutdown) }

// ShutdownExec waits for actual workers, including any pending channel open or
// teardown, rather than merely waiting for their callers to return.
func (s *Service) ShutdownExec(ctx context.Context) error {
	s.CancelExec()
	s.mu.RLock()
	active := make(map[string]*execOperation, len(s.execActive))
	for id, done := range s.execActive {
		active[id] = done
	}
	s.mu.RUnlock()
	var errs []error
	for id, operation := range active {
		select {
		case <-operation.done:
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		}
		result := operation.result
		if result.CleanupError != "" {
			errs = append(errs, fmt.Errorf("%s: %s", id, result.CleanupError))
		}
	}
	return errors.Join(errs...)
}

// Exec is the compatibility entry point for callers without a context.
func (s *Service) Exec(req ExecRequest) (Exec, error) {
	return s.ExecContext(context.Background(), req)
}

// ExecContext synchronously executes a command under caller, service and total
// operation deadlines. A nonzero remote exit status is a completed result.
func (s *Service) ExecContext(ctx context.Context, req ExecRequest) (Exec, error) {
	if strings.TrimSpace(req.ServerRef) == "" {
		return Exec{}, fmt.Errorf("%w: server_ref is required", ErrValidation)
	}
	if strings.TrimSpace(req.Command) == "" {
		return Exec{}, fmt.Errorf("%w: command is required", ErrValidation)
	}
	if req.TimeoutMS < 0 || req.TimeoutMS > math.MaxInt64/int64(time.Millisecond) {
		return Exec{}, fmt.Errorf("%w: timeout_ms is out of range", ErrValidation)
	}
	if req.HostKeyPolicy == "ask" {
		req.HostKeyPolicy = sshpool.HostKeyPolicyAsk
	}
	switch req.HostKeyPolicy {
	case sshpool.HostKeyPolicyAsk, sshpool.HostKeyPolicyFail, sshpool.HostKeyPolicyStrict,
		sshpool.HostKeyPolicyAcceptNew, sshpool.HostKeyPolicyInsecureSkip:
	default:
		return Exec{}, fmt.Errorf("%w: invalid host_key_policy", ErrValidation)
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	stopService := context.AfterFunc(s.execCtx, func() { cancel(errExecShutdown) })
	defer stopService()
	defer cancel(nil)
	if req.TimeoutMS > 0 {
		var stop context.CancelFunc
		runCtx, stop = context.WithTimeout(runCtx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer stop()
	}
	now := time.Now().UTC()
	s.mu.Lock()
	if len(s.execActive) >= s.execPolicy.Active {
		s.mu.Unlock()
		return Exec{}, fmt.Errorf("%w: exec limit reached", ErrConflict)
	}
	if s.stopped || s.execCtx.Err() != nil {
		s.mu.Unlock()
		return Exec{}, fmt.Errorf("%w: exec service is shutting down", ErrConflict)
	}
	id := "exec_" + strconv.FormatInt(s.nextID, 10)
	s.nextID++
	operation := &execOperation{done: make(chan struct{})}
	s.execActive[id] = operation
	s.mu.Unlock()
	result := Exec{ID: id, ServerRef: req.ServerRef, Command: req.Command, State: "running", StartedAt: now}
	var settled <-chan struct{}
	defer func() {
		finish := func() {
			s.mu.Lock()
			delete(s.execActive, id)
			close(operation.done)
			s.mu.Unlock()
		}
		if settled == nil {
			finish()
		} else {
			select {
			case <-settled:
				finish()
			default:
				go func() { <-settled; finish() }()
			}
		}
	}()
	cfgProvider, pool, dialOpts, testMode := s.dependencies()
	var cfg config.RuntimeConfig
	if runCtx.Err() != nil {
		result = failedExec(result, context.Cause(runCtx), cfg)
	} else if testMode {
		result.State = "completed"
		result.CompletedAt = now
	} else {
		if cfgProvider == nil || pool == nil {
			return Exec{}, fmt.Errorf("%w: exec dependencies are not available", ErrConflict)
		}
		var err error
		cfg, err = cfgProvider.RuntimeConfig()
		if err != nil {
			return Exec{}, err
		}
		server, err := resolveServer(cfg, req.ServerRef)
		if err != nil {
			return Exec{}, err
		}
		if req.Passphrase != "" {
			key, ok := cfg.Keys[server.KeyID]
			if !ok {
				return Exec{}, fmt.Errorf("%w: passphrase requires a configured key", ErrValidation)
			}
			key.Passphrase = req.Passphrase
			cfg.Keys[server.KeyID] = key
		}
		result, settled = runExec(runCtx, result, req, server, cfg, pool, dialOpts)
	}
	s.mu.Lock()
	s.execs[id] = result
	s.pruneExecsLocked()
	operation.result = result
	s.mu.Unlock()
	return result, nil
}

func failedExec(result Exec, err error, cfg config.RuntimeConfig) Exec {
	result.State = "failed"
	result.ExitCode = -1
	result.CompletedAt = time.Now().UTC()
	result.FrameworkError = safeError(err, cfg)
	switch {
	case errors.Is(err, errExecShutdown):
		result.FrameworkCode = "service_shutdown"
	case errors.Is(err, context.DeadlineExceeded):
		result.FrameworkCode = "timeout"
	case errors.Is(err, context.Canceled):
		result.FrameworkCode = "canceled"
	case errors.Is(err, ErrTeardownTimeout):
		result.FrameworkCode = "cleanup_timeout"
	case errors.Is(err, sshpool.ErrHostKeyReject):
		result.FrameworkCode = "host_key_verification_failed"
	case errors.Is(err, keyutil.ErrPassphraseRequired):
		result.FrameworkCode = "passphrase_required"
	case sshpool.IsAuthError(err):
		result.FrameworkCode = "authentication_failed"
	default:
		var missing *ssh.ExitMissingError
		var network net.Error
		switch {
		case errors.As(err, &missing):
			result.FrameworkCode = "exit_status_missing"
		case errors.As(err, &network) && network.Timeout():
			result.FrameworkCode = "timeout"
		default:
			result.FrameworkCode = "ssh_error"
		}
	}
	return result
}

func runExec(ctx context.Context, result Exec, req ExecRequest, server config.ServerProfile, cfg config.RuntimeConfig, pool *sshpool.Pool, dialOpts DialOptions) (Exec, <-chan struct{}) {
	lease, err := pool.AcquireClientContext(ctx, server, cfg, nil, sshpool.DialOptions{
		AgentSocket: dialOpts.AgentSocket, HostKeyPolicy: req.HostKeyPolicy, Timeout: dialOpts.Timeout,
	})
	if err != nil {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		return failedExec(result, err, cfg), nil
	}
	client := lease.Client
	sshSession, err, settled := openExecSession(ctx, client)
	if err != nil {
		result = failedExec(result, err, cfg)
		select {
		case <-settled:
		default:
			result.CleanupError = "channel_open_pending"
		}
	} else {
		result, settled = executeSSH(ctx, sshSession, result, cfg)
	}
	// Retain the reference until every worker has really exited, even if the
	// bounded HTTP operation already returned a cleanup error.
	released := make(chan struct{})
	release := func() { lease.Release(); close(released) }
	select {
	case <-settled:
		release()
	default:
		go func() { <-settled; release() }()
	}
	return result, released
}

// The SSH library cannot retract an unconfirmed channel open. One owner waits
// for its result and closes a late channel; settled tracks that actual work so
// shutdown and pool references do not claim completion while it is pending.
func openExecSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error, <-chan struct{}) {
	ctx, cancel := context.WithTimeout(ctx, execChannelTimeout)
	defer cancel()
	settled := make(chan struct{})
	if ctx.Err() != nil {
		close(settled)
		return nil, context.Cause(ctx), settled
	}
	type opened struct {
		session *ssh.Session
		err     error
	}
	ready := make(chan opened)
	go func() {
		defer close(settled)
		session, err := client.NewSession()
		select {
		case ready <- opened{session, err}:
		case <-ctx.Done():
			if session != nil {
				_ = session.Close()
			}
		}
	}()
	select {
	case r := <-ready:
		<-settled
		return r.session, r.err, settled
	case <-ctx.Done():
		return nil, context.Cause(ctx), settled
	}
}

// executeSSH owns Run and both output writers. Normal results are read only
// after Run has drained both streams. Cancellation attempts a signal, then
// closes this channel even when the server ignores signals. A stalled peer is
// reported explicitly and the actual completion remains observable.
func executeSSH(ctx context.Context, session *ssh.Session, result Exec, cfg config.RuntimeConfig) (Exec, <-chan struct{}) {
	stdout := &limitedWriter{limit: maxExecOutput}
	stderr := &limitedWriter{limit: maxExecOutput}
	session.Stdout, session.Stderr = stdout, stderr
	ran := make(chan error, 1)
	go func() {
		if ctx.Err() != nil {
			ran <- context.Cause(ctx)
			return
		}
		ran <- session.Run(result.Command)
	}()
	settled := make(chan struct{})
	runErr, runReceived := waitExecResult(ctx, ran)
	// Give the signal a short opportunity to be sent. Its writer can itself be
	// stalled; channel closure must not wait indefinitely behind it.
	signaled := make(chan struct{})
	if !runReceived {
		go func() { defer close(signaled); _ = session.Signal(ssh.SIGKILL) }()
		timer := time.NewTimer(execSignalGrace)
		select {
		case <-signaled:
		case <-timer.C:
		}
		timer.Stop()
	} else {
		close(signaled)
	}
	closed := make(chan struct{})
	var closeErr error
	go func() { defer close(closed); closeErr = session.Close() }()
	// If cancellation won the select, Run still owns the output. Wait for it
	// and teardown together; on timeout take synchronized partial snapshots.
	runDone := make(chan struct{})
	if !runReceived {
		go func() { <-ran; close(runDone) }()
	} else {
		close(runDone)
	}
	go func() { <-runDone; <-closed; <-signaled; close(settled) }()
	timer := time.NewTimer(execCleanupGrace)
	select {
	case <-settled:
		if closeErr != nil && !errors.Is(closeErr, io.EOF) && !errors.Is(closeErr, net.ErrClosed) {
			result.CleanupError = "channel_close_failed"
		}
	case <-timer.C:
		result.CleanupError = "cleanup_timeout"
	}
	timer.Stop()
	result.Stdout, result.Truncated = stdout.snapshot()
	var truncated bool
	result.Stderr, truncated = stderr.snapshot()
	result.Truncated = result.Truncated || truncated
	result.CompletedAt = time.Now().UTC()
	if runErr == nil && result.CleanupError == "" {
		result.State = "completed"
		result.ExitCode = 0
		return result, settled
	}
	var exit *ssh.ExitError
	if errors.As(runErr, &exit) && result.CleanupError == "" {
		result.State = "completed"
		result.ExitCode = exit.ExitStatus()
		return result, settled
	}
	if runErr == nil || (result.CleanupError != "" && ctx.Err() == nil) {
		runErr = ErrTeardownTimeout
	}
	return failedExec(result, runErr, cfg), settled
}

// Prefer a completed, drained Run result when completion and cancellation are
// both observable. Cancellation wins only while no result has been published.
func waitExecResult(ctx context.Context, ran <-chan error) (error, bool) {
	select {
	case err := <-ran:
		return err, true
	case <-ctx.Done():
		select {
		case err := <-ran:
			return err, true
		default:
			return context.Cause(ctx), false
		}
	}
}

// Writers acknowledge the original byte count, including discarded bytes, so
// truncation never stops the SSH copy loop or changes the retained prefix.
type limitedWriter struct {
	mu        sync.Mutex
	buf       strings.Builder
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.limit - w.buf.Len()
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	_, _ = w.buf.Write(p)
	return n, nil
}

func (w *limitedWriter) snapshot() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.truncated
}
