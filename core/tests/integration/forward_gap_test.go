package integration

import (
	"bytes"
	protocolsftp "github.com/pkg/sftp"
	"io"
	"knot-core/internal/proxyproto"
	"knot-core/pkg/config"
	"knot-core/pkg/forward"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"
	"net"
	"net/http"
	"testing"
	"time"
)

func waitForward(t *testing.T, c *sftpAPIClient, id, state string) forward.View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got forward.View
		c.call(t, http.MethodGet, "/v1/forwards/"+id, nil, 200, &got)
		if got.State == state {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("forward state: %+v want %s", got, state)
		}
		time.Sleep(time.Millisecond)
	}
}
func echoTarget(t *testing.T, network, address string) string {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = listener.Close(); <-done })
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String()
}
func roundtripForward(t *testing.T, address string, dynamic bool, target string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if dynamic {
		conn, err = proxyproto.Negotiate(t.Context(), conn, proxyproto.Options{Type: "socks5"}, target, time.Second)
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	payload := bytes.Repeat([]byte{0, 255, '\r', '\n', 'a', 0xe4, 0xb8, 0xad}, 8192)
	go func() { _, _ = conn.Write(payload) }()
	received := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, received); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("forward changed bytes")
	}
}
func gapSSH(t *testing.T, c *sftpAPIClient, policy string) session.Resource {
	t.Helper()
	var shell session.Resource
	c.call(t, http.MethodPost, "/v1/sessions", map[string]any{"server_ref": c.serverRef, "host_key_policy": policy}, 201, &shell)
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.call(t, http.MethodGet, "/v1/sessions/"+shell.ID, nil, 200, &shell)
		if shell.State == "connected" {
			return shell
		}
		if shell.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("SSH: %+v", shell)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestForwardLocalRemoteDynamicThroughPublicAPI(t *testing.T) {
	c, remote := newSFTPAPI(t, protocolsftp.InMemHandler())
	echo := echoTarget(t, "tcp", "127.0.0.1:0")
	c.call(t, http.MethodPost, "/v1/forwards", map[string]any{"server_id": c.serverRef, "type": "local", "bind": "127.0.0.1:0", "target": echo}, 409, nil)
	shell := gapSSH(t, c, "insecure-skip")
	var ids []string
	for _, typ := range []string{"local", "remote", "dynamic"} {
		target := echo
		if typ == "dynamic" {
			target = ""
		}
		var created forward.View
		c.call(t, http.MethodPost, "/v1/forwards", map[string]any{"server_id": c.serverRef, "type": typ, "bind": "127.0.0.1:0", "target": target}, 202, &created)
		if created.ConnectionID != shell.ConnectionID || created.Origin != "temporary" {
			t.Fatalf("wrong binding: %+v", created)
		}
		ready := waitForward(t, c, created.ID, "active")
		roundtripForward(t, ready.BoundAddress, typ == "dynamic", echo)
		ids = append(ids, created.ID)
	}
	if remote.ForwardCount() < 2 {
		t.Fatal("local/dynamic did not open real direct-tcpip channels")
	}
	// Closing the shell releases its own channel and lease; forwarding continues.
	c.call(t, http.MethodDelete, "/v1/sessions/"+shell.ID, nil, 200, nil)
	for i, id := range ids {
		ready := waitForward(t, c, id, "active")
		roundtripForward(t, ready.BoundAddress, i == 2, echo)
	}
	var status struct {
		ActiveForwards int `json:"active_forwards"`
	}
	c.call(t, http.MethodGet, "/v1/status", nil, 200, &status)
	if status.ActiveForwards != 3 {
		t.Fatal(status)
	}
	for _, id := range ids {
		c.call(t, http.MethodPost, "/v1/forwards/"+id+"/control", map[string]any{"op": "stop"}, 200, nil)
		waitForward(t, c, id, "stopped")
	}
	// Runtime restart uses the original connection and definition.
	c.call(t, http.MethodPost, "/v1/forwards/"+ids[0]+"/control", map[string]any{"op": "start"}, 200, nil)
	roundtripForward(t, waitForward(t, c, ids[0], "active").BoundAddress, false, echo)
	c.call(t, http.MethodDelete, "/v1/forwards/"+ids[0], nil, 200, nil)
	waitForward(t, c, ids[0], "deleted")
	var definitions []config.ForwardDefinition
	c.call(t, http.MethodGet, "/v1/config/servers/"+c.serverRef+"/forwards", nil, 200, &definitions)
	if len(definitions) != 0 {
		t.Fatal("temporary forward persisted")
	}
}
func TestAutoForwardsSnapshotDedupLatestAndConflict(t *testing.T) {
	c, remote := newSFTPAPI(t, protocolsftp.InMemHandler())
	echo := echoTarget(t, "tcp", "127.0.0.1:0")
	var definitions []config.ForwardDefinition
	c.call(t, http.MethodPut, "/v1/config/servers/"+c.serverRef+"/forwards", map[string]any{"forwards": []map[string]any{{"type": "local", "bind": "127.0.0.1:0", "target": echo, "enabled": true}}}, 200, &definitions)
	first := gapSSH(t, c, "insecure-skip")
	var instances []forward.View
	c.call(t, http.MethodGet, "/v1/forwards", nil, 200, &instances)
	if len(instances) != 1 || instances[0].DefinitionID != definitions[0].ID || instances[0].ConnectionID != first.ConnectionID {
		t.Fatalf("auto instances: %+v", instances)
	}
	active := waitForward(t, c, instances[0].ID, "active")
	roundtripForward(t, active.BoundAddress, false, echo)
	reused := gapSSH(t, c, "insecure-skip")
	if reused.ConnectionID != first.ConnectionID {
		t.Fatal("expected shared transport")
	}
	c.call(t, http.MethodGet, "/v1/forwards", nil, 200, &instances)
	if len(instances) != 1 {
		t.Fatal("reuse restarted automatic rule")
	}
	// Editing definitions does not hot-update existing connection instances.
	c.call(t, http.MethodPut, "/v1/config/servers/"+c.serverRef+"/forwards", map[string]any{"forwards": []map[string]any{{"type": "local", "bind": active.BoundAddress, "target": echo, "enabled": true}}}, 200, nil)
	second := gapSSH(t, c, "accept-new")
	if second.ConnectionID == first.ConnectionID {
		t.Fatal("expected different transport identity")
	}
	c.call(t, http.MethodGet, "/v1/forwards", nil, 200, &instances)
	if len(instances) != 2 {
		t.Fatalf("missing auto conflict: %+v", instances)
	}
	var secondID string
	for _, v := range instances {
		if v.ConnectionID == second.ConnectionID {
			secondID = v.ID
		}
	}
	waitForward(t, c, secondID, "failed")
	gapSSH(t, c, "insecure-skip") // Reusing the older connection must not change latest.
	var temporary forward.View
	c.call(t, http.MethodPost, "/v1/forwards", map[string]any{"server_id": c.serverRef, "type": "dynamic", "bind": "127.0.0.1:0"}, 202, &temporary)
	if temporary.ConnectionID != second.ConnectionID {
		t.Fatal("latest transport ordering changed on session reuse")
	}
	ready := waitForward(t, c, temporary.ID, "active")
	roundtripForward(t, ready.BoundAddress, true, echo)
	var connections []sshpool.Connection
	c.call(t, http.MethodGet, "/v1/connections?server_id="+c.serverRef, nil, 200, &connections)
	if len(connections) != 2 || connections[1].ID != second.ConnectionID || connections[1].Sequence <= connections[0].Sequence {
		t.Fatalf("connection query: %+v", connections)
	}
	remote.DropConnections()
	waitForward(t, c, temporary.ID, "failed")
}

func TestIPv6ForwardsAndDynamicProtocolFailuresThroughAPI(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	_ = probe.Close()
	c, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
	echo := echoTarget(t, "tcp6", "[::1]:0")
	gapSSH(t, c, "insecure-skip")
	for _, typ := range []string{"local", "remote", "dynamic"} {
		target := echo
		if typ == "dynamic" {
			target = ""
		}
		var v forward.View
		c.call(t, http.MethodPost, "/v1/forwards", map[string]any{"server_id": c.serverRef, "type": typ, "bind": "[::1]:0", "target": target}, 202, &v)
		v = waitForward(t, c, v.ID, "active")
		roundtripForward(t, v.BoundAddress, typ == "dynamic", echo)
		if typ == "dynamic" {
			conn, err := net.DialTimeout("tcp", v.BoundAddress, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, _ = conn.Write([]byte{5, 1, 0})
			var method [2]byte
			if _, err = io.ReadFull(conn, method[:]); err != nil {
				t.Fatal(err)
			}
			_, _ = conn.Write([]byte{5, 2, 0, 1, 127, 0, 0, 1, 0, 22})
			var reply [10]byte
			if _, err = io.ReadFull(conn, reply[:]); err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			if reply[1] != 7 {
				t.Fatal("unsupported SOCKS command accepted", reply)
			}
		}
		c.call(t, http.MethodDelete, "/v1/forwards/"+v.ID, nil, 200, nil)
	}
}
