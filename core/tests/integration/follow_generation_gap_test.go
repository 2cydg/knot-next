package integration

import (
	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/sftp"
	"net/http"
	"sync"
	"testing"
	"time"
)

type blockedFollowList struct {
	protocolsftp.FileLister
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockedFollowList) Filelist(req *protocolsftp.Request) (protocolsftp.ListerAt, error) {
	if req.Method == "List" && req.Filepath == "/old" {
		l.once.Do(func() { close(l.entered) })
		<-l.release
	}
	return l.FileLister.Filelist(req)
}
func TestFollowClearRejectsLateDirectoryValidationThroughAPI(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	blocked := &blockedFollowList{FileLister: handlers.FileList, entered: make(chan struct{}), release: make(chan struct{})}
	handlers.FileList = blocked
	c, remote := newSFTPAPI(t, handlers)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(blocked.release) }) })
	output, exit := make(chan struct{}), make(chan struct{})
	remote.SetShellBehavior(sshserver.ShellBehavior{Scripted: true, SendExitStatus: true, BeforeExit: exit, Writes: []sshserver.ScriptedWrite{{Gate: output, Data: []byte("\x1b]7;file://remote/old\a")}}})
	shell := gapSSH(t, c, "insecure-skip")
	var files sftp.Session
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": c.serverRef, "connection_id": shell.ConnectionID}, 201, &files)
	route := "/v1/sftp/" + files.ID
	waitCWD(t, c, route, func(r cwdSessionJSON) bool { return r.State == "open" })
	for _, dir := range []string{"/old", "/manual"} {
		c.call(t, http.MethodPost, route+"/dirs", map[string]any{"path": dir}, 201, nil)
	}
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "cd", "path": "/manual"}, 200, nil)
	close(output)
	waitCWD(t, c, "/v1/sessions/"+shell.ID, func(r cwdSessionJSON) bool { return r.CurrentDir == "/old" })
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "set-follow", "follow_session_id": shell.ID, "follow_connection_id": shell.ConnectionID}, 200, nil)
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("old directory validation did not start")
	}
	c.call(t, http.MethodPost, route+"/control", map[string]any{"op": "clear-follow"}, 200, nil)
	once.Do(func() { close(blocked.release) })
	// A subsequent round trip confirms the same subsystem is still usable after
	// the old protocol result; it must not restore the cleared relationship.
	c.call(t, http.MethodGet, route+"/files?path=%2F&stat=true", nil, 200, nil)
	var current sftp.Session
	c.call(t, http.MethodGet, route, nil, 200, &current)
	if current.CurrentDir != "/manual" || current.FollowSessionID != "" || current.FollowState != "" || current.ConnectionID != shell.ConnectionID {
		t.Fatalf("stale follow write: %+v", current)
	}
	close(exit)
	c.call(t, http.MethodDelete, route, nil, 200, nil)
}
