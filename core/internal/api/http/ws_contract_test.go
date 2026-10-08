package http

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	"knot-core/internal/auth"
	"knot-core/internal/testutil/sshserver"
)

// The standard x/net client has no fragmentation API. This transport adapter
// splits its first binary frame on the wire while leaving the handshake and
// independently implemented receive/close behavior to the standard client.
type fragmentTransport struct {
	net.Conn
	mu    sync.Mutex
	split bool
}

func (c *fragmentTransport) Write(raw []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.split && len(raw) > 6 && raw[0] == 0x82 && raw[1]&0x80 != 0 && raw[1]&0x7f < 126 && len(raw) == 6+int(raw[1]&0x7f) {
		c.split = true
		payload := append([]byte(nil), raw[6:]...)
		for i := range payload {
			payload[i] ^= raw[2+i%4]
		}
		middle := len(payload) / 2
		first := maskedClientFrame(wsOpcodeBinary, payload[:middle])
		first[0] &= 0x7f
		wire := append(first, maskedClientFrame(0, payload[middle:])...)
		if _, err := io.Copy(c.Conn, bytes.NewReader(wire)); err != nil {
			return 0, err
		}
		return len(raw), nil
	}
	return c.Conn.Write(raw)
}
func TestStandardClientFragmentedBinaryMessage(t *testing.T) {
	remote := startAttachSSHServer(t, sshserver.ShellBehavior{})
	server, serverID := newSSHBackedTestServer(t, remote)
	server.origins = auth.NewOriginChecker([]string{"http://localhost"})
	created := createAttachSession(t, server, serverID, "")
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := &fragmentTransport{Conn: raw}
	defer transport.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	cfg, err := websocket.NewConfig(strings.Replace(ts.URL, "http://", "ws://", 1)+"/v1/sessions/"+created.ID+"/attach", "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Header.Set("Authorization", "Bearer test-token")
	ws, err := websocket.NewClient(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var initial string
	if err = websocket.Message.Receive(ws, &initial); err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello\x00\xff\r\n world\n")
	if err = websocket.Message.Send(ws, payload); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < len(payload) {
		var message []byte
		if err = websocket.Message.Receive(ws, &message); err != nil {
			t.Fatal(err)
		}
		if len(message) > 0 && message[0] == '{' {
			continue
		}
		got = append(got, message...)
	}
	if !transport.split || !bytes.Equal(got, payload) {
		t.Fatalf("split=%v PTY=%q want=%q", transport.split, got, payload)
	}
}
func TestWebSocketCloseEcho(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := &websocketConn{c: a, rw: bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))}
	defer c.Close()
	done := make(chan struct{})
	go func() { c.drainControlFrames(func() { close(done) }) }()
	payload := []byte{3, 232, 'b', 'y', 'e'}
	if _, err := b.Write(maskedClientFrame(wsOpcodeClose, payload)); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(b)
	op, p, err := readServerFrame(reader)
	if err != nil || op != wsOpcodeClose || !bytes.Equal(p, payload) {
		t.Fatalf("close echo %d %q %v", op, p, err)
	}
	<-done
}
