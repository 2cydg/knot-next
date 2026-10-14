package integration

import (
	"bytes"
	"context"
	"encoding/json"
	protocolsftp "github.com/pkg/sftp"
	"io"
	"knot-core/internal/testutil/proxyserver"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/forward"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestSSHThroughSavedSOCKSAndHTTPProxy(t *testing.T) {
	for _, typ := range []string{"socks5", "http"} {
		t.Run(typ, func(t *testing.T) {
			c, remote := newSFTPAPI(t, protocolsftp.InMemHandler())
			proxy := proxyserver.New(t, proxyserver.Config{Type: typ, Username: "proxy-user", Password: "proxy-pass"})
			host, p, _ := net.SplitHostPort(proxy.Address())
			port, _ := strconv.Atoi(p)
			var saved struct {
				ID string `json:"id"`
			}
			c.call(t, http.MethodPost, "/v1/config/proxies", map[string]any{"alias": typ, "type": typ, "host": host, "port": port, "username": "proxy-user"}, 201, &saved)
			c.call(t, http.MethodPut, "/v1/secrets/proxies/"+saved.ID+"/password", map[string]any{"password": "proxy-pass"}, 200, nil)
			c.call(t, http.MethodPut, "/v1/config/servers/"+c.serverRef, map[string]any{"alias": "controlled-sftp", "host": remote.Host(), "port": remote.Port(), "user": "sftp-test", "auth_method": "password", "proxy_id": saved.ID}, 200, nil)
			shell := gapSSH(t, c, "insecure-skip")
			if shell.ConnectionID == "" || proxy.ConnectionCount() == 0 {
				t.Fatal("SSH bypassed saved proxy", shell)
			}
		})
	}
}
func TestIntermediateConnectionQueriesAndExplicitForward(t *testing.T) {
	c, remote := newSFTPAPI(t, protocolsftp.InMemHandler())
	jump := sshserver.New(t, sshserver.Config{User: "jump-user", Password: "jump-pass"})
	var saved struct {
		ID string `json:"id"`
	}
	c.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"alias": "jump", "host": jump.Host(), "port": jump.Port(), "user": "jump-user", "auth_method": "password"}, 201, &saved)
	c.call(t, http.MethodPut, "/v1/secrets/servers/"+saved.ID+"/password", map[string]any{"password": "jump-pass"}, 200, nil)
	c.call(t, http.MethodPut, "/v1/config/servers/"+c.serverRef, map[string]any{"alias": "controlled-sftp", "host": remote.Host(), "port": remote.Port(), "user": "sftp-test", "auth_method": "password", "jump_host_ids": []string{saved.ID}}, 200, nil)
	shell := gapSSH(t, c, "insecure-skip")
	var target sshpool.Connection
	c.call(t, http.MethodGet, "/v1/connections/"+shell.ConnectionID, nil, 200, &target)
	if !target.Target || len(target.Route) != 1 {
		t.Fatal("missing target route", target)
	}
	var intermediate sshpool.Connection
	c.call(t, http.MethodGet, "/v1/connections/"+target.Route[0], nil, 200, &intermediate)
	if intermediate.Target || intermediate.ServerID != saved.ID {
		t.Fatal("jump mislabeled as business target", intermediate)
	}
	var listing []sshpool.Connection
	c.call(t, http.MethodGet, "/v1/connections?server_id="+saved.ID, nil, 200, &listing)
	if len(listing) != 1 || listing[0].ID != intermediate.ID {
		t.Fatal(listing)
	}
	echo := echoTarget(t, "tcp", "127.0.0.1:0")
	req := map[string]any{"server_id": saved.ID, "type": "local", "bind": "127.0.0.1:0", "target": echo}
	c.call(t, http.MethodPost, "/v1/forwards", req, 409, nil)
	req["connection_id"] = intermediate.ID
	var v forward.View
	c.call(t, http.MethodPost, "/v1/forwards", req, 202, &v)
	roundtripForward(t, waitForward(t, c, v.ID, "active").BoundAddress, false, echo)
}
func TestRemoteForwardRefusalKeepsAuthenticatedSSHReady(t *testing.T) {
	c, remote := newSFTPAPI(t, protocolsftp.InMemHandler())
	remote.SetRejectForward(true)
	c.call(t, http.MethodPut, "/v1/config/servers/"+c.serverRef+"/forwards", map[string]any{"forwards": []map[string]any{{"type": "remote", "bind": "127.0.0.1:0", "target": "127.0.0.1:80", "enabled": true}}}, 200, nil)
	shell := gapSSH(t, c, "insecure-skip")
	var list []forward.View
	c.call(t, http.MethodGet, "/v1/forwards", nil, 200, &list)
	if len(list) != 1 {
		t.Fatal("auto refusal hidden", list)
	}
	got := waitForward(t, c, list[0].ID, "failed")
	if got.FailureCode == "" || got.Origin != "persistent" {
		t.Fatal(got)
	}
	c.call(t, http.MethodGet, "/v1/sessions/"+shell.ID, nil, 200, &shell)
	if shell.State != "connected" {
		t.Fatal("forward refusal broke SSH", shell)
	}
}
func TestConfigFieldDetailsThroughHTTP(t *testing.T) {
	c, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
	body := []byte(`{"forwards":[{"type":"dynamic","bind":"[not-an-ip]:8080","enabled":true}]}`)
	req, err := http.NewRequest(http.MethodPut, c.baseURL+"/v1/config/servers/"+c.serverRef+"/forwards", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error struct {
			Code     string `json:"code"`
			Resource string `json:"resource"`
			Details  struct {
				Fields []config.ValidationError `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 || envelope.Error.Code != "VALIDATION_FAILED" || len(envelope.Error.Details.Fields) != 1 || envelope.Error.Details.Fields[0].Field != "forwards[0].bind" || envelope.Error.Resource == "" {
		t.Fatal(string(raw))
	}
}
func waitReviewChallenge(t *testing.T, c *sftpAPIClient, route, kind string) session.Challenge {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ch session.Challenge
		c.call(t, http.MethodGet, route+"/challenges/"+kind, nil, 200, &ch)
		if ch.Pending {
			return ch
		}
		if time.Now().After(deadline) {
			t.Fatal("no challenge", kind)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestChallengeAbortCrossResourceAndTransportFailureHTTP(t *testing.T) {
	for _, root := range []string{"/v1/sessions", "/v1/sftp"} {
		t.Run(root, func(t *testing.T) {
			c, remote := newSFTPAPI(t, protocolsftp.InMemHandler())
			var a, b struct {
				ID string `json:"id"`
			}
			c.call(t, http.MethodPost, root, map[string]any{"server_ref": c.serverRef}, 201, &a)
			first := waitReviewChallenge(t, c, root+"/"+a.ID, "host-key")
			c.call(t, http.MethodPost, root, map[string]any{"server_ref": c.serverRef}, 201, &b)
			second := waitReviewChallenge(t, c, root+"/"+b.ID, "host-key")
			c.call(t, http.MethodPost, root+"/"+b.ID+"/challenges/host-key", map[string]any{"challenge_id": first.ID, "accept": true}, 409, nil)
			c.call(t, http.MethodPost, root+"/"+a.ID+"/challenges/host-key", map[string]any{"challenge_id": first.ID, "abort": true}, 200, nil)
			deadline := time.Now().Add(2 * time.Second)
			for {
				var got struct {
					State       string `json:"state"`
					FailureCode string `json:"failure_code"`
				}
				c.call(t, http.MethodGet, root+"/"+a.ID, nil, 200, &got)
				if got.State == "failed" {
					if got.FailureCode != "CHALLENGE_ABORTED" {
						t.Fatal(got)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("abort not settled")
				}
				time.Sleep(time.Millisecond)
			}
			remote.DropTransports()
			for {
				var got struct {
					State         string             `json:"state"`
					FailureCode   string             `json:"failure_code"`
					LastChallenge *session.Challenge `json:"last_challenge"`
				}
				c.call(t, http.MethodGet, root+"/"+b.ID, nil, 200, &got)
				if got.State == "failed" {
					if got.FailureCode != "CONNECTION_FAILED" || got.LastChallenge == nil || got.LastChallenge.Outcome != "aborted" {
						t.Fatal("disconnect misreported as expiry", got)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("disconnect waited for challenge expiry")
				}
				time.Sleep(time.Millisecond)
			}
			c.call(t, http.MethodPost, root+"/"+b.ID+"/challenges/host-key", map[string]any{"challenge_id": second.ID, "accept": true}, 409, nil)
		})
	}
}
func TestAuthExpiryThroughHTTP(t *testing.T) {
	for _, root := range []string{"/v1/sessions", "/v1/sftp"} {
		t.Run(root, func(t *testing.T) {
			c, _ := newSFTPAPIWithDeadline(t, protocolsftp.InMemHandler(), 200*time.Millisecond)
			c.call(t, http.MethodPut, "/v1/secrets/servers/"+c.serverRef+"/password", map[string]any{"password": "incorrect-fixture-password"}, 200, nil)
			var resource struct {
				ID string `json:"id"`
			}
			c.call(t, http.MethodPost, root, map[string]any{"server_ref": c.serverRef, "host_key_policy": "insecure-skip", "allow_auth_retry": true}, 201, &resource)
			ch := waitReviewChallenge(t, c, root+"/"+resource.ID, "auth")
			deadline := time.Now().Add(2 * time.Second)
			for {
				var got struct {
					State       string `json:"state"`
					FailureCode string `json:"failure_code"`
				}
				c.call(t, http.MethodGet, root+"/"+resource.ID, nil, 200, &got)
				if got.State == "failed" {
					if got.FailureCode != "AUTH_CHALLENGE_EXPIRED" {
						t.Fatal(got)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("auth expiry not settled")
				}
				time.Sleep(time.Millisecond)
			}
			c.call(t, http.MethodPost, root+"/"+resource.ID+"/challenges/auth", map[string]any{"challenge_id": ch.ID, "password": "artificial-password"}, 409, nil)
		})
	}
}

func TestRenameWithoutPosixExtensionReturnsDestinationConflict(t *testing.T) {
	// The library exposes server extension configuration globally. This package
	// deliberately has no parallel tests; cleanup restores it after server stop.
	if err := protocolsftp.SetSFTPExtensions(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := protocolsftp.SetSFTPExtensions("hardlink@openssh.com", "posix-rename@openssh.com", "statvfs@openssh.com"); err != nil {
			t.Error(err)
		}
	})
	handlers := protocolsftp.InMemHandler()
	for _, path := range []string{"/from", "/to"} {
		w, err := handlers.FilePut.Filewrite(&protocolsftp.Request{Method: "Put", Filepath: path, Flags: 0x1a})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.WriteAt([]byte("fixture"), 0); err != nil {
			t.Fatal(err)
		}
		if c, ok := w.(io.Closer); ok {
			_ = c.Close()
		}
	}
	c, _ := newSFTPAPI(t, handlers)
	var resource struct {
		ID string `json:"id"`
	}
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": c.serverRef, "host_key_policy": "insecure-skip"}, 201, &resource)
	route := "/v1/sftp/" + resource.ID
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.State == "open" })
	c.call(t, http.MethodPost, route+"/rename", map[string]any{"old_path": "/from", "new_path": "/to", "overwrite": true}, 409, nil)
	c.call(t, http.MethodGet, route+"/files?path=%2Ffrom&stat=true", nil, 200, nil)
	c.call(t, http.MethodGet, route+"/files?path=%2Fto&stat=true", nil, 200, nil)
}

func TestProxyCapacity409ThroughPublicHTTP(t *testing.T) {
	c, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
	remote := proxyserver.New(t, proxyserver.Config{Type: "socks5", Stall: true})
	host, p, _ := net.SplitHostPort(remote.Address())
	port, _ := strconv.Atoi(p)
	payload, err := json.Marshal(map[string]any{"draft": map[string]any{"type": "socks5", "host": host, "port": port}, "timeout_ms": 60000})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/proxy-tests", bytes.NewReader(payload))
			if err != nil {
				return
			}
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.http.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for remote.ConnectionCount() != 16 {
		if time.Now().After(deadline) {
			t.Fatal("probe capacity did not fill", remote.ConnectionCount())
		}
		time.Sleep(time.Millisecond)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/v1/proxy-tests", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 409 || !bytes.Contains(raw, []byte(`"code":"RESOURCE_LIMIT"`)) {
		t.Fatal("capacity response", resp.StatusCode, string(raw))
	}
}
