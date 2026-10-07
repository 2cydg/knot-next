package sftp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	protocolsftp "github.com/pkg/sftp"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"
)

func TestManualCDPausesFollowAndSourceClosePreservesDirectory(t *testing.T) {
	svc := NewService(filepath.Join(t.TempDir(), "sftp"))
	svc.UseLocalTestBackend()
	cfg := config.RuntimeConfig{Servers: map[string]config.ServerProfile{"srv": {ID: "srv", Alias: "web"}}}
	svc.UseConfig(&testRuntimeConfigProvider{cfg: cfg})
	ch := make(chan session.CWDNotify, 8)
	source := &fakeSessionProvider{resource: session.Resource{ID: "ssh", ServerRef: "srv", Alias: "web", State: "connected"}, events: ch}
	svc.UseSession(source)
	pool := sshpool.NewPool()
	svc.UsePool(pool)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Shutdown(ctx)
		_ = pool.Shutdown(ctx)
	})
	res, err := svc.Create(CreateRequest{ServerRef: "srv", FollowSessionID: "ssh"})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/a", "/b", "/manual"} {
		if _, err := svc.Mkdir(res.ID, dir, true); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(dir, state, failure string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			got, err := svc.Get(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.CurrentDir == dir && got.FollowState == state && got.FollowError == failure {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("follow snapshot: %+v", got)
			}
			time.Sleep(time.Millisecond)
		}
	}
	ch <- session.CWDNotify{SessionID: "ssh", Path: "/a"}
	wait("/a", "active", "")
	if _, err = svc.Control(res.ID, ControlRequest{Op: "cd", Path: "/manual"}); err != nil {
		t.Fatal(err)
	}
	ch <- session.CWDNotify{SessionID: "ssh", Path: "/b"}
	// Resume explicitly reads the latest source snapshot, independent of an event
	// queue being drained while paused.
	source.resource.CurrentDir = "/b"
	if _, err = svc.Control(res.ID, ControlRequest{Op: "resume-follow"}); err != nil {
		t.Fatal(err)
	}
	wait("/b", "active", "")
	ch <- session.CWDNotify{SessionID: "ssh", Path: "/missing"}
	wait("/b", "active", "directory_unavailable")
	ch <- session.CWDNotify{SessionID: "ssh", Closed: true}
	wait("/b", "invalid", "directory_unavailable")
	if _, err = svc.Control(res.ID, ControlRequest{Op: "resume-follow"}); err == nil {
		t.Fatal("resumed invalid source")
	}
	if _, err = svc.Control(res.ID, ControlRequest{Op: "cd", Path: "/manual"}); err != nil {
		t.Fatal(err)
	}
}

func TestFollowControlValidationAndPauseResume(t *testing.T) {
	svc := NewService(filepath.Join(t.TempDir(), "sftp"))
	svc.UseLocalTestBackend()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Shutdown(ctx)
	}()
	if _, err := svc.Control("missing", ControlRequest{Op: "cd", Path: "/"}); err == nil {
		t.Fatal("missing session accepted")
	}
	res, err := svc.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []ControlRequest{{Op: "unknown"}, {Op: "cd"}, {Op: "cd", Path: "/missing"}, {Op: "pause-follow"}, {Op: "resume-follow"}} {
		if _, err := svc.Control(res.ID, req); err == nil {
			t.Fatalf("invalid control accepted: %+v", req)
		}
	}
	if _, err := svc.Mkdir(res.ID, "/dir", true); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Control(res.ID, ControlRequest{Op: "cd", Path: "dir"})
	if err != nil || got.CurrentDir != "/dir" {
		t.Fatalf("relative cd: %+v %v", got, err)
	}
	if _, err := svc.Close(res.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Control(res.ID, ControlRequest{Op: "cd", Path: "/"}); err == nil {
		t.Fatal("closed cd accepted")
	}
}

func TestCDRejectsFileAndAcceptsEmptyReadableDirectory(t *testing.T) {
	svc := NewService(filepath.Join(t.TempDir(), "sftp"))
	svc.UseLocalTestBackend()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Shutdown(ctx)
	}()
	res, err := svc.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Mkdir(res.ID, "/empty", true); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Control(res.ID, ControlRequest{Op: "cd", Path: "/empty"}); err != nil || got.CurrentDir != "/empty" {
		t.Fatalf("empty directory: %+v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(res.Root, "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Control(res.ID, ControlRequest{Op: "cd", Path: "/file"}); err == nil {
		t.Fatal("regular file accepted")
	}
	if got, err := svc.Get(res.ID); err != nil || got.CurrentDir != "/empty" {
		t.Fatalf("failed cd changed cwd: %+v %v", got, err)
	}
}

type denyDirectoryList struct {
	protocolsftp.FileLister
	lists atomic.Int32
}

func (p *denyDirectoryList) Filelist(req *protocolsftp.Request) (protocolsftp.ListerAt, error) {
	if req.Method == "List" && req.Filepath == "/denied" {
		p.lists.Add(1)
		return nil, os.ErrPermission
	}
	return p.FileLister.Filelist(req)
}

func TestRemoteCDChecksListingPermissionBeyondStat(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	lister := &denyDirectoryList{FileLister: handlers.FileList}
	handlers.FileList = lister
	remote := sshserver.New(t, sshserver.Config{User: testAuthUser, Password: testAuthPassword, SFTPHandlers: &handlers})
	svc, serverID, _ := newSFTPAuthService(t, remote, testAuthPassword)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Shutdown(ctx)
	})
	res, err := svc.Create(CreateRequest{ServerRef: serverID, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil {
		t.Fatal(err)
	}
	waitForSFTPState(t, svc, res.ID, time.Second, "open")
	if _, err := svc.Mkdir(res.ID, "/denied", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Stat(res.ID, "/denied"); err != nil {
		t.Fatalf("stat should succeed: %v", err)
	}
	if _, err := svc.Control(res.ID, ControlRequest{Op: "cd", Path: "/denied"}); err == nil {
		t.Fatal("stat succeeded but directory listing is forbidden")
	}
	if lister.lists.Load() == 0 {
		t.Fatal("listing permission was not checked")
	}
	if got, err := svc.Get(res.ID); err != nil || got.CurrentDir != "/" {
		t.Fatalf("inaccessible directory committed: %+v %v", got, err)
	}
}
