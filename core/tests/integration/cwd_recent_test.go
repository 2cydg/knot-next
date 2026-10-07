package integration

import (
	"net/http"
	"testing"
	"time"

	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/testutil/sshserver"
)

type cwdSessionJSON struct {
	ID            string     `json:"id"`
	ServerID      string     `json:"server_id"`
	State         string     `json:"state"`
	CurrentDir    string     `json:"current_dir"`
	CWDUpdatedAt  *time.Time `json:"cwd_updated_at"`
	FollowState   string     `json:"follow_state"`
	FollowError   string     `json:"follow_error"`
	Attached      bool       `json:"attached"`
	StartedAt     time.Time  `json:"started_at"`
	FrameworkCode string     `json:"framework_code"`
	ForwardAgent  bool       `json:"forward_agent"`
}

func waitCWD(t *testing.T, c *sftpAPIClient, route string, predicate func(cwdSessionJSON) bool) cwdSessionJSON {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var got cwdSessionJSON
		c.call(t, http.MethodGet, route, nil, http.StatusOK, &got)
		if predicate(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("CWD wait %s: %+v", route, got)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestOSC7FollowThroughPublicAPIAndRecentSuccessOnly(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	client, remote := newSFTPAPI(t, handlers)
	first, second, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	remote.SetShellBehavior(sshserver.ShellBehavior{Scripted: true, SendExitStatus: true, BeforeExit: exit, Writes: []sshserver.ScriptedWrite{
		{Gate: first, Data: []byte("\x00\xff\x1b]7;file://remote/a%20%E4%B8%AD\a"), ChunkSize: 1},
		{Gate: second, Data: []byte("\x1b]7;file://remote/b\x1b\\"), ChunkSize: 2},
	}})
	var shell cwdSessionJSON
	client.call(t, http.MethodPost, "/v1/sessions", map[string]any{"server_ref": client.serverRef, "host_key_policy": "insecure-skip"}, http.StatusCreated, &shell)
	sourceRoute := "/v1/sessions/" + shell.ID
	waitCWD(t, client, sourceRoute, func(r cwdSessionJSON) bool { return r.State == "connected" })
	var files cwdSessionJSON
	client.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": client.serverRef, "follow_session_id": shell.ID, "host_key_policy": "insecure-skip"}, http.StatusCreated, &files)
	fileRoute := "/v1/sftp/" + files.ID
	waitCWD(t, client, fileRoute, func(r cwdSessionJSON) bool { return r.State == "open" })
	for _, dir := range []string{"/a 中", "/b", "/manual"} {
		client.call(t, http.MethodPost, fileRoute+"/dirs", map[string]any{"path": dir, "recursive": true}, http.StatusCreated, nil)
	}
	close(first)
	got := waitCWD(t, client, fileRoute, func(r cwdSessionJSON) bool { return r.CurrentDir == "/a 中" })
	if got.CWDUpdatedAt == nil || got.FollowState != "active" || got.ServerID != client.serverRef {
		t.Fatalf("missing candidate/follow fields: %+v", got)
	}
	client.call(t, http.MethodPost, fileRoute+"/control", map[string]any{"op": "cd", "path": "/manual"}, http.StatusOK, &got)
	if got.FollowState != "paused" {
		t.Fatal("manual cd did not pause")
	}
	close(second)
	waitCWD(t, client, sourceRoute, func(r cwdSessionJSON) bool { return r.CurrentDir == "/b" })
	client.call(t, http.MethodGet, fileRoute, nil, http.StatusOK, &got)
	if got.CurrentDir != "/manual" {
		t.Fatal("paused follow overwrote manual cd")
	}
	client.call(t, http.MethodPost, fileRoute+"/control", map[string]any{"op": "resume-follow"}, http.StatusOK, &got)
	if got.CurrentDir != "/b" {
		t.Fatal("resume did not read latest source")
	}
	close(exit)
	waitCWD(t, client, fileRoute, func(r cwdSessionJSON) bool { return r.FollowState == "invalid" && r.CurrentDir == "/b" })
	var page struct {
		Items []struct {
			ID       string     `json:"id"`
			LastUsed *time.Time `json:"last_used"`
		} `json:"items"`
	}
	client.call(t, http.MethodGet, "/v1/config/servers?sort=recent", nil, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Items[0].LastUsed == nil {
		t.Fatal("successful targets did not record last_used")
	}
	used := *page.Items[0].LastUsed
	// A distinct profile has no successful use and must remain null.
	var failedProfile struct {
		ID string `json:"id"`
	}
	client.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"alias": "never-used", "host": remote.Host(), "port": remote.Port(), "user": "sftp-test", "auth_method": "password"}, http.StatusCreated, &failedProfile)
	client.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": failedProfile.ID, "command": "unlogged-command-sentinel", "host_key_policy": "insecure-skip"}, http.StatusOK, nil)
	client.call(t, http.MethodGet, "/v1/config/servers?sort=recent", nil, http.StatusOK, &page)
	if page.Items[0].ID != client.serverRef || !page.Items[0].LastUsed.Equal(used) || page.Items[1].LastUsed != nil {
		t.Fatal("failed connection changed history")
	}
	var candidates []cwdSessionJSON
	client.call(t, http.MethodGet, "/v1/sessions", nil, http.StatusOK, &candidates)
	if len(candidates) == 0 || candidates[0].ID != shell.ID || candidates[0].ServerID != client.serverRef || candidates[0].StartedAt.IsZero() {
		t.Fatalf("invalid candidates: %v", candidates)
	}
	client.call(t, http.MethodDelete, fileRoute, nil, http.StatusOK, nil)
}
func TestFollowMultipleSourcesDirectoryErrorAndUnavailableForwarding(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", t.TempDir()+"/missing-agent")
	handlers := protocolsftp.InMemHandler()
	client, remote := newSFTPAPI(t, handlers)
	output, exit := make(chan struct{}), make(chan struct{})
	remote.SetShellBehavior(sshserver.ShellBehavior{Scripted: true, SendExitStatus: true, BeforeExit: exit, Writes: []sshserver.ScriptedWrite{{Gate: output, Data: []byte("\x1b]7;file://remote/missing\a")}}})
	sources := make([]cwdSessionJSON, 2)
	files := make([]cwdSessionJSON, 2)
	for i := range sources {
		client.call(t, http.MethodPost, "/v1/sessions", map[string]any{"server_ref": client.serverRef, "host_key_policy": "insecure-skip"}, http.StatusCreated, &sources[i])
		waitCWD(t, client, "/v1/sessions/"+sources[i].ID, func(r cwdSessionJSON) bool { return r.State == "connected" })
		client.call(t, http.MethodPost, "/v1/sftp", map[string]any{"server_ref": client.serverRef, "follow_session_id": sources[i].ID, "host_key_policy": "insecure-skip"}, http.StatusCreated, &files[i])
		waitCWD(t, client, "/v1/sftp/"+files[i].ID, func(r cwdSessionJSON) bool { return r.State == "open" })
	}
	if sources[0].ID == sources[1].ID || files[0].ID == files[1].ID {
		t.Fatal("unstable session IDs")
	}
	close(output)
	for i := range files {
		waitCWD(t, client, "/v1/sftp/"+files[i].ID, func(r cwdSessionJSON) bool { return r.FollowError == "directory_unavailable" && r.CurrentDir == "/" })
	}
	client.call(t, http.MethodDelete, "/v1/sessions/"+sources[0].ID, nil, http.StatusOK, nil)
	waitCWD(t, client, "/v1/sftp/"+files[0].ID, func(r cwdSessionJSON) bool { return r.FollowState == "invalid" })
	var independent cwdSessionJSON
	client.call(t, http.MethodGet, "/v1/sftp/"+files[1].ID, nil, http.StatusOK, &independent)
	if independent.FollowState != "active" {
		t.Fatal("source closure crossed session binding")
	}
	var forwarded cwdSessionJSON
	client.call(t, http.MethodPost, "/v1/sessions", map[string]any{"server_ref": client.serverRef, "host_key_policy": "insecure-skip", "forward_agent": true}, http.StatusCreated, &forwarded)
	failed := waitCWD(t, client, "/v1/sessions/"+forwarded.ID, func(r cwdSessionJSON) bool { return r.State == "failed" })
	if failed.FrameworkCode != "agent_forwarding_unavailable" || failed.ForwardAgent {
		t.Fatalf("false forwarding success: %+v", failed)
	}
	for _, file := range files {
		client.call(t, http.MethodDelete, "/v1/sftp/"+file.ID, nil, http.StatusOK, nil)
	}
	close(exit)
}

func TestRecentRecordsTargetAndNonzeroExecButNotJump(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	client, remote := newSFTPAPI(t, handlers)
	remote.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, ExitCode: 7, SendExitStatus: true})
	var target struct {
		ID string `json:"id"`
	}
	client.call(t, http.MethodPost, "/v1/config/servers", map[string]any{"alias": "jump-target", "host": remote.Host(), "port": remote.Port(), "user": "sftp-test", "auth_method": "password", "jump_host_ids": []string{client.serverRef}}, http.StatusCreated, &target)
	client.call(t, http.MethodPut, "/v1/secrets/servers/"+target.ID+"/password", map[string]any{"password": "artificial-password"}, http.StatusOK, nil)
	var result struct {
		State    string `json:"state"`
		ExitCode int    `json:"exit_code"`
	}
	client.call(t, http.MethodPost, "/v1/sessions/exec", map[string]any{"server_ref": target.ID, "command": "nonzero", "host_key_policy": "insecure-skip"}, http.StatusOK, &result)
	if result.State != "completed" || result.ExitCode != 7 {
		t.Fatalf("exec result: %+v", result)
	}
	var page struct {
		Items []struct {
			ID       string     `json:"id"`
			LastUsed *time.Time `json:"last_used"`
		} `json:"items"`
	}
	client.call(t, http.MethodGet, "/v1/config/servers?sort=recent", nil, http.StatusOK, &page)
	if len(page.Items) != 2 || page.Items[0].ID != target.ID || page.Items[0].LastUsed == nil || page.Items[1].LastUsed != nil {
		t.Fatalf("jump incorrectly recorded: %+v", page)
	}
	if remote.ForwardCount() == 0 {
		t.Fatal("fixture did not use real direct-tcpip jump")
	}
}
