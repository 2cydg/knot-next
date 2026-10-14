package integration

import (
	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"net/http"
	"testing"
	"time"
)

func TestCrossConnectionFollowSwitchClearAndSourceLoss(t *testing.T) {
	c, remote := newSFTPAPI(t, protocolsftp.InMemHandler(), "artificial-second-password")
	started1, started2, output1, output2, exit1, exit2 := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	remote.SetShellBehavior(sshserver.ShellBehavior{Started: started1, Scripted: true, BeforeExit: exit1, SendExitStatus: true, Writes: []sshserver.ScriptedWrite{{Gate: output1, Data: []byte("\x1b]7;file://remote/a\a")}}})
	first := gapSSH(t, c, "insecure-skip")
	select {
	case <-started1:
	case <-time.After(time.Second):
		t.Fatal("shell not started")
	}
	remote.SetShellBehavior(sshserver.ShellBehavior{Started: started2, Scripted: true, BeforeExit: exit2, SendExitStatus: true, Writes: []sshserver.ScriptedWrite{{Gate: output2, Data: []byte("\x1b]7;file://remote/b\a")}}})
	c.call(t, http.MethodPut, "/v1/secrets/servers/"+c.serverRef+"/password", map[string]any{"password": "artificial-second-password"}, 200, nil)
	second := gapSSH(t, c, "insecure-skip")
	select {
	case <-started2:
	case <-time.After(time.Second):
		t.Fatal("shell not started")
	}
	if first.ConnectionID == second.ConnectionID {
		t.Fatal("fixture did not establish distinct transports")
	}
	var other struct {
		ID string `json:"id"`
	}
	c.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"alias": "different-server", "host": remote.Host(), "port": remote.Port(), "user": "sftp-test", "auth_method": "password"}, 201, &other)
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": other.ID, "follow_session_id": first.ID, "follow_connection_id": first.ConnectionID}, 400, nil)
	var candidates []session.Resource
	c.call(t, http.MethodGet, "/v1/sessions?server_id="+c.serverRef+"&active=true", nil, 200, &candidates)
	if len(candidates) != 2 {
		t.Fatal(candidates)
	}
	var files sftp.Session
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": c.serverRef, "connection_id": first.ConnectionID, "follow_session_id": second.ID, "follow_connection_id": first.ConnectionID}, 400, nil)
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": c.serverRef, "connection_id": first.ConnectionID, "follow_session_id": second.ID, "follow_connection_id": second.ConnectionID}, 201, &files)
	originalID := files.ID
	route := "/v1/sftp/" + files.ID
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.State == "open" })
	for _, dir := range []string{"/a", "/b", "/manual"} {
		c.call(t, http.MethodPost, route+"/dirs", map[string]any{"path": dir}, 201, nil)
	}
	close(output1)
	close(output2)
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.CurrentDir == "/b" })
	c.call(t, http.MethodGet, route, nil, 200, &files)
	if files.ConnectionID != first.ConnectionID || files.FollowConnectionID != second.ConnectionID {
		t.Fatal(files)
	}
	// Wrong pairing must keep the existing binding and directory intact.
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "set-follow", "follow_session_id": first.ID, "follow_connection_id": second.ConnectionID}, 400, nil)
	c.call(t, http.MethodGet, route, nil, 200, &files)
	if files.FollowSessionID != second.ID || files.CurrentDir != "/b" {
		t.Fatal("failed switch modified binding")
	}
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "set-follow", "follow_session_id": first.ID, "follow_connection_id": first.ConnectionID}, 200, &files)
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.CurrentDir == "/a" })
	if files.ID != originalID || files.ConnectionID != first.ConnectionID {
		t.Fatal("switch rebuilt transport")
	}
	close(exit1)
	waitCWD(t, c, route, func(r cwdSessionJSON) bool {
		return r.FollowState == "invalid" && r.State == "open" && r.CurrentDir == "/a"
	})
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "set-follow", "follow_session_id": second.ID, "follow_connection_id": second.ConnectionID}, 200, &files)
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.CurrentDir == "/b" })
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "cd", "path": "/missing"}, 400, nil)
	c.call(t, http.MethodGet, route, nil, 200, &files)
	if files.FollowState != "active" {
		t.Fatal("failed cd paused follow")
	}
	files = sftp.Session{}
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "clear-follow"}, 200, &files)
	if files.FollowSessionID != "" || files.FollowConnectionID != "" || files.FollowState != "" || files.ConnectionID != first.ConnectionID || files.CurrentDir != "/b" {
		t.Fatalf("clear semantics: %+v", files)
	}
	close(exit2)
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "cd", "path": "/manual"}, 200, &files)
	if files.FollowState != "" {
		t.Fatal("old source callback restored binding")
	}
	c.call(t, http.MethodDelete, route, nil, 200, nil)
}

func TestFollowSubscriptionCapacityRollbackAndIdempotency(t *testing.T) {
	c, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
	first := gapSSH(t, c, "insecure-skip")
	second := gapSSH(t, c, "insecure-skip")
	var firstTarget string
	for _, source := range []session.Resource{first, second} {
		for i := 0; i < 8; i++ {
			var created sftp.Session
			c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": c.serverRef, "connection_id": source.ConnectionID, "follow_session_id": source.ID, "follow_connection_id": source.ConnectionID}, 201, &created)
			waitCWD(t, c, "/v1/sftp/"+created.ID, func(r cwdSessionJSON) bool { return r.State == "open" })
			if firstTarget == "" {
				firstTarget = created.ID
			}
		}
	}
	route := "/v1/sftp/" + firstTarget
	// Both source subscriber capacities are full; same-source requests need no new slot.
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "set-follow", "follow_session_id": first.ID, "follow_connection_id": first.ConnectionID}, 200, nil)
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "set-follow", "follow_session_id": second.ID, "follow_connection_id": second.ConnectionID}, 409, nil)
	var got sftp.Session
	c.call(t, http.MethodGet, route, nil, 200, &got)
	if got.FollowSessionID != first.ID || got.FollowState != "active" {
		t.Fatal("failed subscription replaced old binding", got)
	}
}
