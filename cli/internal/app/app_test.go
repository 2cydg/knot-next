package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"knot-cli/internal/entry"
)

type languageProvider struct{ languages []string }

func (p languageProvider) PreferredLanguages(context.Context) ([]string, error) {
	return p.languages, nil
}

func TestInformationDoesNotInitializeInteractiveFlow(t *testing.T) {
	for _, language := range []string{"zh-CN", "en-US"} {
		for _, argument := range []string{"--help", "--version"} {
			var output, diagnostics bytes.Buffer
			code := Run(context.Background(), []string{argument}, Options{
				Stdout: &output, Stderr: &diagnostics, LanguageProvider: languageProvider{[]string{language}},
				IsTerminal: func() bool { t.Fatal("information checked TTY"); return false },
				Getwd:      func() (string, error) { t.Fatal("information read startup directory"); return "", nil },
				Runner:     func(context.Context, Launch) int { t.Fatal("information started core/TUI"); return 1 },
			})
			if code != 0 || output.Len() == 0 || diagnostics.Len() != 0 {
				t.Fatalf("%s/%s: code=%d out=%q err=%q", language, argument, code, output.String(), diagnostics.String())
			}
			if argument == "--help" {
				word := "Usage:"
				if language == "zh-CN" {
					word = "用法："
				}
				if !strings.Contains(output.String(), word) {
					t.Fatal("wrong language")
				}
			}
		}
	}
}

func TestDispatchPreservesEntryAndStartupDirectory(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		mode   entry.Mode
		page   entry.Page
		alias  string
		policy entry.ReturnPolicy
	}{
		{nil, entry.Management, entry.Servers, "", entry.ReturnToTUI},
		{[]string{"proxy"}, entry.PageDirect, entry.Proxies, "", entry.ReturnToTUI},
		{[]string{"ssh", "key"}, entry.AliasDirect, "", "key", entry.ExitProgram},
	} {
		called := false
		startup := t.TempDir()
		code := Run(context.Background(), tc.args, Options{
			LanguageProvider: languageProvider{[]string{"en"}}, IsTerminal: func() bool { return true },
			Getwd: func() (string, error) { return startup, nil },
			Runner: func(_ context.Context, launch Launch) int {
				called = true
				if launch.Context.Mode() != tc.mode || launch.Context.InitialPage() != tc.page || launch.Context.ServerRef() != tc.alias || launch.Context.ReturnPolicy() != tc.policy {
					t.Fatalf("wrong context: %+v", launch.Context)
				}
				if launch.Defaults.LocalDirectory != filepath.Clean(startup) || !launch.Defaults.ForwardAgent || !launch.Defaults.ClearOnConnect {
					t.Fatalf("wrong defaults: %+v", launch.Defaults)
				}
				return 7
			},
		})
		if !called || code != 7 {
			t.Fatalf("dispatch called=%v code=%d", called, code)
		}
	}
}

func TestInvalidAndNonTTYEntriesStopBeforeRunner(t *testing.T) {
	for _, language := range []string{"en", "zh"} {
		for _, tc := range []struct {
			args []string
			want int
		}{
			{[]string{"key", "extra"}, entry.ExitUsage}, {[]string{"missing-alias"}, entry.ExitFailure},
		} {
			var diagnostics bytes.Buffer
			code := Run(context.Background(), tc.args, Options{
				Stderr: &diagnostics, LanguageProvider: languageProvider{[]string{language}},
				IsTerminal: func() bool { return false },
				Getwd:      func() (string, error) { t.Fatal("error flow read directory"); return "", nil },
				Runner:     func(context.Context, Launch) int { t.Fatal("error flow ran business"); return 0 },
			})
			if code != tc.want || diagnostics.Len() == 0 {
				t.Fatalf("code=%d stderr=%q", code, diagnostics.String())
			}
			if tc.want == entry.ExitFailure && !strings.Contains(diagnostics.String(), map[string]string{"en": "terminal", "zh": "终端"}[language]) {
				t.Fatal("TTY failure not localized")
			}
		}
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestOutputFailureCancellationAndMissingFrontend(t *testing.T) {
	options := Options{LanguageProvider: languageProvider{[]string{"en"}}, Stdout: failedWriter{}}
	if got := Run(context.Background(), []string{"--help"}, options); got != entry.ExitFailure {
		t.Fatalf("closed stdout: %d", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Run(ctx, nil, options); got != entry.ExitCanceled {
		t.Fatalf("canceled launch: %d", got)
	}
	options.IsTerminal = func() bool { return true }
	options.Getwd = func() (string, error) { return "", errors.New("directory removed") }
	if got := Run(context.Background(), nil, options); got != entry.ExitFailure {
		t.Fatalf("removed directory: %d", got)
	}
	var diagnostics bytes.Buffer
	options.Stderr = &diagnostics
	options.Getwd = func() (string, error) { return t.TempDir(), nil }
	if got := Run(context.Background(), nil, options); got != entry.ExitFailure || !strings.Contains(diagnostics.String(), "under development") {
		t.Fatalf("missing runner: %d %s", got, diagnostics.String())
	}
}
