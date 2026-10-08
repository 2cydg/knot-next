package http

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegressionFragmentedMessage(t *testing.T) {
	a := maskedClientFrame(wsOpcodeBinary, []byte("hello"))
	a[0] &= 0x7f
	b := maskedClientFrame(0, []byte(" world"))
	c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(append(a, b...))), bufio.NewWriter(io.Discard))}
	op, p, err := c.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if op != wsOpcodeBinary || string(p) != "hello world" {
		t.Errorf("fragmented message delivered prematurely: opcode=%d payload=%q", op, p)
	}
	op, p, err = c.ReadFrame()
	if err != io.EOF {
		t.Fatalf("extra delivery: %d %q %v", op, p, err)
	}
}
func TestRegressionInvalidControl(t *testing.T) {
	frame := maskedClientFrame(wsOpcodePing, []byte("x"))
	frame[0] &= 0x7f
	c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(frame)), bufio.NewWriter(io.Discard))}
	if op, _, err := c.ReadFrame(); err == nil {
		t.Errorf("fragmented ping accepted: opcode=%d", op)
	}
}
func TestRegressionJSONLimit(t *testing.T) {
	body := "{}" + strings.Repeat(" ", maxJSONBody) + "invalid trailing data"
	req := httptest.NewRequest("POST", "/v1/config/servers", strings.NewReader(body))
	rec := httptest.NewRecorder()
	var dst struct{}
	if decodeJSON(rec, req, &dst) {
		t.Errorf("accepted %d-byte body with trailing invalid data beyond limit", len(body))
	}
}
func TestRegressionEventWriteBound(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &websocketConn{writeTimeout: 20 * time.Millisecond, c: a, rw: bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))}
	done := make(chan error, 1)
	go func() { done <- c.WriteFrame(wsOpcodeText, []byte("event")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled write succeeded")
		}
	case <-time.After(time.Second):
		t.Error("event WriteFrame has no deadline and remains blocked until connection closes")
	}

}

func TestWebSocketProtocolMatrix(t *testing.T) {
	for _, opcode := range []int{wsOpcodeBinary, wsOpcodeText} {
		first := maskedClientFrame(opcode, []byte("hello"))
		first[0] &= 0x7f
		ping := maskedClientFrame(wsOpcodePing, []byte("p"))
		last := maskedClientFrame(0, []byte(" world"))
		c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(append(append(first, ping...), last...))), bufio.NewWriter(io.Discard))}
		op, p, err := c.ReadFrame()
		if err != nil || op != wsOpcodePing || string(p) != "p" {
			t.Fatalf("interleaved ping: %d %q %v", op, p, err)
		}
		op, p, err = c.ReadFrame()
		if err != nil || op != opcode || string(p) != "hello world" {
			t.Fatalf("message: %d %q %v", op, p, err)
		}
	}
	oversized := maskedClientFrame(wsOpcodeBinary, bytes.Repeat([]byte("x"), maxWSFrameSize))
	oversized[0] &= 0x7f
	for name, wire := range map[string][]byte{
		"orphan": maskedClientFrame(0, nil), "reserved": maskedClientFrame(3, nil),
		"control length": maskedClientFrame(wsOpcodePing, make([]byte, 126)),
		"close length":   maskedClientFrame(wsOpcodeClose, []byte{1}),
		"close status":   maskedClientFrame(wsOpcodeClose, []byte{3, 237}),
		"text UTF8":      maskedClientFrame(wsOpcodeText, []byte{255}),
		"message limit":  append(oversized, maskedClientFrame(0, []byte("x"))...),
	} {
		t.Run(name, func(t *testing.T) {
			c := &websocketConn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(wire)), bufio.NewWriter(io.Discard))}
			_, _, err := c.ReadFrame()
			if err == nil {
				t.Fatal("invalid frame accepted")
			}
			if name == "message limit" && !strings.Contains(err.Error(), "message is too large") {
				t.Fatalf("wrong rejection: %v", err)
			}
		})
	}
}
func TestJSONBodyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{"{}" + strings.Repeat(" ", maxJSONBody-2), true},
		{"{}" + strings.Repeat(" ", maxJSONBody-1), false},
		{"null", false}, {"[]", false}, {"{} {}", false}, {"{} trailing", false}, {"{\"unknown\":1}", false},
	} {
		req := httptest.NewRequest("POST", "/v1/config", strings.NewReader(tc.body))
		var dst struct{}
		if got := decodeJSON(httptest.NewRecorder(), req, &dst); got != tc.valid {
			t.Errorf("len=%d valid=%v want=%v", len(tc.body), got, tc.valid)
		}
	}
}
