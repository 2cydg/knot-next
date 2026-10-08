package http

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	stdhttp "net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	wsOpcodeText   = 1
	wsOpcodeBinary = 2
	wsOpcodeClose  = 8
	wsOpcodePing   = 9
	wsOpcodePong   = 10
)

type websocketConn struct {
	rw             *bufio.ReadWriter
	c              net.Conn
	writeMu        sync.Mutex
	fragmentOpcode int
	fragment       []byte
	writeTimeout   time.Duration
	// tracker is the registry this connection was handed to at upgrade, so
	// Close removes it and teardown never closes it twice.
	tracker *ConnTracker
}

// upgradeWebSocket hijacks the connection and, when a tracker is configured,
// registers it so service teardown can close it. A connection that arrives
// after teardown began is closed instead of served.
func (s *Server) upgradeWebSocket(w stdhttp.ResponseWriter, r *stdhttp.Request) (*websocketConn, error) {
	// Refuse before the upgrade when teardown has begun: an upgraded connection
	// would already have received a 101 that teardown is about to invalidate.
	if s.tracker != nil && !s.tracker.accepting() {
		return nil, errServerShuttingDown
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing websocket upgrade header")
	}
	upgrade := false
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			upgrade = true
		}
	}
	if !upgrade {
		return nil, errors.New("missing connection upgrade header")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	decodedKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decodedKey) != 16 {
		return nil, errors.New("invalid Sec-WebSocket-Key")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("unsupported WebSocket version")
	}
	hijacker, ok := w.(stdhttp.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	accept := websocketAccept(key)
	_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	ws := &websocketConn{rw: rw, c: conn}
	if s.tracker != nil {
		// Publish the back-reference before the connection becomes reachable
		// through the tracker, so a concurrent CloseAll can never observe a
		// tracked connection that does not yet know how to deregister itself.
		ws.tracker = s.tracker
		if !s.tracker.track(ws) {
			_ = conn.Close()
			return nil, errServerShuttingDown
		}
	}
	return ws, nil
}

var errServerShuttingDown = errors.New("server is shutting down")

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (c *websocketConn) Close() error {
	if c.tracker != nil {
		c.tracker.release(c)
	}
	return c.c.Close()
}

// ReadFrame returns complete data messages and interleaved control frames.
// Fragment state is retained across a ping/pong, preserving every PTY byte.
func (c *websocketConn) ReadFrame() (int, []byte, error) {
	for {
		opcode, payload, final, err := c.readWireFrame()
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case wsOpcodeClose, wsOpcodePing, wsOpcodePong:
			return opcode, payload, nil
		case 0:
			if c.fragmentOpcode == 0 {
				return 0, nil, c.protocolError("unexpected continuation")
			}
			if len(c.fragment)+len(payload) > maxWSFrameSize {
				return 0, nil, c.closeError(1009, "message is too large")
			}
			c.fragment = append(c.fragment, payload...)
			if !final {
				continue
			}
			opcode, payload = c.fragmentOpcode, c.fragment
			c.fragmentOpcode, c.fragment = 0, nil
		default:
			if c.fragmentOpcode != 0 {
				return 0, nil, c.protocolError("data frame interrupts fragmented message")
			}
			if !final {
				c.fragmentOpcode, c.fragment = opcode, payload
				continue
			}
		}
		if opcode == wsOpcodeText && !utf8.Valid(payload) {
			return 0, nil, c.protocolError("invalid UTF-8 text")
		}
		return opcode, payload, nil
	}
}
func (c *websocketConn) protocolError(message string) error {
	return c.closeError(1002, message)
}

func (c *websocketConn) closeError(code uint16, message string) error {
	if c.c != nil {
		_ = c.WriteFrame(wsOpcodeClose, binary.BigEndian.AppendUint16(nil, code))
		_ = c.Close()
	}
	return errors.New("websocket: " + message)
}
func (c *websocketConn) readWireFrame() (int, []byte, bool, error) {
	first, err := c.rw.ReadByte()
	if err != nil {
		return 0, nil, false, err
	}
	second, err := c.rw.ReadByte()
	if err != nil {
		return 0, nil, false, err
	}
	final, opcode := first&0x80 != 0, int(first&0xf)
	control := opcode >= 8
	if first&0x70 != 0 || (opcode != 0 && opcode != 1 && opcode != 2 && opcode != 8 && opcode != 9 && opcode != 10) {
		return 0, nil, false, c.protocolError("unsupported frame flags or opcode")
	}
	if second&0x80 == 0 {
		return 0, nil, false, c.protocolError("client frames must be masked")
	}
	length := uint64(second & 0x7f)
	if control && (!final || length > 125) {
		return 0, nil, false, c.protocolError("invalid control frame")
	}
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.rw, b[:]); err != nil {
			return 0, nil, false, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
		if length < 126 {
			return 0, nil, false, c.protocolError("non-minimal frame length")
		}
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.rw, b[:]); err != nil {
			return 0, nil, false, err
		}
		length = binary.BigEndian.Uint64(b[:])
		if length < 65536 || length>>63 != 0 {
			return 0, nil, false, c.protocolError("invalid frame length")
		}
	}
	if length > maxWSFrameSize {
		return 0, nil, false, c.closeError(1009, "frame is too large")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.rw, mask[:]); err != nil {
		return 0, nil, false, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return 0, nil, false, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	if opcode == wsOpcodeClose && !validClosePayload(payload) {
		return 0, nil, false, c.protocolError("invalid close payload")
	}
	return opcode, payload, final, nil
}
func validClosePayload(payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	if len(payload) == 1 || !utf8.Valid(payload[2:]) {
		return false
	}
	code := binary.BigEndian.Uint16(payload[:2])
	return (code >= 1000 && code <= 1014 && code != 1004 && code != 1005 && code != 1006) || (code >= 3000 && code <= 4999)
}

func (c *websocketConn) WriteFrame(opcode int, payload []byte) (err error) {
	if opcode != 1 && opcode != 2 && opcode != 8 && opcode != 9 && opcode != 10 {
		return errors.New("invalid websocket opcode")
	}
	if opcode >= 8 && len(payload) > 125 {
		return errors.New("control payload is too large")
	}
	if opcode == wsOpcodeClose && !validClosePayload(payload) {
		return errors.New("invalid close payload")
	}
	if len(payload) > maxWSFrameSize {
		return errors.New("websocket frame is too large")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// Set the deadline while holding the serialization lock: ping and data cannot
	// race to extend the deadline of a stalled write.
	if c.c != nil {
		timeout := c.writeTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		if err = c.c.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			_ = c.Close()
			return err
		}
		defer func() {
			if err != nil {
				_ = c.Close()
			}
		}()
	}
	header := []byte{0x80 | byte(opcode)}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		header = append(header, 127)
		header = append(header, length[:]...)
	}
	if _, err := c.rw.Write(header); err != nil {
		return err
	}
	if _, err := c.rw.Write(payload); err != nil {
		return err
	}
	return c.rw.Flush()
}

func (c *websocketConn) drainControlFrames(cancel context.CancelFunc) {
	defer cancel()
	for {
		opcode, payload, err := c.ReadFrame()
		if err != nil {
			return
		}
		switch opcode {
		case wsOpcodePing:
			if err := c.WriteFrame(wsOpcodePong, payload); err != nil {
				return
			}
		case wsOpcodeClose:
			_ = c.WriteFrame(wsOpcodeClose, payload)
			return
		}
	}
}
