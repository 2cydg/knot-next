// Package corestart coordinates public API checks and optional process startup.
// It never opens or parses the daemon configuration file.
package corestart

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/preferences"
)

type Process interface{ Done() <-chan struct{} }
type Dependencies struct {
	Probe    func(context.Context, preferences.Core) error
	Check    func(string) error
	Start    func(preferences.Core) (Process, error)
	Wait     time.Duration
	Interval time.Duration
}
type attempt struct {
	done chan struct{}
	err  error
}
type Coordinator struct {
	mu        sync.Mutex
	deps      Dependencies
	active    *attempt
	attempted map[preferences.Core]*attempt
}

func New(deps Dependencies) *Coordinator {
	if deps.Probe == nil {
		deps.Probe = Probe
	}
	if deps.Check == nil {
		deps.Check = checkExecutable
	}
	if deps.Start == nil {
		deps.Start = start
	}
	if deps.Wait == 0 {
		deps.Wait = 10 * time.Second
	}
	if deps.Interval == 0 {
		deps.Interval = 150 * time.Millisecond
	}
	return &Coordinator{deps: deps, attempted: make(map[preferences.Core]*attempt)}
}

func Probe(ctx context.Context, settings preferences.Core) error {
	client, err := coreclient.New(settings.URL, settings.Secret)
	if err != nil {
		return err
	}
	defer client.Close()
	version, err := client.Version(ctx)
	if err != nil {
		return err
	}
	if version.Version == "" || version.APIVersion == "" {
		return coreclient.Failure("PROTOCOL_ERROR")
	}
	if version.APIVersion != "v1" {
		return coreclient.Failure("API_UNSUPPORTED")
	}
	first, err := client.Runtime(ctx)
	if err != nil {
		return err
	}
	if first.InstanceID == "" || first.APIVersion != "v1" {
		return coreclient.Failure("PROTOCOL_ERROR")
	}
	last, err := client.Runtime(ctx)
	if err != nil {
		return err
	}
	if last.InstanceID != first.InstanceID {
		return coreclient.Failure("INSTANCE_CHANGED")
	}
	return nil
}

// ResolvePaths fixes relative paths against the CLI launch directory. The core
// configuration path is deliberately never stat'ed, read or created here.
func ResolvePaths(settings preferences.Core, directory string) (preferences.Core, error) {
	expand := func(path string) (string, error) {
		if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			path = filepath.Join(home, strings.TrimLeft(path[1:], "/\\"))
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(directory, path)
		}
		return filepath.Abs(path)
	}
	if !settings.AutoStart {
		return settings, nil
	}
	if strings.TrimSpace(settings.Executable) == "" {
		return settings, coreclient.Failure("EXECUTABLE_INVALID")
	}
	if strings.TrimSpace(settings.ConfigFile) == "" {
		return settings, coreclient.Failure("CONFIG_PATH_INVALID")
	}
	var err error
	settings.Executable, err = expand(settings.Executable)
	if err != nil {
		return settings, coreclient.Failure("EXECUTABLE_INVALID")
	}
	settings.ConfigFile, err = expand(settings.ConfigFile)
	if err != nil {
		return settings, coreclient.Failure("CONFIG_PATH_INVALID")
	}
	return settings, nil
}

func (c *Coordinator) Ensure(ctx context.Context, settings preferences.Core) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	client, err := coreclient.New(settings.URL, settings.Secret)
	if err != nil {
		return err
	}
	client.Close()
	if settings.AutoStart {
		if settings.ConfigFile == "" {
			return coreclient.Failure("CONFIG_PATH_INVALID")
		}
		if err := c.deps.Check(settings.Executable); err != nil {
			return err
		}
	}
	err = c.deps.Probe(ctx, settings)
	if err == nil {
		return nil
	}
	if coreclient.Code(err) != "UNREACHABLE" || !settings.AutoStart {
		return err
	}
	c.mu.Lock()
	if previous := c.attempted[settings]; previous != nil {
		c.mu.Unlock()
		select {
		case <-previous.done:
			if previous.err != nil {
				return previous.err
			}
			return err
		default:
			return await(ctx, previous)
		}
	}
	if previous := c.active; previous != nil {
		c.mu.Unlock()
		if err := await(ctx, previous); ctx.Err() != nil {
			return err
		}
		return c.Ensure(ctx, settings)
	}
	current := &attempt{done: make(chan struct{})}
	c.active = current
	c.attempted[settings] = current
	c.mu.Unlock()
	// The initiating request owns this bounded wait; cancellation prevents any
	// subsequent save but does not terminate the detached daemon.
	err = c.launch(ctx, settings)
	c.mu.Lock()
	current.err = err
	c.active = nil
	close(current.done)
	c.mu.Unlock()
	return err
}
func await(ctx context.Context, a *attempt) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
		return a.err
	}
}
func (c *Coordinator) launch(parent context.Context, settings preferences.Core) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	process, err := c.deps.Start(settings)
	if err != nil {
		return coreclient.Failure("CORE_START_FAILED")
	}
	ctx, cancel := context.WithTimeout(parent, c.deps.Wait)
	defer cancel()
	ticker := time.NewTicker(c.deps.Interval)
	defer ticker.Stop()
	exited := false
	done := process.Done()
	for {
		probeCtx, stop := context.WithTimeout(ctx, time.Second)
		err = c.deps.Probe(probeCtx, settings)
		stop()
		if err == nil {
			return nil
		}
		switch coreclient.Code(err) {
		case "UNREACHABLE", "OFFLINE", "TIMEOUT", "CANCELED":
		default:
			return err
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			select {
			case <-process.Done():
				exited = true
			default:
			}
			if exited {
				failure := &coreclient.Error{Code: "CORE_START_FAILED"}
				if child, ok := process.(interface{ ExitCode() int }); ok {
					failure.Details, _ = json.Marshal(map[string]int{"exit_code": child.ExitCode()})
				}
				return failure
			}
			return coreclient.Failure("OFFLINE")
		case <-done:
			exited = true
			done = nil // A competing CLI may still make the API ready.
		case <-ticker.C:
		}
	}
}
func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		return coreclient.Failure("EXECUTABLE_INVALID")
	}
	return nil
}

type child struct {
	done     chan struct{}
	exitCode int
}

func (p *child) Done() <-chan struct{} { return p.done }
func (p *child) ExitCode() int         { return p.exitCode }
func start(settings preferences.Core) (Process, error) {
	cmd := exec.Command(settings.Executable, "--config", settings.ConfigFile)
	detach(cmd)
	// Nil streams use the null device; no terminal bytes are inherited.
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &child{done: make(chan struct{})}
	go func() { _ = cmd.Wait(); process.exitCode = cmd.ProcessState.ExitCode(); close(process.done) }()
	return process, nil
}
