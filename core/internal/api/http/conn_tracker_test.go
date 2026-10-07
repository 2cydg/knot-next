package http

import (
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"
)

// R02: a hijacked WebSocket is invisible to http.Server.Shutdown, so teardown
// must close it explicitly. Without the tracker the handler, its relays, and its
// session attachment survive the whole shutdown.
func TestConnTrackerClosesHijackedConnections(t *testing.T) {
	tracker := NewConnTracker()
	server := newStatefulTestServerWithOptions(t, WithConnTracker(tracker))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	conn, _ := openTestWebSocketWithDeadline(t, ts, "/v1/events", 2*time.Second)

	waitForTrackedConns(t, tracker, 1)

	tracker.CloseAll()

	// The client observes the connection ending rather than staying subscribed.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := conn.Read(buf); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Any read error means the connection is gone, which is the point.
			break
		}
	}

	waitForTrackedConns(t, tracker, 0)

	// Connections that arrive after teardown began are refused instead of being
	// served by a server that is already gone.
	if _, _, status := tryOpenTestWebSocket(t, ts, "/v1/events", 2*time.Second); containsStatus(status, "101") {
		t.Fatalf("websocket upgrade succeeded after teardown: %q", status)
	}
}

func TestConnTrackerReleasesOnNormalClose(t *testing.T) {
	tracker := NewConnTracker()
	server := newStatefulTestServerWithOptions(t, WithConnTracker(tracker))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	conn, _ := openTestWebSocketWithDeadline(t, ts, "/v1/events", 2*time.Second)
	waitForTrackedConns(t, tracker, 1)

	_ = conn.Close()
	waitForTrackedConns(t, tracker, 0)
}

func waitForTrackedConns(t *testing.T, tracker *ConnTracker, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.Count() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tracked connections = %d, want %d", tracker.Count(), want)
}

func containsStatus(status, want string) bool {
	for i := 0; i+len(want) <= len(status); i++ {
		if status[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
