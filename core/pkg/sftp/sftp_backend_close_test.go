package sftp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pkgsftp "github.com/pkg/sftp"
)

// Exercise the actual SFTP protocol without external SSH servers or TCP ports.
// The in-memory server never interprets remote paths as host filesystem paths.
func newRemoteTransferFixture(t *testing.T, handlers pkgsftp.Handlers) (*Service, *pkgsftp.Client) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	if err := clientConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	server := pkgsftp.NewRequestServer(serverConn, handlers)
	done := make(chan struct{})
	go func() { _ = server.Serve(); close(done) }()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = server.Close()
		_ = serverConn.Close()
		awaitTransferSignal(t, done)
	})
	client, err := pkgsftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	svc := newTransferTestService(t)
	svc.sessions["test"].Backend = "ssh"
	svc.sessions["test"].client = client
	svc.sessions["test"].cache = newRemoteDirCache(client, remoteDirCacheTTL)
	t.Cleanup(func() { _, _ = svc.Close("test") })
	return svc, client
}

func TestTransferRemoteCloseBeforeDispatch(t *testing.T) {
	for _, direction := range []string{"upload", "download", "batch-download"} {
		t.Run(direction, func(t *testing.T) {
			svc, client := newRemoteTransferFixture(t, pkgsftp.InMemHandler())
			remoteFile, err := client.Create("/source")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := remoteFile.Write([]byte("remote content")); err != nil {
				t.Fatal(err)
			}
			if err := remoteFile.Close(); err != nil {
				t.Fatal(err)
			}
			// A local file with the same path would expose accidental local dispatch.
			if err := os.WriteFile(filepath.Join(svc.root, "source"), []byte("wrong local backend"), 0600); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "empty-source")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			req := TransferRequest{Source: source, Target: "/must-not-exist", Recursive: true}
			if direction != "upload" {
				req.Source, req.Target = "/source", filepath.Join(t.TempDir(), "download")
			}
			ready, done := make(chan struct{}), make(chan struct{})
			release, unblock := transferBarrier(t)
			var items []TransferItem
			if direction == "batch-download" {
				items = []TransferItem{{Source: "/source", State: "queued"}}
			}
			transfer, err := svc.startTransfer("test", direction, req.Source, req.Target, items, func(ctx context.Context, backend *backendView, work *transferWork) (string, error) {
				defer close(done)
				close(ready)
				<-release
				// The view remains SSH even though Close clears the shared client.
				if backend.client == nil {
					return "failed", fmt.Errorf("remote backend lost")
				}
				switch direction {
				case "upload":
					return svc.runUpload(ctx, backend, work, req)
				case "download":
					return svc.runDownload(ctx, backend, work, req)
				default:
					return svc.runBatchDownload(ctx, backend, work, BatchTransferRequest{Sources: []string{"/source"}, Target: req.Target})
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestTransfer(t, svc, transfer.ID, unblock)
			awaitTransferSignal(t, ready)
			if _, err := svc.Close("test"); err != nil {
				t.Fatal(err)
			}
			unblock()
			awaitTransferSignal(t, done)
			final := waitTransferState(t, svc, "test", transfer.ID)
			if final.State != "canceled" {
				t.Fatalf("final=%+v", final)
			}
			localTarget := req.Target
			if direction == "upload" {
				localTarget = filepath.Join(svc.root, "must-not-exist")
			}
			if _, err := os.Stat(localTarget); !os.IsNotExist(err) {
				t.Fatalf("unexpected local I/O: %s err=%v", localTarget, err)
			}
		})
	}
}

type remoteWriteGate struct {
	pkgsftp.FileWriter
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (g *remoteWriteGate) Filewrite(req *pkgsftp.Request) (io.WriterAt, error) {
	writer, err := g.FileWriter.Filewrite(req)
	if err != nil {
		return nil, err
	}
	return &gatedRemoteWriter{WriterAt: writer, gate: g}, nil
}

type gatedRemoteWriter struct {
	io.WriterAt
	gate *remoteWriteGate
}

func (w *gatedRemoteWriter) WriteAt(p []byte, off int64) (int, error) {
	if off > 0 {
		w.gate.once.Do(func() { close(w.gate.ready) })
		<-w.gate.release
	}
	return w.WriterAt.WriteAt(p, off)
}
func (w *gatedRemoteWriter) Close() error {
	if closer, ok := w.WriterAt.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type remoteReadGate struct {
	pkgsftp.FileReader
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (g *remoteReadGate) Fileread(req *pkgsftp.Request) (io.ReaderAt, error) {
	reader, err := g.FileReader.Fileread(req)
	if err != nil {
		return nil, err
	}
	return &gatedRemoteReader{ReaderAt: reader, gate: g}, nil
}

type gatedRemoteReader struct {
	io.ReaderAt
	gate *remoteReadGate
}

func (r *gatedRemoteReader) ReadAt(p []byte, off int64) (int, error) {
	if off > 0 {
		r.gate.once.Do(func() { close(r.gate.ready) })
		<-r.gate.release
	}
	return r.ReaderAt.ReadAt(p, off)
}
func (r *gatedRemoteReader) Close() error {
	if closer, ok := r.ReaderAt.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func TestTransferRemoteCloseDuringIO(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			ready := make(chan struct{})
			// Register release before fixture cleanup, then also release explicitly
			// before server cleanup if the test exits at an assertion.
			release, unblock := transferBarrier(t)
			handlers := pkgsftp.InMemHandler()
			if direction == "upload" {
				handlers.FilePut = &remoteWriteGate{FileWriter: handlers.FilePut, ready: ready, release: release}
			}
			if direction == "download" {
				handlers.FileGet = &remoteReadGate{FileReader: handlers.FileGet, ready: ready, release: release}
			}
			svc, client := newRemoteTransferFixture(t, handlers)
			t.Cleanup(unblock)
			data := bytes.Repeat([]byte("x"), 96*1024)
			localPath := filepath.Join(t.TempDir(), "data")
			var transfer Transfer
			var err error
			if direction == "upload" {
				if err := os.WriteFile(localPath, data, 0600); err != nil {
					t.Fatal(err)
				}
				transfer, err = svc.Upload("test", TransferRequest{Source: localPath, Target: "/data"})
			} else {
				remoteFile, err := client.Create("/data")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := remoteFile.Write(data); err != nil {
					t.Fatal(err)
				}
				if err := remoteFile.Close(); err != nil {
					t.Fatal(err)
				}
				transfer, err = svc.Download("test", TransferRequest{Source: "/data", Target: localPath})
			}
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestTransfer(t, svc, transfer.ID, unblock)
			awaitTransferSignal(t, ready)
			before, err := svc.Transfer("test", transfer.ID)
			if err != nil || before.State != "running" || before.BytesCopied == 0 {
				t.Fatalf("before=%+v err=%v", before, err)
			}
			if _, err := svc.Close("test"); err != nil {
				t.Fatal(err)
			}
			// Closing the client must unblock real in-flight network I/O, even while
			// the remote handler remains blocked. GET confirms the worker has finished.
			final := waitTransferState(t, svc, "test", transfer.ID)
			if final.State != "canceled" || final.BytesCopied == 0 || final.FilesDone != 0 {
				t.Fatalf("final=%+v", final)
			}
			unblock()
		})
	}
}

func TestTransferRemoteGlobViewSurvivesClose(t *testing.T) {
	svc, _ := newRemoteTransferFixture(t, pkgsftp.InMemHandler())
	view, _, err := svc.requireOpenSession("test", "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Close("test"); err != nil {
		t.Fatal(err)
	}
	if view.client == nil || view.cache == nil {
		t.Fatal("captured backend was changed by Close")
	}
	for _, cache := range []bool{false, true} {
		if _, err := svc.globMatches(view, "/*", false, cache); err == nil {
			t.Fatal("closed remote glob unexpectedly succeeded")
		}
	}
}
