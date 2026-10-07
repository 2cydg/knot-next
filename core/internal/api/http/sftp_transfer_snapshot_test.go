package http

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knot-core/pkg/sftp"
)

// TestSFTPTransferSnapshotAfterCompletion tests A01: upload completes, then subscribe
// The snapshot should contain the completed transfer so the client can see the result
// without waiting for another event.
func TestSFTPTransferSnapshotAfterCompletion(t *testing.T) {
	server := newStatefulTestServer(t)

	// Create SFTP session
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Upload a small file
	localFile := filepath.Join(t.TempDir(), "completed.txt")
	if err := os.WriteFile(localFile, []byte("test"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	uploadBody := `{"source":` + strconvQuote(localFile) + `,"target":"/completed.txt"}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", uploadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var uploadResp struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("decode upload: %v", err)
	}

	// Wait for transfer to complete via GET
	final := waitForSFTPTransfer(t, server, created.Data.ID, uploadResp.Data.ID)
	if final.State != "completed" {
		t.Fatalf("transfer state = %q, want completed", final.State)
	}

	// Now subscribe AFTER completion
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()

	// Read the snapshot frame
	_, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	var snapshot struct {
		Type      string          `json:"type"`
		SessionID string          `json:"session_id"`
		Transfers []sftp.Transfer `json:"transfers"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	// Verify snapshot structure
	if snapshot.Type != "sftp.transfer.snapshot" {
		t.Fatalf("snapshot type = %q, want sftp.transfer.snapshot", snapshot.Type)
	}
	if snapshot.SessionID != created.Data.ID {
		t.Fatalf("snapshot session_id = %q, want %q", snapshot.SessionID, created.Data.ID)
	}

	// THIS IS THE KEY ASSERTION: snapshot should contain the completed transfer
	if len(snapshot.Transfers) != 1 {
		t.Fatalf("snapshot transfers count = %d, want 1, transfers = %+v", len(snapshot.Transfers), snapshot.Transfers)
	}
	if snapshot.Transfers[0].ID != uploadResp.Data.ID {
		t.Fatalf("snapshot transfer id = %q, want %q", snapshot.Transfers[0].ID, uploadResp.Data.ID)
	}
	if snapshot.Transfers[0].State != "completed" {
		t.Fatalf("snapshot transfer state = %q, want completed", snapshot.Transfers[0].State)
	}
	if snapshot.Transfers[0].CompletedAt.IsZero() {
		t.Fatal("snapshot transfer completed_at is zero")
	}

	// Verify file content
	targetPath := filepath.Join(created.Data.Root, "completed.txt")
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "test" {
		t.Fatalf("uploaded content = %q, want test", got)
	}
}

// TestSFTPTransferSnapshotEmpty tests A02: subscribe with no transfers
func TestSFTPTransferSnapshotEmpty(t *testing.T) {
	server := newStatefulTestServer(t)

	// Create SFTP session
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Subscribe immediately without any transfers
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()

	// Read snapshot
	_, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	var snapshot struct {
		Type      string          `json:"type"`
		SessionID string          `json:"session_id"`
		Transfers []sftp.Transfer `json:"transfers"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	// Should return empty array, not null
	if snapshot.Transfers == nil {
		t.Fatal("snapshot transfers is null, want empty array")
	}
	if len(snapshot.Transfers) != 0 {
		t.Fatalf("snapshot transfers count = %d, want 0", len(snapshot.Transfers))
	}

	// Now start an upload and verify we receive the event
	localFile := filepath.Join(t.TempDir(), "after.txt")
	if err := os.WriteFile(localFile, []byte("after"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	uploadBody := `{"source":` + strconvQuote(localFile) + `,"target":"/after.txt"}`
	upload := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", uploadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, upload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var started struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}

	// Should receive transfer events
	deadline := time.Now().Add(2 * time.Second)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	receivedEvent := false
	for time.Now().Before(deadline) {
		_, eventPayload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		var event sftp.TransferEvent
		if err := json.Unmarshal(eventPayload, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event.Type == "sftp.transfer.queued" || event.Type == "sftp.transfer.started" {
			receivedEvent = true
			break
		}
	}
	if !receivedEvent {
		t.Fatal("did not receive transfer event after snapshot")
	}
	if final := waitForSFTPTransfer(t, server, created.Data.ID, started.Data.ID); final.State != "completed" {
		t.Fatalf("transfer=%+v", final)
	}
}

// TestSFTPBatchTransferPartialFailed tests A05: batch download with one success and one failure
func TestSFTPBatchTransferPartialFailed(t *testing.T) {
	server := newStatefulTestServer(t)

	// Create SFTP session
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Prepare remote files: one exists, one doesn't
	existingFile := filepath.Join(created.Data.Root, "exists.txt")
	if err := os.WriteFile(existingFile, []byte("content"), 0o600); err != nil {
		t.Fatalf("write remote file: %v", err)
	}

	// Batch download: one file exists, one doesn't
	targetDir := t.TempDir()
	downloadBody := `{"sources":["/exists.txt","/missing.txt"],"target":` + strconvQuote(targetDir) + `}`
	download := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/batch-download", downloadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, download)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("batch download status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var downloadResp struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&downloadResp); err != nil {
		t.Fatalf("decode download: %v", err)
	}

	// Wait for transfer to finish
	final := waitForSFTPTransfer(t, server, created.Data.ID, downloadResp.Data.ID)

	// Should be partial_failed, not failed or completed
	if final.State != "partial_failed" {
		t.Fatalf("transfer state = %q, want partial_failed, items = %+v", final.State, final.Items)
	}

	// Verify items
	if len(final.Items) != 2 {
		t.Fatalf("items count = %d, want 2", len(final.Items))
	}

	// Check one succeeded, one failed
	successCount := 0
	failedCount := 0
	for _, item := range final.Items {
		switch item.State {
		case "completed":
			successCount++
			if item.Source != "/exists.txt" {
				t.Fatalf("completed item source = %q, want /exists.txt", item.Source)
			}
		case "failed":
			failedCount++
			if item.Source != "/missing.txt" {
				t.Fatalf("failed item source = %q, want /missing.txt", item.Source)
			}
			if item.Error == "" {
				t.Fatal("failed item has no error message")
			}
		default:
			t.Fatalf("unexpected item state = %q", item.State)
		}
	}
	if successCount != 1 || failedCount != 1 {
		t.Fatalf("success = %d, failed = %d, want 1 of each", successCount, failedCount)
	}

	// FilesDone should only count successful files
	if final.FilesDone != 1 {
		t.Fatalf("files_done = %d, want 1", final.FilesDone)
	}

	// Verify the successful file was downloaded
	downloadedFile := filepath.Join(targetDir, "exists.txt")
	got, err := os.ReadFile(downloadedFile)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != "content" {
		t.Fatalf("downloaded content = %q, want content", got)
	}

	// Subscribe after completion and verify snapshot shows partial_failed
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()

	_, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	var snapshot struct {
		Type      string          `json:"type"`
		SessionID string          `json:"session_id"`
		Transfers []sftp.Transfer `json:"transfers"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	if len(snapshot.Transfers) != 1 {
		t.Fatalf("snapshot transfers count = %d, want 1", len(snapshot.Transfers))
	}
	if snapshot.Transfers[0].State != "partial_failed" {
		t.Fatalf("snapshot transfer state = %q, want partial_failed", snapshot.Transfers[0].State)
	}
}

// TestSFTPBatchTransferAllFailed tests A06: batch with all items failing
func TestSFTPBatchTransferAllFailed(t *testing.T) {
	server := newStatefulTestServer(t)

	// Create SFTP session
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatal(err)
	}

	// Batch download of non-existent files
	targetDir := t.TempDir()
	downloadBody := `{"sources":["/missing1.txt","/missing2.txt"],"target":` + strconvQuote(targetDir) + `}`
	download := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/batch-download", downloadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, download)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("batch download status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var downloadResp struct {
		Data sftp.Transfer `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&downloadResp); err != nil {
		t.Fatalf("decode download: %v", err)
	}

	// Wait for transfer
	final := waitForSFTPTransfer(t, server, created.Data.ID, downloadResp.Data.ID)

	// Should be "failed", not "partial_failed" or "completed"
	if final.State != "failed" {
		t.Fatalf("transfer state = %q, want failed when all items fail", final.State)
	}

	event := readTransferEventOfType(t, conn, reader, "sftp.transfer.failed")
	if event.TransferID != final.ID || event.State != final.State || event.FilesDone != final.FilesDone {
		t.Fatalf("terminal event=%+v final=%+v", event, final)
	}

	// All items should be failed
	for _, item := range final.Items {
		if item.State != "failed" {
			t.Fatalf("item state = %q, want failed", item.State)
		}
	}

	// FilesDone should be 0
	if final.FilesDone != 0 {
		t.Fatalf("files_done = %d, want 0", final.FilesDone)
	}
}

// readTransferEventOfType reads transfer events until finding the specified type or timing out
func readTransferEventOfType(t *testing.T, conn net.Conn, reader *bufio.Reader, eventType string) sftp.TransferEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Now().Add(5 * time.Second)) // Restore a bounded default.

	for time.Now().Before(deadline) {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		var event sftp.TransferEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			// Skip non-transfer events
			continue
		}
		if event.Type == eventType {
			return event
		}
	}
	t.Fatalf("timed out waiting for event %q", eventType)
	return sftp.TransferEvent{}
}

// TestSFTPPartialFailedEventType tests that partial_failed generates the correct event type
func TestSFTPPartialFailedEventType(t *testing.T) {
	server := newStatefulTestServer(t)

	// Create SFTP session
	create := authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data sftp.Session `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Prepare one existing file
	existingFile := filepath.Join(created.Data.Root, "exists.txt")
	if err := os.WriteFile(existingFile, []byte("data"), 0o600); err != nil {
		t.Fatalf("write remote file: %v", err)
	}

	// Subscribe BEFORE starting the transfer
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+created.Data.ID+"/transfers/events")
	defer conn.Close()

	// Read and discard snapshot
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	// Start batch download with mixed results
	targetDir := t.TempDir()
	downloadBody := `{"sources":["/exists.txt","/missing.txt"],"target":` + strconvQuote(targetDir) + `}`
	download := authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/batch-download", downloadBody)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, download)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("batch download status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Wait for the terminal event - should be "sftp.transfer.partial_failed"
	event := readTransferEventOfType(t, conn, reader, "sftp.transfer.partial_failed")

	if event.State != "partial_failed" {
		t.Fatalf("event state = %q, want partial_failed", event.State)
	}
	if event.Type != "sftp.transfer.partial_failed" {
		t.Fatalf("event type = %q, want sftp.transfer.partial_failed", event.Type)
	}
}

func TestSFTPTransferSnapshotSessionIsolationAndRecovery(t *testing.T) {
	server := newStatefulTestServer(t)
	var sessions [2]sftp.Session
	var transfers [2]sftp.Transfer
	for i := range sessions {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, authenticatedJSONRequest(http.MethodPost, "/v1/sftp", `{"server_ref":"web"}`))
		var created struct {
			Data sftp.Session `json:"data"`
		}
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		sessions[i] = created.Data
		source := filepath.Join(t.TempDir(), "file")
		content := []byte("content")
		if i == 0 {
			content = nil
		} // Cover a zero-byte upload through the API as well.
		if err := os.WriteFile(source, content, 0600); err != nil {
			t.Fatal(err)
		}
		rec = httptest.NewRecorder()
		body := `{"source":` + strconvQuote(source) + `,"target":"/file"}`
		server.Handler().ServeHTTP(rec, authenticatedJSONRequest(http.MethodPost, "/v1/sftp/"+created.Data.ID+"/upload", body))
		var started struct {
			Data sftp.Transfer `json:"data"`
		}
		if rec.Code != http.StatusAccepted {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
			t.Fatal(err)
		}
		transfers[i] = waitForSFTPTransfer(t, server, created.Data.ID, started.Data.ID)
		if transfers[i].State != "completed" {
			t.Fatalf("transfer=%+v", transfers[i])
		}
		got, err := os.ReadFile(filepath.Join(created.Data.Root, "file"))
		if err != nil || string(got) != string(content) {
			t.Fatalf("content=%q err=%v", got, err)
		}
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	for i := range sessions {
		conn, reader := openTestWebSocket(t, ts, "/v1/sftp/"+sessions[i].ID+"/transfers/events")
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			Transfers []sftp.Transfer `json:"transfers"`
		}
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Transfers) != 1 || snapshot.Transfers[0].ID != transfers[i].ID {
			t.Fatalf("session snapshot=%+v", snapshot)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		// Recover the same task using GET after disconnect, then reconnect.
		final := waitForSFTPTransfer(t, server, sessions[i].ID, transfers[i].ID)
		if final.State != "completed" || !final.CompletedAt.Equal(transfers[i].CompletedAt) {
			t.Fatalf("recovered=%+v", final)
		}
		conn, reader = openTestWebSocket(t, ts, "/v1/sftp/"+sessions[i].ID+"/transfers/events")
		_, payload, err = readServerFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Transfers) != 1 || snapshot.Transfers[0].ID != final.ID {
			t.Fatalf("reconnect snapshot=%+v", snapshot)
		}
		_ = conn.Close()
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, authenticatedRequest(method, "/v1/sftp/"+sessions[1-i].ID+"/transfers/"+transfers[i].ID))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("cross-session %s=%d", method, rec.Code)
			}
		}
	}
}
