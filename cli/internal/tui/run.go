package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

func Run(ctx context.Context, launch entry.LaunchContext, tr i18n.Translator, input io.Reader, output, diagnostics io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if ctx.Err() != nil {
		return entry.ExitCanceled
	}
	path, err := coreclient.RuntimeFile()
	if err != nil {
		return runError(ctx, err, tr, diagnostics)
	}
	if launch.Mode() == entry.AliasDirect {
		// Preserve direct-entry policy without starting a management menu. The
		// real waiting/challenge/terminal controller is implemented next.
		client, _, err := coreclient.Discover(path)
		if err != nil {
			return runError(ctx, err, tr, diagnostics)
		}
		defer client.Close()
		if _, err := client.Resolve(ctx, launch.ServerRef()); err != nil {
			return runError(ctx, err, tr, diagnostics)
		}
		if ctx.Err() != nil {
			return entry.ExitCanceled
		}
		fmt.Fprintln(diagnostics, tr.Message("ui.ssh_pending"))
		return entry.ExitFailure
	}
	uiCtx, cancel := context.WithCancel(ctx)
	m := New(uiCtx, launch, tr, path)
	program := tea.NewProgram(m, tea.WithContext(uiCtx), tea.WithInput(input), tea.WithOutput(output))
	_, err = program.Run()
	cancel()
	m.Close()
	if ctx.Err() != nil {
		return entry.ExitCanceled
	}
	if err != nil {
		return runError(ctx, err, tr, diagnostics)
	}
	return m.exitCode
}

func runError(ctx context.Context, err error, tr i18n.Translator, diagnostics io.Writer) int {
	if ctx.Err() != nil || errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) {
		return entry.ExitCanceled
	}
	var protocol *coreclient.Error
	code := "PROTOCOL_ERROR"
	if errors.As(err, &protocol) {
		code = protocol.Code
	}
	fmt.Fprintln(diagnostics, tr.ProtocolError(code))
	return entry.ExitFailure
}
