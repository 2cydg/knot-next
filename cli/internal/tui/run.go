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
	"knot-cli/internal/terminal"
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
	m := New(ctx, launch, tr, path)
	defer m.Close()
	for {
		program := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output))
		_, err = program.Run()
		// Run has joined Bubble Tea's reader and restored its terminal. Only
		// now may the native bridge read stdin or change terminal modes.
		m.pause()
		if ctx.Err() != nil {
			return entry.ExitCanceled
		}
		if err != nil {
			return runError(ctx, err, tr, diagnostics)
		}
		if m.handoff == nil {
			if launch.ReturnPolicy() == entry.ExitProgram && m.exitCode == entry.ExitFailure && m.directMessage != "" {
				fmt.Fprintln(diagnostics, m.directMessage)
			}
			return m.exitCode
		}
		ssh := m.handoff
		in, _ := input.(*os.File)
		out, _ := output.(*os.File)
		result := terminal.Run(ctx, ssh.Attempt.Client, ssh.Attempt.Instance, ssh.Session.ID, in, out)
		if ctx.Err() != nil {
			ssh.Attempt.Close()
		}
		warnings := ssh.Attempt.Finish()
		m.ssh, m.handoff, m.modal = nil, nil, nil
		if launch.ReturnPolicy() == entry.ExitProgram {
			if result.Err != nil {
				fmt.Fprintln(diagnostics, tr.ProtocolError(coreclient.Code(result.Err)))
			}
			if result.Detached {
				fmt.Fprintln(diagnostics, tr.Text("ssh.detached", map[string]any{"ID": Display(ssh.Session.ID)}))
			}
			if result.Truncated {
				fmt.Fprintln(diagnostics, tr.Message("ssh.truncated"))
			}
			for _, code := range warnings {
				fmt.Fprintln(diagnostics, tr.Text("ui.warning", map[string]any{"Code": Display(code)}))
			}
			return result.Code()
		}
		if ctx.Err() != nil {
			return entry.ExitCanceled
		}
		m.page, m.focus = entry.Servers, OperationsFocus
		m.search.SetValue("")
		m.resumeCmd = m.showSSHResult(result, warnings, ssh.Session.ID)
	}
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
