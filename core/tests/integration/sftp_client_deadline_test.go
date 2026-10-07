package integration

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// This tests the WS client's timeout, not SSH/SFTP business behavior. Both a
// stalled stream and continuous progress must use one total waiting budget.
func TestSFTPProtocolTerminalEventDeadline(t *testing.T) {
	for _, progress := range []bool{false, true} {
		name := "idle"
		if progress {
			name = "continuous_progress"
		}
		t.Run(name, func(t *testing.T) {
			stop, done := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
				defer close(done)
				defer conn.Close()
				if err := websocket.JSON.Send(conn, transferMessageJSON{Type: "sftp.transfer.snapshot", SessionID: "session", Transfers: []transferJSON{}}); err != nil {
					return
				}
				if !progress {
					<-stop
					return
				}
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tick.C:
						if err := websocket.JSON.Send(conn, transferMessageJSON{Type: "sftp.transfer.progress", SessionID: "session", TransferID: "expected", State: "running"}); err != nil {
							return
						}
					}
				}
			}))
			t.Cleanup(func() {
				close(stop)
				server.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("WS deadline fixture did not stop")
				}
			})
			client := &sftpAPIClient{baseURL: server.URL, token: "test-token"}
			conn, _ := client.subscribe(t, "session")
			start := time.Now()
			err := receiveTerminalTransferEvent(conn, transferJSON{ID: "expected", SessionID: "session", State: "completed"}, start.Add(60*time.Millisecond))
			if !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "expected") {
				t.Fatalf("timeout missing cause or transfer ID: %v", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("message reads extended total deadline: %s", elapsed)
			}
		})
	}
}
