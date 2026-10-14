package integration

import (
	"bytes"
	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/testutil/proxyserver"
	"knot-core/pkg/proxytest"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
)

func TestProxyDraftSavedCredentialsAndFailuresThroughPublicAPI(t *testing.T) {
	c, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
	echo := echoTarget(t, "tcp", "127.0.0.1:0")
	var summary struct {
		Metadata struct {
			ConfigPath string `json:"config_path"`
		} `json:"metadata"`
	}
	c.call(t, http.MethodGet, "/v1/config", nil, 200, &summary)
	for _, typ := range []string{"socks5", "http"} {
		t.Run(typ, func(t *testing.T) {
			remote := proxyserver.New(t, proxyserver.Config{Type: typ, Username: "fixture", Password: "artificial-proxy-password"})
			host, portText, _ := net.SplitHostPort(remote.Address())
			port, _ := strconv.Atoi(portText)
			var saved struct {
				ID string `json:"id"`
			}
			c.call(t, http.MethodPost, "/v1/config/proxies", map[string]any{"alias": typ, "type": typ, "host": host, "port": port, "username": "fixture"}, 201, &saved)
			c.call(t, http.MethodPut, "/v1/secrets/proxies/"+saved.ID+"/password", map[string]any{"password": "artificial-proxy-password"}, 200, nil)
			before, err := os.ReadFile(summary.Metadata.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			var result proxytest.Result
			c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"proxy_id": saved.ID, "target": echo}, 200, &result)
			if !result.Passed || result.Scope != "tunnel" {
				t.Fatal(result)
			}
			// A complete draft with an omitted password retains this protocol's saved secret.
			draft := map[string]any{"type": typ, "host": host, "port": port, "username": "fixture"}
			c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"proxy_id": saved.ID, "draft": draft, "target": echo}, 200, &result)
			if !result.Passed {
				t.Fatal(result)
			}
			// Moving the draft endpoint cannot send the original secret there.
			other := proxyserver.New(t, proxyserver.Config{Type: typ, Username: "fixture", Password: ""})
			otherHost, otherPortText, _ := net.SplitHostPort(other.Address())
			otherPort, _ := strconv.Atoi(otherPortText)
			moved := map[string]any{"type": typ, "host": otherHost, "port": otherPort, "username": "fixture"}
			c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"proxy_id": saved.ID, "draft": moved, "target": echo}, 200, &result)
			if !result.Passed {
				t.Fatal("saved password leaked to moved endpoint", result)
			}
			draft["password"] = ""
			c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"proxy_id": saved.ID, "draft": draft, "target": echo}, 200, &result)
			if result.Passed || result.FailureCode != "PROXY_AUTH_FAILED" {
				t.Fatal("explicit empty password reused secret", result)
			}
			draft["password"] = "artificial-proxy-password"
			c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"draft": draft}, 200, &result)
			scope := "endpoint"
			if typ == "socks5" {
				scope = "negotiation"
			}
			if !result.Passed || result.Scope != scope {
				t.Fatal(result)
			}
			after, _ := os.ReadFile(summary.Metadata.ConfigPath)
			if !bytes.Equal(before, after) {
				t.Fatal("proxy test changed persisted configuration")
			}
		})
	}
	for _, entry := range []struct {
		cfg     proxyserver.Config
		code    string
		timeout int
	}{{proxyserver.Config{Type: "socks5", RejectNegotiation: true}, "PROXY_NEGOTIATION_FAILED", 0}, {proxyserver.Config{Type: "http", RejectTarget: true}, "PROXY_TARGET_FAILED", 0}, {proxyserver.Config{Type: "socks5", Stall: true}, "PROXY_TEST_TIMEOUT", 30}} {
		remote := proxyserver.New(t, entry.cfg)
		host, p, _ := net.SplitHostPort(remote.Address())
		port, _ := strconv.Atoi(p)
		var result proxytest.Result
		c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"draft": map[string]any{"type": entry.cfg.Type, "host": host, "port": port}, "target": echo, "timeout_ms": entry.timeout}, 200, &result)
		if result.Passed || result.FailureCode != entry.code {
			t.Fatalf("probe scope/result: %+v want %s", result, entry.code)
		}
	}
	c.call(t, http.MethodPost, "/v1/proxy-tests", map[string]any{"draft": map[string]any{"type": "unknown", "host": "localhost", "port": 80}}, 400, nil)
}
