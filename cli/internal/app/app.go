// Package app dispatches immutable launch intent. Business runners are added in
// the API/TUI and SSH stages, keeping this foundation independent of core.
package app

import (
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"knot-cli/internal/buildinfo"
	"knot-cli/internal/defaults"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

type Launch struct {
	Context    entry.LaunchContext
	Defaults   defaults.Values
	Translator i18n.Translator
}

// Runner takes ownership only after language, arguments and terminal checks.
// It must preserve Launch.Context.ReturnPolicy through terminal handoffs.
type Runner func(context.Context, Launch) int

type Options struct {
	Stdin            *os.File
	Stdout           io.Writer
	Stderr           io.Writer
	LanguageProvider i18n.LanguageProvider
	IsTerminal       func() bool
	Getwd            func() (string, error)
	Runner           Runner
}

func Run(ctx context.Context, args []string, options Options) int {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	if options.LanguageProvider == nil {
		options.LanguageProvider = i18n.SystemProvider{}
	}
	translator := i18n.New(i18n.Detect(ctx, options.LanguageProvider).Language)
	launch, err := entry.Parse(args)
	if err != nil {
		usageErr := err.(*entry.UsageError)
		fmt.Fprintln(options.Stderr, translator.Text(i18n.InvalidArguments, map[string]any{"Argument": usageErr.Argument}))
		return entry.ExitUsage
	}
	var output string
	switch launch.Mode() {
	case entry.Help:
		output = translator.Text(i18n.Usage, nil)
	case entry.Version:
		output = buildinfo.String() + "\n"
	}
	if output != "" {
		if _, err := io.WriteString(options.Stdout, output); err != nil {
			fmt.Fprintln(options.Stderr, translator.Text(i18n.OutputFailed, map[string]any{"Reason": err.Error()}))
			return entry.ExitFailure
		}
		return entry.ExitOK
	}
	if ctx.Err() != nil {
		return entry.ExitCanceled
	}
	if options.IsTerminal == nil {
		options.IsTerminal = func() bool {
			stdout, ok := options.Stdout.(*os.File)
			return ok && options.Stdin != nil && term.IsTerminal(int(options.Stdin.Fd())) && term.IsTerminal(int(stdout.Fd()))
		}
	}
	if !options.IsTerminal() {
		fmt.Fprintln(options.Stderr, translator.Text(i18n.TTYRequired, nil))
		return entry.ExitFailure
	}
	if options.Getwd == nil {
		options.Getwd = os.Getwd
	}
	directory, err := options.Getwd()
	if err != nil {
		fmt.Fprintln(options.Stderr, translator.Text(i18n.StartupDirectoryFailed, map[string]any{"Reason": err.Error()}))
		return entry.ExitFailure
	}
	if options.Runner == nil {
		fmt.Fprintln(options.Stderr, translator.Text(i18n.FrontendPending, nil))
		return entry.ExitFailure
	}
	return options.Runner(ctx, Launch{Context: launch, Defaults: defaults.FromStartupDirectory(directory), Translator: translator})
}
