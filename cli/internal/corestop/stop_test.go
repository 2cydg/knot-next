package corestop

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
	"knot-cli/internal/preferences"
)

func TestStopSendsAuthenticatedShutdownAndReportsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want int
	}{
		{"accepted", http.StatusOK, `{"data":{"shutdown":true}}`, entry.ExitOK},
		{"rejected", http.StatusUnauthorized, `{"error":{"code":"UNAUTHORIZED"}}`, entry.ExitFailure},
		{"invalid_ack", http.StatusOK, `{"data":{"shutdown":false}}`, entry.ExitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/shutdown" || r.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			var out, diag bytes.Buffer
			code := stop(context.Background(), preferences.Core{URL: srv.URL, Secret: "secret", AutoStart: true}, i18n.New(i18n.English), &out, &diag)
			if code != tc.want || calls != 1 {
				t.Fatalf("code %d calls %d stdout %q stderr %q", code, calls, out.String(), diag.String())
			}
			if code == 0 && !strings.Contains(out.String(), "requested") {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestStopDoesNotStartCoreWhenOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected request") }))
	url := srv.URL
	srv.Close()
	var out, diag bytes.Buffer
	code := stop(context.Background(), preferences.Core{URL: url, AutoStart: true}, i18n.New(i18n.English), &out, &diag)
	if code != 0 || !strings.Contains(out.String(), "not running") {
		t.Fatalf("code %d out %q err %q", code, out.String(), diag.String())
	}
}
