package core

import (
	"context"
	"knot-core/internal/testutil/sshserver"
	"knot-core/pkg/session"
	"knot-core/pkg/sftp"
	"knot-core/pkg/sshpool"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Real SSH/SFTP channels share a pool. Success of the domain Shutdown methods
// proves their owned channel, pump, follower, transfer and cleanup workers ended.
func TestResourceShutdownAfterRepeatedWork(t *testing.T) {
	f := newShutdownFixture(t)
	f.sftpSvc.UseSession(f.sessions)
	life, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()
	f.sessions.StartMaintenance(life)
	f.sftpSvc.StartMaintenance(life)
	f.startResources(t)
	var attach session.AttachStream
	for i := 0; i < 5; i++ {
		var err error
		attach, _, err = f.sessions.AttachStream(f.sshID)
		if err != nil {
			t.Fatal(err)
		}
		attach.Cancel()
		attach.Release()
	}
	var err error
	attach, _, err = f.sessions.AttachStream(f.sshID)
	if err != nil {
		t.Fatal(err)
	}
	defer attach.Cancel()
	defer attach.Release()
	followed, err := f.sftpSvc.Create(sftp.CreateRequest{ServerRef: shutdownTestServerID, FollowSessionID: f.sshID, HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, func() (string, error) { res, err := f.sftpSvc.Get(followed.ID); return res.State, err }, "open")
	source := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(source, []byte("lifecycle transfer bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		transfer, err := f.sftpSvc.Upload(followed.ID, sftp.TransferRequest{Source: source, Target: "/lifecycle-payload", Overwrite: true})
		if err != nil {
			t.Fatal(err)
		}
		waitForState(t, func() (string, error) {
			res, err := f.sftpSvc.Transfer(followed.ID, transfer.ID)
			return res.State, err
		}, "completed")
		res, err := f.sessions.Exec(session.ExecRequest{ServerRef: shutdownTestServerID, Command: "completed", HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
		if err != nil || res.State != "completed" {
			t.Fatalf("exec: %+v %v", res, err)
		}
	}
	started, remoteDone := make(chan struct{}), make(chan struct{})
	f.server.SetExecBehavior(sshserver.ExecBehavior{Scripted: true, Started: started, Done: remoteDone, BeforeExit: make(chan struct{})})
	execDone := make(chan session.Exec, 1)
	go func() {
		res, _ := f.sessions.Exec(session.ExecRequest{ServerRef: shutdownTestServerID, Command: "active", HostKeyPolicy: sshpool.HostKeyPolicyInsecureSkip})
		execDone <- res
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("exec never started")
	}
	events, cancelEvents, err := f.core.SubscribeEvents()
	if err != nil {
		t.Fatal(err)
	}
	defer cancelEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.core.Shutdown(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	select {
	case res := <-execDone:
		if res.State != "failed" {
			t.Fatalf("active exec result: %+v", res)
		}
	case <-ctx.Done():
		t.Fatal("exec caller leaked")
	}
	select {
	case <-remoteDone:
	case <-ctx.Done():
		t.Fatal("remote exec leaked")
	}
	if err := f.sessions.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.sftpSvc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sessions.ActiveCount() != 0 || f.sftpSvc.SessionCount() != 0 || f.sftpSvc.RunningTransferCount() != 0 || f.pool.Count() != 0 {
		t.Fatal("live resources left after shutdown")
	}
	for range events {
	}
	cancelEvents()
	cancelEvents()
	if _, err := f.sessions.Create(session.CreateRequest{ServerRef: shutdownTestServerID}); err == nil {
		t.Fatal("closed SSH service accepted resource")
	}
	if _, err := f.sftpSvc.Create(sftp.CreateRequest{ServerRef: shutdownTestServerID}); err == nil {
		t.Fatal("closed SFTP service accepted resource")
	}
	f.server.Close()
	f.server.Wait()
}
