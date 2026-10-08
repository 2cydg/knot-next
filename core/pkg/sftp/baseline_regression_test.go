package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocol "github.com/pkg/sftp"

	"knot-core/internal/testutil/sshserver"
)

type auditDirReader struct {
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (r *auditDirReader) ReadDir(name string) ([]os.FileInfo, error) {
	r.calls++
	if r.entered != nil {
		close(r.entered)
		<-r.release
	}
	return nil, nil
}
func TestRegressionCacheRetention(t *testing.T) {
	r := &auditDirReader{}
	c := newRemoteDirCache(r, time.Second)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	for i := 0; i < 1000; i++ {
		if _, err := c.ReadDir(fmt.Sprintf("/dir/%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	if _, err := c.ReadDir("/new"); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) > 1 {
		t.Errorf("expired cache entries retained: %d", len(c.entries))
	}
}
func TestRegressionCacheInvalidateRace(t *testing.T) {
	r := &auditDirReader{entered: make(chan struct{}), release: make(chan struct{})}
	c := newRemoteDirCache(r, time.Minute)
	done := make(chan struct{})
	go func() { _, _ = c.ReadDir("/dir"); close(done) }()
	<-r.entered
	c.Invalidate("/dir")
	close(r.release)
	<-done
	r.entered = nil
	if _, err := c.ReadDir("/dir"); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Errorf("in-flight stale result repopulated invalidated directory: remote reads=%d", r.calls)
	}
}

type auditPut struct{ under protocol.FileWriter }

func (p auditPut) Filewrite(r *protocol.Request) (io.WriterAt, error) {
	w, e := p.under.Filewrite(r)
	if e != nil {
		return nil, e
	}
	return auditCloseWriter{w}, nil
}

type auditCloseWriter struct{ io.WriterAt }

func (w auditCloseWriter) Close() error { return errors.New("audit remote CLOSE failed") }
func TestRegressionUploadCloseError(t *testing.T) {
	h := protocol.InMemHandler()
	h.FilePut = auditPut{h.FilePut}
	_, client := sshserver.NewMemory(t, sshserver.Config{User: "u", Password: "p", SFTPHandlers: &h})
	files, err := openSFTPSubsystem(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	src := filepath.Join(t.TempDir(), "source")
	if err = os.WriteFile(src, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(t.TempDir())
	defer svc.lifeCancel()
	res := &backendView{ID: "audit", client: files}
	work := &transferWork{}
	state, err := svc.executeRemoteUploadPlan(context.Background(), res, work, transferPlan{files: []fileJob{{source: src, target: "/dest", size: 7}}}, true)
	if state == "completed" || err == nil || !strings.Contains(err.Error(), "audit remote CLOSE failed") || work.FilesDone != 0 {
		t.Error("remote CLOSE failed but upload returned completed")
	}
}

func TestCacheCapacityAndClose(t *testing.T) {
	reader := &auditDirReader{}
	cache := newRemoteDirCache(reader, time.Hour)
	for i := 0; i < remoteDirCacheMaxEntries*2; i++ {
		if _, err := cache.ReadDir(fmt.Sprintf("/dir%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) > remoteDirCacheMaxEntries || cache.cost > remoteDirCacheMaxCost {
		t.Fatalf("unbounded cache: %d/%d", len(cache.entries), cache.cost)
	}
	cache.Close()
	if len(cache.entries) != 0 || cache.cost != 0 {
		t.Fatal("cache retained data after close")
	}
	if _, err := cache.ReadDir("/"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed cache accepted read: %v", err)
	}
}

type oversizedDirReader struct {
	info  os.FileInfo
	calls int
}

func (r *oversizedDirReader) ReadDir(string) ([]os.FileInfo, error) {
	r.calls++
	return []os.FileInfo{oversizedInfo{r.info}}, nil
}

type oversizedInfo struct{ os.FileInfo }

func (i oversizedInfo) Name() string { return strings.Repeat("x", remoteDirCacheMaxCost) }
func TestCacheSkipsOversizedDirectory(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(name, nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	reader := &oversizedDirReader{info: info}
	cache := newRemoteDirCache(reader, time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := cache.ReadDir("/huge"); err != nil {
			t.Fatal(err)
		}
	}
	if reader.calls != 2 || len(cache.entries) != 0 {
		t.Fatal("oversized directory retained")
	}
}

func TestFailedCDKeepsFollowActive(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.UseLocalTestBackend()
	defer svc.lifeCancel()
	current, err := svc.Create(CreateRequest{ServerRef: "server"})
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	res := svc.sessions[current.ID]
	res.FollowSessionID = "source"
	res.FollowState = "active"
	generation := res.cwdGeneration
	svc.mu.Unlock()
	if _, err := svc.Control(current.ID, ControlRequest{Op: "cd", Path: "/missing"}); err == nil {
		t.Fatal("missing directory accepted")
	}
	got, err := svc.Get(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FollowState != "active" || got.CurrentDir != current.CurrentDir {
		t.Fatalf("failed cd changed state: %+v", got)
	}
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if res.cwdGeneration != generation {
		t.Fatal("failed cd invalidated ongoing follow")
	}
}

func TestFailedUploadInvalidatesActualNestedDirectory(t *testing.T) {
	handlers := protocol.InMemHandler()
	handlers.FilePut = auditPut{handlers.FilePut}
	_, sshClient := sshserver.NewMemory(t, sshserver.Config{User: "u", Password: "p", SFTPHandlers: &handlers})
	files, err := openSFTPSubsystem(context.Background(), sshClient)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if err := files.MkdirAll("/nested/child"); err != nil {
		t.Fatal(err)
	}
	cache := newRemoteDirCache(files, time.Hour)
	before, err := cache.ReadDir("/nested/child")
	if err != nil || len(before) != 0 {
		t.Fatalf("initial listing: %v %v", before, err)
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(t.TempDir())
	defer svc.lifeCancel()
	work := &transferWork{Transfer: Transfer{Target: "/nested"}}
	res := &backendView{client: files, cache: cache}
	_, err = svc.executeRemoteUploadPlan(context.Background(), res, work, transferPlan{files: []fileJob{{source: source, target: "/nested/child/file", size: 4}}}, true)
	if err == nil {
		t.Fatal("CLOSE error not propagated")
	}
	after, err := cache.ReadDir("/nested/child")
	if err != nil || len(after) != 1 || after[0].Name() != "file" {
		t.Fatalf("partial upload left stale nested listing: %v %v", after, err)
	}
}
