package sftp

import (
	"context"
	"errors"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/session"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type reviewFollowProvider struct {
	source     session.Resource
	entered    chan struct{}
	release    chan struct{}
	subscribed atomic.Int32
}

func (p *reviewFollowProvider) Get(string) (session.Resource, error) { return p.source, nil }
func (p *reviewFollowProvider) SubscribeCWD(string) (<-chan session.CWDNotify, func(), session.Resource, error) {
	p.subscribed.Add(1)
	if p.entered != nil {
		p.entered <- struct{}{}
		<-p.release
	}
	ch := make(chan session.CWDNotify)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }, p.source, nil
}
func TestConcurrentSameSourceFollowIsIdempotent(t *testing.T) {
	s := NewService(t.TempDir())
	s.UseLocalTestBackend()
	defer s.Shutdown(context.Background())
	res, err := s.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &reviewFollowProvider{source: session.Resource{ID: "source", ConnectionID: "conn", ServerRef: "srv", State: "connected"}, entered: make(chan struct{}, 2), release: make(chan struct{})}
	s.UseSession(provider)
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.Control(res.ID, ControlRequest{Op: "set-follow", FollowSessionID: "source", FollowConnectionID: "conn"})
			results <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-provider.entered:
		case <-time.After(time.Second):
			t.Fatal("requests did not concurrently subscribe")
		}
	}
	close(provider.release)
	for i := 0; i < 2; i++ {
		if err = <-results; err != nil {
			t.Fatal("same-source conflict", err)
		}
	}
	s.mu.RLock()
	generation := s.sessions[res.ID].followGeneration
	s.mu.RUnlock()
	if generation != 1 {
		t.Fatal("duplicate request replaced binding", generation)
	}
}
func TestFollowWorkerCapacityRollbackAndResumeDirectoryResult(t *testing.T) {
	s := NewService(t.TempDir())
	s.UseLocalTestBackend()
	defer s.Shutdown(context.Background())
	res, err := s.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &reviewFollowProvider{source: session.Resource{ID: "source", ConnectionID: "conn", ServerRef: "srv", State: "connected", CurrentDir: "/missing"}}
	s.UseSession(provider)
	s.mu.Lock()
	r := s.sessions[res.ID]
	r.followWorkers = maxFollowWorkers
	s.mu.Unlock()
	if _, err = s.Control(res.ID, ControlRequest{Op: "set-follow", FollowSessionID: "source"}); !errors.Is(err, ErrConflict) {
		t.Fatal("worker capacity not enforced", err)
	}
	if provider.subscribed.Load() != 0 {
		t.Fatal("capacity rejection leaked subscription")
	}
	s.mu.Lock()
	r.followWorkers = 0
	s.mu.Unlock()
	if _, err = s.Control(res.ID, ControlRequest{Op: "set-follow", FollowSessionID: "source"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, _ := s.Get(res.ID)
		if got.FollowError == "directory_unavailable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial directory validation did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = s.Control(res.ID, ControlRequest{Op: "pause-follow"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Control(res.ID, ControlRequest{Op: "resume-follow"})
	if err != nil || got.FollowState != "active" || got.FollowError != "directory_unavailable" || got.ID != res.ID {
		t.Fatal("committed resume returned failure", got, err)
	}
}
func TestLiteralDirectoryAndUploadSourceModeValidation(t *testing.T) {
	s := NewService(t.TempDir())
	s.UseLocalTestBackend()
	defer s.Shutdown(context.Background())
	res, err := s.Create(CreateRequest{ServerRef: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BatchUpload(res.ID, BatchTransferRequest{Sources: []string{"file"}, Target: "/", SourceMode: "garbage"}); !errors.Is(err, ErrValidation) {
		t.Fatal("invalid upload source mode ignored", err)
	}
	transfer, err := s.BatchDownload(res.ID, BatchTransferRequest{Sources: []string{"/"}, Target: t.TempDir(), SourceMode: "literal"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		transfer, err = s.Transfer(res.ID, transfer.ID)
		if err != nil {
			t.Fatal(err)
		}
		if isTerminalState(transfer.State) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transfer did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	if transfer.State != "failed" || transfer.Error == "" {
		t.Fatal("directory reported missing", transfer)
	}
}

func TestFollowSwitchClearPreserveExactTransportLease(t *testing.T) {
	remote := sshserver.New(t, sshserver.Config{User: testAuthUser, Password: testAuthPassword, SFTPRoot: t.TempDir()})
	s, serverID, cfg := newSFTPAuthService(t, remote, testAuthPassword)
	shells := session.NewService()
	shells.UseConfig(cfg)
	shells.UsePool(s.pool)
	s.UseSession(shells)
	defer shells.Shutdown(context.Background())
	defer s.Shutdown(context.Background())
	sources := make([]session.Resource, 0, 2)
	for i := 0; i < 2; i++ {
		source, err := shells.Create(session.CreateRequest{ServerRef: serverID, HostKeyPolicy: "insecure-skip"})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			source, err = shells.Get(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if source.State == "connected" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("source not connected", source)
			}
			time.Sleep(time.Millisecond)
		}
		sources = append(sources, source)
	}
	files, err := s.Create(CreateRequest{ServerRef: serverID, ConnectionID: sources[0].ConnectionID, FollowSessionID: sources[0].ID, FollowConnectionID: sources[0].ConnectionID})
	if err != nil {
		t.Fatal(err)
	}
	files = waitForSFTPState(t, s, files.ID, time.Second, "open")
	before := s.pool.Stats()
	if len(before) != 1 || before[0].RefCount != 3 {
		t.Fatal("fixture lacks shared owners", before)
	}
	for _, req := range []ControlRequest{{Op: "set-follow", FollowSessionID: sources[1].ID, FollowConnectionID: sources[1].ConnectionID}, {Op: "clear-follow"}} {
		got, err := s.Control(files.ID, req)
		if err != nil || got.ID != files.ID || got.ConnectionID != files.ConnectionID {
			t.Fatal("binding rebuilt transport", got, err)
		}
		after := s.pool.Stats()
		if len(after) != 1 || after[0].RefCount != before[0].RefCount {
			t.Fatal("binding changed exact lease references", after)
		}
	}
	if _, err = s.Close(files.ID); err != nil {
		t.Fatal(err)
	}
	settleSFTP(t, s)
	if _, err = s.Close(files.ID); err != nil {
		t.Fatal(err)
	}
	after := s.pool.Stats()
	if len(after) != 1 || after[0].RefCount != 2 {
		t.Fatal("close released someone else's lease", after)
	}
}
