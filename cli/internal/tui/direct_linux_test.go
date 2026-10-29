package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

func TestAliasRunUsesConsoleTrustAndNativeTerminalWithoutTUI(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	var trusted atomic.Bool
	requests := make(chan coreclient.SessionRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		switch r.URL.Path {
		case "/v1/runtime":
			data(coreclient.Runtime{InstanceID: "instance", APIVersion: "v1"})
		case "/v1/version":
			data(coreclient.Version{Version: "test", APIVersion: "v1"})
		case "/v1/health":
			data(coreclient.Health{Status: "ok", Authenticated: true})
		case "/v1/capabilities":
			data([]coreclient.Capability{{Name: "config", Available: true}})
		case "/v1/config":
			data(coreclient.Summary{})
		case "/v1/config/servers/resolve":
			data(coreclient.Server{ID: "persistent-id", Alias: "production"})
		case "/v1/sessions":
			var request coreclient.SessionRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			requests <- request
			data(coreclient.Session{ID: "session", State: "connecting"})
		case "/v1/sessions/session":
			state := "host_key_pending"
			if trusted.Load() {
				state = "connected"
			}
			data(coreclient.Session{ID: "session", Alias: "production", State: state})
		case "/v1/sessions/session/challenges/host-key":
			if r.Method == http.MethodPost {
				var answer coreclient.ChallengeAnswer
				_ = json.NewDecoder(r.Body).Decode(&answer)
				trusted.Store(answer.ChallengeID == "shown-host" && answer.Accept && answer.Remember)
				data(map[string]bool{"accepted": trusted.Load()})
			} else {
				data(coreclient.Challenge{ID: "shown-host", SessionID: "session", Type: "host_key", Pending: true, Host: "example:22", ServerAlias: "production", KeyType: "ssh-ed25519", Fingerprint: "SHA256:console-host"})
			}
		case "/v1/sessions/session/attach":
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"session.snapshot","session":{"id":"session"}}`))
			// Wait for the bridge's resize frame, sent after raw mode starts.
			_, _, _ = ws.Read(ctx)
			_ = ws.Write(ctx, websocket.MessageBinary, []byte("REMOTE_OUTPUT\r\n"))
			_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"session.exit","session_id":"session","exit_code":7}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setRunRuntime(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	captured := make(chan string, 1)
	go func() {
		var output strings.Builder
		answered := false
		buffer := make([]byte, 4096)
		for {
			n, err := master.Read(buffer)
			output.Write(buffer[:n])
			text := output.String()
			if !answered && strings.Contains(text, "yes/no") {
				answered = true
				_, _ = master.Write([]byte("yes\n"))
			}
			if err != nil || (strings.Contains(text, "REMOTE_OUTPUT") && strings.Count(text, "\x1b]0;Knot\x07") >= 2) {
				captured <- text
				return
			}
		}
	}()
	launch, _ := entry.Parse([]string{"ssh", "production"})
	code := Run(ctx, launch, i18n.New(i18n.English), slave, slave, slave)
	var output string
	select {
	case output = <-captured:
	case <-time.After(time.Second):
		master.Close()
		output = <-captured
	}
	if code != 7 || !trusted.Load() || !strings.Contains(output, "REMOTE_OUTPUT") || !strings.Contains(output, "The authenticity of host") || !strings.Contains(output, "SHA256:console-host") {
		t.Fatal("console trust/bridge flow failed", code, output)
	}
	if strings.Contains(output, "\x1b[?1049") || strings.ContainsAny(output, "┌┐└┘") || !strings.Contains(output, "\x1b]0;production\x07") {
		t.Fatal("alias started a TUI or did not set the connected title", output)
	}
	request := <-requests
	if request.AllowAuthRetry || request.ServerRef != "persistent-id" {
		t.Fatal("alias enabled auth retry or failed to resolve its server", request)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("console confirmation or bridge changed terminal state", err)
	}
}
