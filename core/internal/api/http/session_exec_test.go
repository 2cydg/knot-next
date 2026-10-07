package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/session"
)

func TestServerExecUsesRequestContext(t *testing.T) {
	server := newStatefulTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/exec", `{"server_ref":"web","command":"uptime","timeout_ms":0}`).WithContext(ctx)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var body struct {
		Data session.Exec `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body.Data.FrameworkCode != "canceled" || body.Data.State != "failed" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerExecRejectsInvalidTimeout(t *testing.T) {
	server := newStatefulTestServer(t)
	for _, timeout := range []int64{-1, 9223372036854775807} {
		rec := httptest.NewRecorder()
		req := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/exec", fmt.Sprintf(`{"server_ref":"web","command":"uptime","timeout_ms":%d}`, timeout))
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestServerExecHTTPDisconnectClosesRemoteChannel(t *testing.T) {
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{})
	server, id := newSSHBackedTestServer(t, sshSrv)
	started, remoteDone := make(chan struct{}), make(chan struct{})
	sshSrv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, Started: started, Done: remoteDone, IgnoreSignal: true, BeforeExit: make(chan struct{})})
	httpSrv := httptest.NewServer(server.Handler())
	defer httpSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpSrv.URL+"/v1/sessions/exec", strings.NewReader(fmt.Sprintf(`{"server_ref":%q,"command":"wait","timeout_ms":0,"host_key_policy":"insecure-skip"}`, id)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("remote command never started")
	}
	cancel()
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("request unexpectedly completed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP cancel stalled")
	}
	select {
	case <-remoteDone:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP client returned but remote exec worker remained")
	}
	// ShutdownExec waits for the server-side handler and pool-reference release.
	budget, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := server.core.Session().ShutdownExec(budget); err != nil {
		t.Fatal(err)
	}
}

func TestServerExecReturnsRemoteExitSeven(t *testing.T) {
	sshSrv := startAttachSSHServer(t, sshserver.ShellBehavior{})
	server, id := newSSHBackedTestServer(t, sshSrv)
	sshSrv.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, ExitCode: 7, SendExitStatus: true, Writes: []sshserver.ScriptedWrite{{Data: []byte("out")}, {Data: []byte("err"), Stderr: true}}})
	rec := httptest.NewRecorder()
	req := authenticatedJSONRequest(http.MethodPost, "/v1/sessions/exec", fmt.Sprintf(`{"server_ref":%q,"command":"exit7","host_key_policy":"insecure-skip"}`, id))
	server.Handler().ServeHTTP(rec, req)
	var body struct {
		Data session.Exec `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body.Data.State != "completed" || body.Data.ExitCode != 7 || body.Data.Stdout != "out" || body.Data.Stderr != "err" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
