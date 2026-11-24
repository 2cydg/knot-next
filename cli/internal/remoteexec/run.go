// Package remoteexec runs non-interactive commands through the core API.
package remoteexec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/corestart"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

type output struct {
	Success    bool   `json:"success"`
	ExitCode   *int   `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

func Run(ctx context.Context, launch entry.LaunchContext, tr i18n.Translator, out, diagnostics io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	result := output{}
	status := entry.ExitFailure
	finish := func() int {
		result.DurationMS = time.Since(start).Milliseconds()
		if launch.JSON() {
			if err := json.NewEncoder(out).Encode(result); err != nil {
				return entry.ExitFailure
			}
		} else {
			if _, err := io.WriteString(out, result.Stdout); err != nil {
				return entry.ExitFailure
			}
			if _, err := io.WriteString(diagnostics, result.Stderr); err != nil {
				return entry.ExitFailure
			}
			if result.Error != "" {
				fmt.Fprintln(diagnostics, tr.ProtocolError(result.Error))
			}
			if result.Truncated {
				fmt.Fprintln(diagnostics, tr.Message("ssh.truncated"))
			}
		}
		return status
	}
	fail := func(err error) int {
		result.Error = coreclient.Code(err)
		if ctx.Err() != nil {
			status = entry.ExitCanceled
		}
		return finish()
	}
	settings, err := preferences.Load()
	if err != nil || !settings.Configured() {
		return fail(coreclient.Failure("CLIENT_CONFIG_INVALID"))
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	selected, err := corestart.ResolvePaths(*settings.Core, dir)
	if err != nil {
		return fail(err)
	}
	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = corestart.New(corestart.Dependencies{}).Ensure(loadCtx, selected); err != nil {
		return fail(err)
	}
	snapshot, err := coreclient.Load(loadCtx, selected.URL, selected.Secret)
	if err != nil {
		return fail(err)
	}
	defer snapshot.Client.Close()
	server, err := snapshot.Client.Resolve(loadCtx, launch.ServerRef())
	if err != nil {
		return fail(err)
	}
	execResult, err := snapshot.Client.Exec(ctx, server.ID, launch.Command(), launch.SessionKey(), launch.Timeout())
	if err != nil {
		return fail(err)
	}
	result.Stdout, result.Stderr, result.Truncated = execResult.Stdout, execResult.Stderr, execResult.Truncated
	if execResult.State == "completed" && execResult.ExitCode >= 0 && execResult.ExitCode <= 255 {
		result.ExitCode = &execResult.ExitCode
		result.Success = execResult.ExitCode == 0
		status = execResult.ExitCode
	} else {
		result.Error = execResult.FrameworkCode
		if result.Error == "" {
			result.Error = "PROTOCOL_ERROR"
		}
	}
	return finish()
}
