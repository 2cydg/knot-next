package integration

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocolsftp "github.com/pkg/sftp"
)

func assertCompletedFile(t *testing.T, final transferJSON, size int) {
	t.Helper()
	if final.State != "completed" || final.Error != "" || final.BytesTotal != int64(size) || final.BytesCopied != int64(size) || final.FilesTotal != 1 || final.FilesDone != 1 {
		t.Fatalf("incorrect single-file result: %+v", final)
	}
}

func assertSnapshotContains(t *testing.T, message transferMessageJSON, final transferJSON) {
	t.Helper()
	for _, transfer := range message.Transfers {
		if transfer.ID == final.ID {
			if !reflect.DeepEqual(transfer, final) {
				t.Fatalf("snapshot differs from GET: snapshot=%+v GET=%+v", transfer, final)
			}
			return
		}
	}
	t.Fatalf("snapshot missing retained transfer %s: %+v", final.ID, message)
}

// F02/F03: a real SSH subsystem carries each file in both directions; the WS
// subscribed before POST must agree with GET, and late subscribers need no event.
func TestSFTPProtocolFileRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"small", []byte("中文\x00\r\n\xff")},
		{"large", bytes.Repeat([]byte("\x00\xfflarge-file\r\n"), 32769)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
			session := client.createSession(t)
			conn, empty := client.subscribe(t, session.ID)
			if len(empty.Transfers) != 0 {
				t.Fatalf("new session has transfers: %+v", empty)
			}
			source := filepath.Join(t.TempDir(), "source.bin")
			if err := os.WriteFile(source, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			upload := client.startTransfer(t, session.ID, "upload", map[string]any{"source": source, "target": "/roundtrip.bin"})
			finalUpload := client.waitTransfer(t, session.ID, upload.ID)
			assertCompletedFile(t, finalUpload, len(tc.data))
			if finalUpload.Direction != "upload" {
				t.Fatalf("wrong direction: %+v", finalUpload)
			}
			waitTransferEvent(t, conn, finalUpload)
			downloadPath := filepath.Join(t.TempDir(), "download.bin")
			download := client.startTransfer(t, session.ID, "download", map[string]any{"source": "/roundtrip.bin", "target": downloadPath})
			finalDownload := client.waitTransfer(t, session.ID, download.ID)
			assertCompletedFile(t, finalDownload, len(tc.data))
			if finalDownload.Direction != "download" {
				t.Fatalf("wrong direction: %+v", finalDownload)
			}
			waitTransferEvent(t, conn, finalDownload)
			if got := mustRead(t, downloadPath); !bytes.Equal(got, tc.data) {
				t.Fatalf("roundtrip data differs: size=%d want=%d", len(got), len(tc.data))
			}
			_ = conn.Close()
			_, retained := client.subscribe(t, session.ID)
			if len(retained.Transfers) != 2 {
				t.Fatalf("retained %d transfers, want 2", len(retained.Transfers))
			}
			assertSnapshotContains(t, retained, finalUpload)
			assertSnapshotContains(t, retained, finalDownload)
			if retained.Transfers[0].StartedAt.After(retained.Transfers[1].StartedAt) {
				t.Fatal("snapshot not ordered by start time")
			}
		})
	}
}

// writeGate pauses one selected file's first protocol write. Its closed path
// observes fixture shutdown, so an assertion failure cannot deadlock cleanup.
type writeGate struct {
	protocolsftp.FileWriter
	path    string
	ready   chan struct{}
	release chan struct{}
	stop    <-chan struct{}
	once    sync.Once
	opens   atomic.Int32
}

func (g *writeGate) Filewrite(request *protocolsftp.Request) (io.WriterAt, error) {
	writer, err := g.FileWriter.Filewrite(request)
	if err != nil || request.Filepath != g.path {
		return writer, err
	}
	g.opens.Add(1)
	return &gatedProtocolWriter{WriterAt: writer, gate: g}, nil
}

type gatedProtocolWriter struct {
	io.WriterAt
	gate *writeGate
}

func (w *gatedProtocolWriter) WriteAt(data []byte, offset int64) (int, error) {
	w.gate.once.Do(func() { close(w.gate.ready) })
	select {
	case <-w.gate.release:
		return w.WriterAt.WriteAt(data, offset)
	case <-w.gate.stop:
		return 0, net.ErrClosed
	}
}

func (w *gatedProtocolWriter) Close() error {
	if closer, ok := w.WriterAt.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// F03/F07: disconnect the WS while remote I/O is held, recover the same ID on
// another connection, and prove neither subscription nor a wrong-session DELETE
// starts/cancels the transfer. The other SFTP session stays usable.
func TestSFTPProtocolTransferRecoveryAndIsolation(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	gate := &writeGate{FileWriter: handlers.FilePut, path: "/held.bin", ready: make(chan struct{}), release: make(chan struct{})}
	handlers.FilePut = gate
	client, remote := newSFTPAPI(t, handlers)
	gate.stop = remote.Done()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(unblock)
	first, second := client.createSession(t), client.createSession(t)
	var status struct {
		SSHPool struct {
			Count   int `json:"count"`
			Entries []struct {
				RefCount int `json:"ref_count"`
			} `json:"entries"`
		} `json:"ssh_pool"`
	}
	client.call(t, http.MethodGet, "/v1/status", nil, http.StatusOK, &status)
	if status.SSHPool.Count != 1 || len(status.SSHPool.Entries) != 1 || status.SSHPool.Entries[0].RefCount != 2 {
		t.Fatalf("sessions did not share a pooled SSH connection: %+v", status)
	}
	conn, _ := client.subscribe(t, first.ID)
	data := bytes.Repeat([]byte("held-transfer"), 9000)
	source := filepath.Join(t.TempDir(), "held.bin")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	accepted := client.startTransfer(t, first.ID, "upload", map[string]any{"source": source, "target": gate.path})
	select {
	case <-gate.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never reached remote write")
	}
	_ = conn.Close()
	var running transferJSON
	client.call(t, http.MethodGet, transferRoute(first.ID, accepted.ID), nil, http.StatusOK, &running)
	if running.State != "running" || !running.CompletedAt.IsZero() {
		t.Fatalf("transfer did not remain active after WS disconnect: %+v", running)
	}
	reconnected, pending := client.subscribe(t, first.ID)
	assertSnapshotContains(t, pending, running)
	_, isolated := client.subscribe(t, second.ID)
	if len(isolated.Transfers) != 0 {
		t.Fatalf("second session sees first session's transfers: %+v", isolated)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		client.call(t, method, transferRoute(second.ID, accepted.ID), nil, http.StatusNotFound, nil)
	}
	// A second session's file operations still work while the first is blocked.
	other := client.startTransfer(t, second.ID, "upload", map[string]any{"source": source, "target": "/other.bin"})
	assertCompletedFile(t, client.waitTransfer(t, second.ID, other.ID), len(data))
	unblock()
	final := client.waitTransfer(t, first.ID, accepted.ID)
	assertCompletedFile(t, final, len(data))
	waitTransferEvent(t, reconnected, final)
	if gate.opens.Load() != 1 {
		t.Fatalf("recovery repeated remote file creation %d times", gate.opens.Load())
	}
	var transfers []transferJSON
	client.call(t, http.MethodGet, "/v1/sftp/"+first.ID+"/transfers", nil, http.StatusOK, &transfers)
	if len(transfers) != 1 || !reflect.DeepEqual(transfers[0], final) {
		t.Fatalf("recovery produced duplicate/different transfers: %+v", transfers)
	}
	_, late := client.subscribe(t, first.ID)
	assertSnapshotContains(t, late, final)
	for i := 0; i < 2; i++ {
		var repeatedCancel, afterCancel transferJSON
		client.call(t, http.MethodDelete, transferRoute(first.ID, accepted.ID), nil, http.StatusOK, &repeatedCancel)
		if !reflect.DeepEqual(repeatedCancel, final) {
			t.Fatalf("late cancel returned a different terminal result: %+v", repeatedCancel)
		}
		// DELETE returns a snapshot taken before cancellation. Read the retained
		// state again to detect a cancel that corrupts state or CompletedAt.
		client.call(t, http.MethodGet, transferRoute(first.ID, accepted.ID), nil, http.StatusOK, &afterCancel)
		if !reflect.DeepEqual(afterCancel, final) {
			t.Fatalf("late cancel rewrote retained terminal result: %+v", afterCancel)
		}
	}
	_, afterCancelSnapshot := client.subscribe(t, first.ID)
	assertSnapshotContains(t, afterCancelSnapshot, final)
	downloadPath := filepath.Join(t.TempDir(), "recovered.bin")
	download := client.startTransfer(t, first.ID, "download", map[string]any{"source": gate.path, "target": downloadPath})
	assertCompletedFile(t, client.waitTransfer(t, first.ID, download.ID), len(data))
	if !bytes.Equal(mustRead(t, downloadPath), data) {
		t.Fatal("recovered transfer content differs")
	}
}

func TestSFTPProtocolFixtureCloseReleasesWriteGate(t *testing.T) {
	handlers := protocolsftp.InMemHandler()
	gate := &writeGate{FileWriter: handlers.FilePut, path: "/held.bin", ready: make(chan struct{}), release: make(chan struct{})}
	handlers.FilePut = gate
	client, remote := newSFTPAPI(t, handlers)
	gate.stop = remote.Done()
	session := client.createSession(t)
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	accepted := client.startTransfer(t, session.ID, "upload", map[string]any{"source": source, "target": gate.path})
	select {
	case <-gate.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never reached remote write")
	}
	remote.Close()
	stopped := make(chan struct{})
	go func() { remote.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture close did not release gated protocol worker")
	}
	// A lost transport may be observed by pool cancellation or by transfer I/O
	// first. Both outcomes must settle and never report a completed copy.
	final := client.waitTransfer(t, session.ID, accepted.ID)
	if (final.State != "failed" && final.State != "canceled") || final.FilesDone != 0 || final.BytesCopied != 0 {
		t.Fatalf("transport loss was reported as successful copying: %+v", final)
	}
}

// F04: both directions keep per-source errors/results and count only successful
// files. All failures are asynchronous task failures, despite the HTTP 202.
func TestSFTPProtocolBatchResults(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		for _, outcome := range []string{"completed", "partial_failed", "failed"} {
			t.Run(direction+"/"+outcome, func(t *testing.T) {
				client, _ := newSFTPAPI(t, protocolsftp.InMemHandler())
				session := client.createSession(t)
				root := t.TempDir()
				data := []byte("batch contents")
				good := filepath.Join(root, "good.bin")
				goodTwo := filepath.Join(root, "second.bin")
				if err := os.WriteFile(good, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goodTwo, data, 0600); err != nil {
					t.Fatal(err)
				}
				missing := filepath.Join(root, "missing.bin")
				target := "/batch/"
				if direction == "upload" {
					client.call(t, http.MethodPost, "/v1/sftp/"+session.ID+"/dirs", map[string]any{"path": "/batch"}, http.StatusCreated, nil)
				} else {
					seed := client.startTransfer(t, session.ID, "upload", map[string]any{"source": good, "target": "/good.bin"})
					assertCompletedFile(t, client.waitTransfer(t, session.ID, seed.ID), len(data))
					seedTwo := client.startTransfer(t, session.ID, "upload", map[string]any{"source": goodTwo, "target": "/second.bin"})
					assertCompletedFile(t, client.waitTransfer(t, session.ID, seedTwo.ID), len(data))
					good, missing, target = "/good.bin", "/missing.bin", t.TempDir()
					goodTwo = "/second.bin"
				}
				sources := []string{good, goodTwo}
				states := []string{"completed", "completed"}
				wantDone := 2
				if outcome == "partial_failed" {
					// Failure first proves the batch continues to a later success.
					sources, states = []string{missing, good}, []string{"failed", "completed"}
					wantDone = 1
				} else if outcome == "failed" {
					sources, states, wantDone = []string{missing, missing + "-two"}, []string{"failed", "failed"}, 0
				}
				conn, _ := client.subscribe(t, session.ID)
				batch := client.startTransfer(t, session.ID, "batch-"+direction, map[string]any{"sources": sources, "target": target})
				final := client.waitTransfer(t, session.ID, batch.ID)
				if final.State != outcome || final.Direction != direction || len(final.Items) != len(sources) || final.FilesDone != wantDone {
					t.Fatalf("incorrect batch result: %+v", final)
				}
				if (final.Error == "") != (outcome == "completed") {
					t.Fatalf("incorrect aggregate error: %+v", final)
				}
				for i, item := range final.Items {
					if item.Source != sources[i] || item.State != states[i] || (item.Error == "") != (states[i] == "completed") {
						t.Fatalf("incorrect item %d: %+v", i, item)
					}
				}
				if final.BytesCopied != int64(wantDone*len(data)) || final.BytesTotal != final.BytesCopied || final.FilesTotal != wantDone {
					t.Fatalf("incorrect aggregate progress: %+v", final)
				}
				waitTransferEvent(t, conn, final)
				_, retained := client.subscribe(t, session.ID)
				assertSnapshotContains(t, retained, final)
				for i, state := range states {
					if state != "completed" {
						continue
					}
					name := filepath.Base(sources[i])
					downloadPath := filepath.Join(target, name)
					if direction == "upload" {
						downloadPath = filepath.Join(t.TempDir(), "verify.bin")
						verify := client.startTransfer(t, session.ID, "download", map[string]any{"source": "/batch/" + name, "target": downloadPath})
						assertCompletedFile(t, client.waitTransfer(t, session.ID, verify.ID), len(data))
					}
					if !bytes.Equal(mustRead(t, downloadPath), data) {
						t.Fatal("successful batch item contents differ")
					}
				}
			})
		}
	}
}
