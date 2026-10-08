package sftp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"knot-core/pkg/config"
	"knot-core/pkg/session"
	"knot-core/pkg/sshpool"
)

type testRuntimeConfigProvider struct {
	cfg config.RuntimeConfig
}

func (p *testRuntimeConfigProvider) RuntimeConfig() (config.RuntimeConfig, error) {
	return p.cfg, nil
}

func (p *testRuntimeConfigProvider) SetServerPassword(id string, password string) (config.ServerProfileView, error) {
	server := p.cfg.Servers[id]
	server.Password = password
	server.AuthMethod = config.AuthMethodPassword
	p.cfg.Servers[id] = server
	return config.ServerProfileView{ID: server.ID, Alias: server.Alias, PasswordSet: password != ""}, nil
}

func (p *testRuntimeConfigProvider) UpdateServer(id string, server config.ServerProfile) (config.ServerProfileView, error) {
	p.cfg.Servers[id] = server
	return config.ServerProfileView{ID: server.ID, Alias: server.Alias, AuthMethod: server.AuthMethod, KeyID: server.KeyID}, nil
}

type fakeSessionProvider struct {
	resource session.Resource
	events   chan session.CWDNotify
}

func (p *fakeSessionProvider) Get(id string) (session.Resource, error) {
	return p.resource, nil
}

func (p *fakeSessionProvider) SubscribeCWD(id string) (<-chan session.CWDNotify, func(), session.Resource, error) {
	return p.events, func() {}, p.resource, nil
}

func TestCreateFollowSessionUpdatesCurrentDir(t *testing.T) {
	root := t.TempDir()
	cfg := config.RuntimeConfig{
		Servers: map[string]config.ServerProfile{
			"srv": {ID: "srv", Alias: "web"},
		},
		Proxies:       map[string]config.ProxyProfile{},
		Keys:          map[string]config.KeyMetadata{},
		SyncProviders: map[string]config.SyncProviderConfig{},
	}
	followEvents := make(chan session.CWDNotify, 2)
	sessionSvc := &fakeSessionProvider{
		resource: session.Resource{ID: "1", ServerRef: "srv", Alias: "web"},
		events:   followEvents,
	}
	sftpSvc := NewService(filepath.Join(root, "sftp"))
	sftpSvc.UseConfig(&testRuntimeConfigProvider{cfg: cfg})
	sftpSvc.UseSession(sessionSvc)
	sftpSvc.UsePool(sshpool.NewPool())
	sftpSvc.UseLocalTestBackend()
	sftpSession, err := sftpSvc.Create(CreateRequest{ServerRef: "srv", FollowSessionID: "1"})
	if err != nil {
		t.Fatalf("create sftp session: %v", err)
	}
	if sftpSession.CurrentDir != "/" {
		t.Fatalf("initial current dir = %q, want /", sftpSession.CurrentDir)
	}
	if _, err := sftpSvc.Mkdir(sftpSession.ID, "/var/www", true); err != nil {
		t.Fatal(err)
	}
	followEvents <- session.CWDNotify{SessionID: "1", Path: "/var/www", Time: time.Now().UTC()}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err := sftpSvc.Get(sftpSession.ID)
		if err != nil {
			t.Fatalf("get sftp session: %v", err)
		}
		if current.CurrentDir == "/var/www" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := sftpSvc.Get(sftpSession.ID)
	t.Fatalf("current dir = %q, want /var/www", current.CurrentDir)
}

func TestUploadDirectoryRecursiveCreatesTransfer(t *testing.T) {
	root := t.TempDir()
	cfg := config.RuntimeConfig{
		Servers: map[string]config.ServerProfile{
			"srv": {ID: "srv", Alias: "web"},
		},
		Proxies:       map[string]config.ProxyProfile{},
		Keys:          map[string]config.KeyMetadata{},
		SyncProviders: map[string]config.SyncProviderConfig{},
	}
	svc := NewService(filepath.Join(root, "sftp"))
	svc.UseConfig(&testRuntimeConfigProvider{cfg: cfg})
	svc.UsePool(sshpool.NewPool())
	svc.UseLocalTestBackend()

	sessionRes, err := svc.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	transfer, err := svc.Upload(sessionRes.ID, TransferRequest{
		Source:    srcDir,
		Target:    "/dest/",
		Recursive: true,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	final := waitTransferState(t, svc, sessionRes.ID, transfer.ID)
	if final.State != "completed" || final.FilesDone != 1 {
		t.Fatalf("transfer = %+v", final)
	}
	got, err := os.ReadFile(filepath.Join(sessionRes.Root, "dest", "src", "nested", "a.txt"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "abc" {
		t.Fatalf("uploaded content = %q", got)
	}
}

func TestBatchUploadAndTransferList(t *testing.T) {
	root := t.TempDir()
	cfg := config.RuntimeConfig{
		Servers: map[string]config.ServerProfile{
			"srv": {ID: "srv", Alias: "web"},
		},
		Proxies:       map[string]config.ProxyProfile{},
		Keys:          map[string]config.KeyMetadata{},
		SyncProviders: map[string]config.SyncProviderConfig{},
	}
	svc := NewService(filepath.Join(root, "sftp"))
	svc.UseConfig(&testRuntimeConfigProvider{cfg: cfg})
	svc.UsePool(sshpool.NewPool())
	svc.UseLocalTestBackend()

	sessionRes, err := svc.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, []byte("bb"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	transfer, err := svc.BatchUpload(sessionRes.ID, BatchTransferRequest{
		Sources:   []string{a, b},
		Target:    "/batch/",
		Overwrite: true,
	})
	if err != nil {
		t.Fatalf("batch upload: %v", err)
	}
	final := waitTransferState(t, svc, sessionRes.ID, transfer.ID)
	if final.State != "completed" {
		t.Fatalf("transfer = %+v", final)
	}
	if len(final.Items) != 2 || final.Items[0].State != "completed" || final.Items[1].State != "completed" {
		t.Fatalf("transfer items = %+v", final.Items)
	}
	list, err := svc.ListTransfers(sessionRes.ID)
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected transfer list to include batch upload")
	}
}

func TestGlobMatchesAndBatchDownload(t *testing.T) {
	root := t.TempDir()
	cfg := config.RuntimeConfig{
		Servers: map[string]config.ServerProfile{
			"srv": {ID: "srv", Alias: "web"},
		},
		Proxies:       map[string]config.ProxyProfile{},
		Keys:          map[string]config.KeyMetadata{},
		SyncProviders: map[string]config.SyncProviderConfig{},
	}
	svc := NewService(filepath.Join(root, "sftp"))
	svc.UseConfig(&testRuntimeConfigProvider{cfg: cfg})
	svc.UsePool(sshpool.NewPool())
	svc.UseLocalTestBackend()

	sessionRes, err := svc.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sessionRes.Root, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionRes.Root, "logs", "a.log"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a.log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionRes.Root, "logs", "b.log"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b.log: %v", err)
	}

	matches, err := svc.GlobMatches(sessionRes.ID, "/logs/*.log", false, true)
	if err != nil {
		t.Fatalf("glob matches: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(matches))
	}

	targetDir := filepath.Join(root, "downloads")
	transfer, err := svc.BatchDownload(sessionRes.ID, BatchTransferRequest{
		Sources:   []string{"/logs/*.log"},
		Target:    targetDir,
		Overwrite: true,
	})
	if err != nil {
		t.Fatalf("batch download: %v", err)
	}
	final := waitTransferState(t, svc, sessionRes.ID, transfer.ID)
	if final.State != "completed" {
		t.Fatalf("transfer = %+v", final)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "a.log")); err != nil {
		t.Fatalf("missing a.log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "b.log")); err != nil {
		t.Fatalf("missing b.log: %v", err)
	}
}

func waitTransferState(t *testing.T, svc *Service, sessionID string, transferID string) Transfer {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		transfer, err := svc.Transfer(sessionID, transferID)
		if err != nil {
			t.Fatalf("get transfer: %v", err)
		}
		switch transfer.State {
		case "completed", "failed", "partial_failed", "canceled":
			return transfer
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for transfer %s", transferID)
	return Transfer{}
}

func (p *testRuntimeConfigProvider) RememberServerAuth(id string, choice config.AuthChoice) error {
	profile := p.cfg.Servers[id]
	profile.AuthMethod, profile.KeyID, profile.Password = choice.Method, choice.KeyID, choice.Password
	p.cfg.Servers[id] = profile
	return nil
}
