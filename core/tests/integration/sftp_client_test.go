package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

const sftpTestOrigin = "http://sftp-integration.test"

// These wire models deliberately do not import the server's resource types.
type sftpSessionJSON struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Backend string `json:"backend"`
}

type transferJSON struct {
	ID          string             `json:"id"`
	SessionID   string             `json:"session_id"`
	Direction   string             `json:"direction"`
	State       string             `json:"state"`
	Error       string             `json:"error"`
	BytesTotal  int64              `json:"bytes_total"`
	BytesCopied int64              `json:"bytes_copied"`
	FilesTotal  int                `json:"files_total"`
	FilesDone   int                `json:"files_done"`
	Items       []transferItemJSON `json:"items"`
	StartedAt   time.Time          `json:"started_at"`
	CompletedAt time.Time          `json:"completed_at"`
}

type transferItemJSON struct {
	Source string `json:"source"`
	State  string `json:"state"`
	Error  string `json:"error"`
}

type transferMessageJSON struct {
	Type        string         `json:"type"`
	SessionID   string         `json:"session_id"`
	TransferID  string         `json:"transfer_id"`
	State       string         `json:"state"`
	BytesTotal  int64          `json:"bytes_total"`
	BytesCopied int64          `json:"bytes_copied"`
	FilesTotal  int            `json:"files_total"`
	FilesDone   int            `json:"files_done"`
	Transfers   []transferJSON `json:"transfers"`
}

type sftpAPIClient struct {
	baseURL   string
	token     string
	serverRef string
	http      *http.Client
}

func (c *sftpAPIClient) call(t *testing.T, method, route string, body any, status int, result any) {
	t.Helper()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.baseURL+route, input)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, route, err)
	}
	defer resp.Body.Close()
	var wire struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		t.Fatalf("decode %s %s: %v", method, route, err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d error=%+v", method, route, resp.StatusCode, status, wire.Error)
	}
	if status >= 400 && wire.Error.Code == "" {
		t.Fatal("API failure missing error code")
	}
	if result != nil {
		if err := json.Unmarshal(wire.Data, result); err != nil {
			t.Fatalf("decode data for %s: %v", route, err)
		}
	}
}

func (c *sftpAPIClient) createSession(t *testing.T) sftpSessionJSON {
	t.Helper()
	var session sftpSessionJSON
	c.call(t, http.MethodPost, "/v1/sftp", map[string]any{
		"server_ref": c.serverRef, "host_key_policy": "insecure-skip",
	}, http.StatusCreated, &session)
	if session.ID == "" || session.State != "connecting" || session.Backend != "ssh-sftp" {
		t.Fatalf("creation is not an asynchronous accepted resource: %+v", session)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.call(t, http.MethodGet, "/v1/sftp/"+session.ID, nil, http.StatusOK, &session)
		if session.State == "open" {
			if session.Backend != "ssh-sftp" {
				t.Fatalf("unexpected backend: %+v", session)
			}
			return session
		}
		if session.State != "connecting" {
			t.Fatalf("unexpected connection state: %+v", session)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("SFTP did not open: %+v", session)
	return sftpSessionJSON{}
}

func transferRoute(sessionID, transferID string) string {
	return "/v1/sftp/" + sessionID + "/transfers/" + transferID
}

func transferTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "partial_failed" || state == "canceled"
}

func (c *sftpAPIClient) waitTransfer(t *testing.T, sessionID, transferID string) transferJSON {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var current transferJSON
	for time.Now().Before(deadline) {
		c.call(t, http.MethodGet, transferRoute(sessionID, transferID), nil, http.StatusOK, &current)
		if current.ID != transferID || current.SessionID != sessionID {
			t.Fatalf("GET returned another transfer: %+v", current)
		}
		if current.BytesCopied < 0 || current.BytesCopied > current.BytesTotal {
			t.Fatalf("invalid progress: %+v", current)
		}
		if transferTerminal(current.State) {
			if current.StartedAt.IsZero() || current.CompletedAt.Before(current.StartedAt) {
				t.Fatalf("invalid terminal timestamps: %+v", current)
			}
			return current
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("transfer did not settle: %+v", current)
	return transferJSON{}
}

func (c *sftpAPIClient) startTransfer(t *testing.T, sessionID, operation string, body any) transferJSON {
	t.Helper()
	var accepted transferJSON
	c.call(t, http.MethodPost, "/v1/sftp/"+sessionID+"/"+operation, body, http.StatusAccepted, &accepted)
	if accepted.ID == "" || accepted.State != "queued" || accepted.SessionID != sessionID {
		t.Fatalf("invalid accepted transfer: %+v", accepted)
	}
	return accepted
}

func (c *sftpAPIClient) subscribe(t *testing.T, sessionID string) (*websocket.Conn, transferMessageJSON) {
	t.Helper()
	config, err := websocket.NewConfig(strings.Replace(c.baseURL, "http://", "ws://", 1)+"/v1/sftp/"+sessionID+"/transfers/events", sftpTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set("Authorization", "Bearer "+c.token)
	config.Dialer = &net.Dialer{Timeout: 5 * time.Second}
	conn, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	snapshot := readTransferMessage(t, conn)
	if snapshot.Type != "sftp.transfer.snapshot" || snapshot.SessionID != sessionID || snapshot.Transfers == nil {
		t.Fatalf("invalid transfer snapshot: %+v", snapshot)
	}
	return conn, snapshot
}

func readTransferMessage(t *testing.T, conn *websocket.Conn) transferMessageJSON {
	t.Helper()
	message, err := receiveTransferMessage(conn, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("read transfer message: %v", err)
	}
	return message
}

func receiveTransferMessage(conn *websocket.Conn, deadline time.Time) (transferMessageJSON, error) {
	var message transferMessageJSON
	if err := conn.SetReadDeadline(deadline); err != nil {
		return message, err
	}
	err := websocket.JSON.Receive(conn, &message)
	return message, err
}

func waitTransferEvent(t *testing.T, conn *websocket.Conn, final transferJSON) {
	t.Helper()
	if err := receiveTerminalTransferEvent(conn, final, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
}

// Every message uses the same deadline: ongoing progress must not extend the
// total wait, and read failures must identify the transfer being awaited.
func receiveTerminalTransferEvent(conn *websocket.Conn, final transferJSON, deadline time.Time) error {
	for time.Now().Before(deadline) {
		message, err := receiveTransferMessage(conn, deadline)
		if err != nil {
			return fmt.Errorf("missing terminal event for %s: %w", final.ID, err)
		}
		if message.TransferID != final.ID || message.SessionID != final.SessionID {
			return fmt.Errorf("event for a different resource while waiting for %s: %+v", final.ID, message)
		}
		if message.Type != "sftp.transfer."+final.State {
			continue
		}
		if message.State != final.State || message.BytesTotal != final.BytesTotal || message.BytesCopied != final.BytesCopied || message.FilesTotal != final.FilesTotal || message.FilesDone != final.FilesDone {
			return fmt.Errorf("event disagrees with GET: event=%+v final=%+v", message, final)
		}
		return nil
	}
	return fmt.Errorf("missing terminal event for %s: %w", final.ID, os.ErrDeadlineExceeded)
}
