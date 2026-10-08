package sftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocol "github.com/pkg/sftp"
	"knot-core/internal/paths"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/config"
	"knot-core/pkg/crypto"
	"knot-core/pkg/sshpool"
)

func TestReviewRecursiveMkdirInvalidatesAncestors(t *testing.T) {
	handlers := protocol.InMemHandler()
	_, client := sshserver.NewMemory(t, sshserver.Config{User: "u", Password: "p", SFTPHandlers: &handlers})
	files, err := openSFTPSubsystem(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	cache := newRemoteDirCache(files, time.Hour)
	if before, err := cache.ReadDir("/"); err != nil || len(before) != 0 {
		t.Fatalf("initial root: %v %v", before, err)
	}
	svc := NewService(t.TempDir())
	defer svc.lifeCancel()
	svc.sessions["s"] = &resource{Session: Session{ID: "s", State: "open"}, client: files, cache: cache}
	if _, err := svc.Mkdir("s", "/a/b/c", true); err != nil {
		t.Fatal(err)
	}
	after, err := cache.ReadDir("/")
	if err != nil || len(after) != 1 || after[0].Name() != "a" {
		t.Fatalf("stale root: %v %v", after, err)
	}
}

func TestReviewEmptyRememberPreservesStoredAuth(t *testing.T) {
	layout := paths.NewLayout(filepath.Join(t.TempDir(), "cfg"), filepath.Join(t.TempDir(), "state"))
	cfg := config.NewService(layout, crypto.NewStaticProvider([]byte("test-key")))
	if _, err := cfg.CreateServer(config.ServerProfile{ID: "server", Alias: "server", Host: "host", Port: 22, User: "u", AuthMethod: config.AuthMethodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetServerPassword("server", "stored-password"); err != nil {
		t.Fatal(err)
	}
	svc := NewService(t.TempDir())
	defer svc.lifeCancel()
	svc.UseConfig(cfg)
	svc.sessions["attempt"] = &resource{Session: Session{ID: "attempt", State: "connecting"}}
	for _, resp := range []ChallengeResponse{{Remember: true}, {Remember: true, Passphrase: "attempt-only"}} {
		before, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		svc.saveCredentials("attempt", "server", resp)
		after, err := os.ReadFile(filepath.Join(layout.ConfigDir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("empty/passphrase-only remember changed saved config")
		}
	}
	runtime, err := cfg.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, stop, _, err := svc.Subscribe("attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := svc.waitAuthResponse(ctx, "attempt", runtime.Servers["server"], runtime, sshpool.ErrAuthFailed, 1)
		done <- err
	}()
	for e := range events {
		if e.Type == "sftp.auth.challenge" {
			break
		}
	}
	if _, err := svc.RespondAuthChallenge("attempt", ChallengeResponse{Remember: true}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	svc.mu.RLock()
	candidate := svc.sessions["attempt"].pendingCredentials
	svc.mu.RUnlock()
	if candidate != nil {
		t.Fatal("empty response queued a persistent choice")
	}
}

type selectiveClosePut struct{ under protocol.FileWriter }

func (p selectiveClosePut) Filewrite(r *protocol.Request) (io.WriterAt, error) {
	w, err := p.under.Filewrite(r)
	if err != nil {
		return nil, err
	}
	return selectiveCloseWriter{WriterAt: w, fail: strings.HasSuffix(r.Filepath, "/bad")}, nil
}

type selectiveCloseWriter struct {
	io.WriterAt
	fail bool
}

func (w selectiveCloseWriter) Close() error {
	var err error
	if closer, ok := w.WriterAt.(io.Closer); ok {
		err = closer.Close()
	}
	if w.fail {
		return errors.Join(err, errors.New("review remote CLOSE failed"))
	}
	return err
}
func TestReviewBatchUploadClosePartialFailure(t *testing.T) {
	handlers := protocol.InMemHandler()
	handlers.FilePut = selectiveClosePut{handlers.FilePut}
	_, client := sshserver.NewMemory(t, sshserver.Config{User: "u", Password: "p", SFTPHandlers: &handlers})
	files, err := openSFTPSubsystem(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if err := files.Mkdir("/dest"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	good, bad := filepath.Join(root, "good"), filepath.Join(root, "bad")
	for _, name := range []string{good, bad} {
		if err := os.WriteFile(name, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewService(t.TempDir())
	defer svc.lifeCancel()
	work := &transferWork{Transfer: Transfer{Direction: "upload", Items: []TransferItem{{Source: good}, {Source: bad}}}}
	state, err := svc.runBatchUpload(context.Background(), &backendView{client: files}, work, BatchTransferRequest{Sources: []string{good, bad}, Target: "/dest", Overwrite: true})
	if state != "partial_failed" || err == nil || work.FilesDone != 1 || work.Items[0].State != "completed" || work.Items[1].State != "failed" || !strings.Contains(work.Items[1].Error, "review remote CLOSE failed") {
		t.Fatalf("result: %s %v %+v", state, err, work.Transfer)
	}
}
