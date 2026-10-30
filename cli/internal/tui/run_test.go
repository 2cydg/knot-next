package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func TestRunErrorMapsInterruptsWithoutProtocolDiagnostic(t *testing.T) {
	for _, err := range []error{tea.ErrInterrupted, fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrInterrupted), context.Canceled} {
		var output bytes.Buffer
		if code := runError(context.Background(), err, i18n.New(i18n.English), &output); code != entry.ExitCanceled || output.Len() != 0 {
			t.Fatal("interrupt reported as protocol failure", code, output.String())
		}
	}
	var output bytes.Buffer
	if code := runError(context.Background(), errors.New("internal detail"), i18n.New(i18n.English), &output); code != entry.ExitFailure || output.String() != i18n.New(i18n.English).ProtocolError("PROTOCOL_ERROR")+"\n" {
		t.Fatal("unexpected run error not localized")
	}
}

func TestRunCanceledBeforeDiscovery(t *testing.T) {
	setTestHome(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{nil, {"ssh", "alias"}} {
		launch, _ := entry.Parse(args)
		var diagnostics bytes.Buffer
		if code := Run(ctx, launch, i18n.New(i18n.English), nil, io.Discard, &diagnostics); code != entry.ExitCanceled || diagnostics.Len() != 0 {
			t.Fatal("canceled launch touched discovery or printed failure", code, diagnostics.String())
		}
	}
}

func TestRunSetsAndRestoresTitleWhenDiscoveryFails(t *testing.T) {
	setTestHome(t, t.TempDir())
	path, _ := preferences.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"ssh", "alias"}} {
		launch, _ := entry.Parse(args)
		var output, diagnostics bytes.Buffer
		code := Run(context.Background(), launch, i18n.New(i18n.English), nil, &output, &diagnostics)
		if code != entry.ExitFailure || diagnostics.Len() == 0 {
			t.Fatal("expected discovery failure")
		}
		if got, want := output.String(), "\x1b]0;Knot\x07\x1b]0;Knot\x07"; got != want {
			t.Fatalf("management/alias entry did not set and restore the title: %q", got)
		}
	}
}

func TestAliasRunCancellationReleasesHTTPRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requestDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
		close(requestDone)
	}))
	defer server.Close()
	setRunRuntime(t, server)
	launch, _ := entry.Parse([]string{"ssh", "alias"})
	var diagnostics bytes.Buffer
	if code := Run(ctx, launch, i18n.New(i18n.English), nil, io.Discard, &diagnostics); code != entry.ExitCanceled || diagnostics.Len() != 0 {
		t.Fatal("alias cancellation reported failure", code, diagnostics.String())
	}
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled alias retained HTTP request")
	}
}

func setRunRuntime(t *testing.T, server *httptest.Server) {
	t.Helper()
	setTestHome(t, t.TempDir())
	if err := preferences.Save(preferences.Settings{Core: &preferences.Core{URL: server.URL, Secret: "token"}}); err != nil {
		t.Fatal(err)
	}
}
