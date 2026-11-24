// Package corestop requests shutdown of the configured core without starting it.
package corestop

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func Run(ctx context.Context, tr i18n.Translator, out, diagnostics io.Writer) int {
	settings, err := preferences.Load()
	if err != nil || settings.Core == nil || strings.TrimSpace(settings.Core.URL) == "" {
		fmt.Fprintln(diagnostics, tr.ProtocolError("CLIENT_CONFIG_INVALID"))
		return entry.ExitFailure
	}
	return stop(ctx, *settings.Core, tr, out, diagnostics)
}

func stop(ctx context.Context, settings preferences.Core, tr i18n.Translator, out, diagnostics io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := coreclient.New(settings.URL, settings.Secret)
	if err == nil {
		defer client.Close()
		err = client.Shutdown(ctx)
	}
	if ctx.Err() == context.Canceled {
		return entry.ExitCanceled
	}
	message := "cli.core_shutdown_requested"
	if coreclient.Code(err) == "UNREACHABLE" {
		message, err = "cli.core_already_stopped", nil
	}
	if err != nil {
		fmt.Fprintln(diagnostics, tr.ProtocolError(coreclient.Code(err)))
		return entry.ExitFailure
	}
	if _, err = fmt.Fprintln(out, tr.Message(message)); err != nil {
		return entry.ExitFailure
	}
	return entry.ExitOK
}
