// Package serverlist renders server summaries without starting a TUI.
package serverlist

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/corestart"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func Run(ctx context.Context, pattern string, tr i18n.Translator, out, diagnostics io.Writer) int {
	fail := func(err error) int {
		if ctx.Err() != nil {
			return entry.ExitCanceled
		}
		fmt.Fprintln(diagnostics, tr.ProtocolError(coreclient.Code(err)))
		return entry.ExitFailure
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = corestart.New(corestart.Dependencies{}).Ensure(ctx, selected); err != nil {
		return fail(err)
	}
	snapshot, err := coreclient.Load(ctx, selected.URL, selected.Secret)
	if err != nil {
		return fail(err)
	}
	defer snapshot.Client.Close()
	if !snapshot.Available("config") {
		return fail(coreclient.Failure("CONFIG_UNAVAILABLE"))
	}
	if err = Render(out, snapshot.Config.Servers, pattern, tr); err != nil {
		fmt.Fprintln(diagnostics, tr.Text(i18n.OutputFailed, map[string]any{"Reason": err.Error()}))
		return entry.ExitFailure
	}
	return entry.ExitOK
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '�'
		}
		return r
	}, s)
}

func Render(out io.Writer, servers []coreclient.Server, pattern string, tr i18n.Translator) error {
	servers = append([]coreclient.Server(nil), servers...)
	sort.SliceStable(servers, func(i, j int) bool {
		a, b := servers[i], servers[j]
		if a.LastUsed == nil && b.LastUsed != nil {
			return false
		}
		if a.LastUsed != nil && b.LastUsed == nil {
			return true
		}
		if a.LastUsed != nil && b.LastUsed != nil && !a.LastUsed.Equal(*b.LastUsed) {
			return a.LastUsed.After(*b.LastUsed)
		}
		return a.Alias < b.Alias
	})
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", tr.Message("ui.alias"), tr.Message("ui.host"), tr.Message("ui.user"), tr.Message("ui.tags"), tr.Message("ui.recent")); err != nil {
		return err
	}
	pattern = strings.ToLower(pattern)
	for _, s := range servers {
		if pattern != "" && !strings.Contains(strings.ToLower(strings.Join(append([]string{s.Alias, s.Host, s.User}, s.Tags...), " ")), pattern) {
			continue
		}
		recent := "-"
		if s.LastUsed != nil {
			recent = s.LastUsed.Local().Format("2006-01-02 15:04")
		}
		tags := strings.Join(s.Tags, ", ")
		if tags == "" {
			tags = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", safe(s.Alias), safe(s.Host), safe(s.User), safe(tags), recent); err != nil {
			return err
		}
	}
	return w.Flush()
}
