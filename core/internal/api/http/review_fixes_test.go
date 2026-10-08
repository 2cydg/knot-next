package http

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"knot-core/internal/testutil/sshserver"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReviewAttachCloseEcho(t *testing.T) {
	remote := startAttachSSHServer(t, sshserver.ShellBehavior{})
	server, id := newSSHBackedTestServer(t, remote)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	created := createAttachSession(t, server, id, "")
	conn, reader := openTestWebSocket(t, ts, created.AttachURL)
	defer conn.Close()
	readInitialAttachFrames(t, reader)
	payload := binary.BigEndian.AppendUint16(nil, 1000)
	payload = append(payload, []byte("detach")...)
	if _, err := conn.Write(maskedClientFrame(wsOpcodeClose, payload)); err != nil {
		t.Fatal(err)
	}
	for {
		op, got, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("close echo missing: %v", err)
		}
		if op == wsOpcodeClose {
			if !bytes.Equal(got, payload) {
				t.Fatalf("close payload: %v", got)
			}
			break
		}
	}
	if _, _, err := readServerFrame(reader); err != io.EOF {
		t.Fatalf("data after close: %v", err)
	}
	waitForAttachState(t, server, created.ID, 5*time.Second, "detached")
}

func TestReviewWebSocketLengthBoundaries(t *testing.T) {
	for _, size := range []int{125, 126, 65535, 65536, maxWSFrameSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), size)
			c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(maskedClientFrame(wsOpcodeBinary, data))), bufio.NewWriter(io.Discard))}
			op, got, err := c.ReadFrame()
			if err != nil || op != wsOpcodeBinary || !bytes.Equal(got, data) {
				t.Fatalf("size %d: op=%d length=%d err=%v", size, op, len(got), err)
			}
		})
	}
	first := maskedClientFrame(wsOpcodeBinary, bytes.Repeat([]byte("x"), maxWSFrameSize))
	first[0] &= 0x7f
	for _, tc := range []struct {
		wire []byte
		want string
	}{
		{append(first, maskedClientFrame(0, []byte("x"))...), "message is too large"},
		{maskedClientFrame(wsOpcodeBinary, make([]byte, maxWSFrameSize+1)), "frame is too large"},
	} {
		c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(tc.wire)), bufio.NewWriter(io.Discard))}
		if _, _, err := c.ReadFrame(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("size rejection: %v want %s", err, tc.want)
		}
	}
	// A fragmented message exactly at the aggregate limit must be accepted.
	first = maskedClientFrame(wsOpcodeBinary, bytes.Repeat([]byte("x"), maxWSFrameSize-1))
	first[0] &= 0x7f
	wire := append(first, maskedClientFrame(0, []byte("x"))...)
	c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(wire)), bufio.NewWriter(io.Discard))}
	if _, got, err := c.ReadFrame(); err != nil || len(got) != maxWSFrameSize {
		t.Fatalf("exact fragmented limit: %d %v", len(got), err)
	}
}

func TestReviewWebSocketOversizeCloseCode(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &websocketConn{c: a, rw: bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))}
	done := make(chan error, 1)
	go func() { _, _, err := c.ReadFrame(); done <- err }()
	// Only the header is needed: the server must reject before allocating payload.
	header := []byte{0x82, 0xff}
	header = binary.BigEndian.AppendUint64(header, maxWSFrameSize+1)
	if err := b.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(header); err != nil {
		t.Fatal(err)
	}
	op, payload, err := readServerFrame(bufio.NewReader(b))
	if err != nil || op != wsOpcodeClose || len(payload) != 2 || binary.BigEndian.Uint16(payload) != 1009 {
		t.Fatalf("oversize close: %d %v %v", op, payload, err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "frame is too large") {
		t.Fatalf("read error: %v", err)
	}
}

func TestReviewInvalidOutboundFrameKeepsConnection(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &websocketConn{c: a, rw: bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))}
	if err := c.WriteFrame(3, nil); err == nil {
		t.Fatal("invalid opcode accepted")
	}
	done := make(chan error, 1)
	go func() { done <- c.WriteFrame(wsOpcodePing, []byte("alive")) }()
	if err := b.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	op, payload, err := readServerFrame(bufio.NewReader(b))
	if err != nil || op != wsOpcodePing || string(payload) != "alive" {
		t.Fatalf("connection lost on argument error: %d %q %v", op, payload, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
