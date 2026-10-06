package http

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"strings"
	"sync"
	"time"

	"knot-core/internal/api/response"
	"knot-core/pkg/session"
)

const maxWSFrameSize = 1 << 20

func (s *Server) handleSessions(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	sessionService := s.core.Session()
	if sessionService == nil {
		writeAPIError(w, stdhttp.StatusServiceUnavailable, response.RiskReadOnly, "sessions", "CAPABILITY_UNAVAILABLE", "session service is not available")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions")
	parts := splitPath(path)
	switch {
	case len(parts) == 0:
		switch r.Method {
		case stdhttp.MethodGet:
			response.JSON(w, stdhttp.StatusOK, sessionService.ListWithOptions(session.ListOptions{
				ServerRef: r.URL.Query().Get("server_ref"),
				Alias:     r.URL.Query().Get("alias"),
				State:     r.URL.Query().Get("state"),
			}))
		case stdhttp.MethodPost:
			var body session.CreateRequest
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := sessionService.Create(body)
			writeSessionCreatedResult(w, data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 1 && parts[0] == "exec":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body session.ExecRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sessionService.Exec(body)
		writeSessionResult(w, response.RiskLongRunning, "sessions/exec", data, err)
	case len(parts) == 1:
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := sessionService.Get(id)
			writeSessionResult(w, response.RiskReadOnly, "sessions/"+id, data, err)
		case stdhttp.MethodDelete:
			data, err := sessionService.Disconnect(id)
			writeSessionResult(w, response.RiskLongRunning, "sessions/"+id, data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 2 && parts[1] == "attach":
		if r.Method != stdhttp.MethodGet {
			requireMethod(w, r, stdhttp.MethodGet)
			return
		}
		s.attachSessionWS(w, r, sessionService, parts[0])
	case len(parts) == 2 && parts[1] == "events":
		if r.Method != stdhttp.MethodGet {
			requireMethod(w, r, stdhttp.MethodGet)
			return
		}
		s.sessionEventsWS(w, r, sessionService, parts[0])
	case len(parts) == 2 && parts[1] == "control":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body session.ControlRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sessionService.Control(parts[0], body)
		writeSessionResult(w, response.RiskLongRunning, "sessions/"+parts[0]+"/control", data, err)
	case len(parts) == 3 && parts[1] == "challenges" && parts[2] == "host-key":
		s.handleHostKeyChallenge(w, r, sessionService, parts[0])
	case len(parts) == 3 && parts[1] == "challenges" && parts[2] == "auth":
		s.handleAuthChallenge(w, r, sessionService, parts[0])
	default:
		s.notFound(w, r)
	}
}

func relayAttachOutput(ctx context.Context, cancel context.CancelFunc, writeFrame func(int, []byte) error, ch <-chan []byte, opcode int) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if len(payload) > 0 {
				if err := writeFrame(opcode, payload); err != nil {
					return
				}
			}
		}
	}
}

func relayAttachInput(ctx context.Context, cancel context.CancelFunc, input io.Writer, in <-chan []byte) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-in:
			if len(payload) == 0 {
				continue
			}
			if _, err := input.Write(payload); err != nil {
				return
			}
		}
	}
}

func pingAttach(ctx context.Context, cancel context.CancelFunc, writeFrame func(int, []byte) error) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeFrame(wsOpcodePing, nil); err != nil {
				cancel()
				return
			}
		}
	}
}

func handleAttachControl(sessionService *session.Service, id string, payload []byte) error {
	var msg session.ControlRequest
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}
	if msg.Type == "" {
		return errors.New("control type is required")
	}
	_, err := sessionService.Control(id, msg)
	return err
}

func (s *Server) handleHostKeyChallenge(w stdhttp.ResponseWriter, r *stdhttp.Request, sessionService *session.Service, id string) {
	switch r.Method {
	case stdhttp.MethodGet:
		data, err := sessionService.HostKeyChallenge(id)
		writeSessionResult(w, response.RiskReadOnly, "sessions/"+id+"/challenges/host-key", data, err)
	case stdhttp.MethodPost:
		var body session.ChallengeResponse
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sessionService.RespondHostKeyChallenge(id, body)
		writeSessionResult(w, response.RiskLongRunning, "sessions/"+id+"/challenges/host-key", data, err)
	default:
		requireMethod(w, r, stdhttp.MethodGet)
	}
}

func (s *Server) handleAuthChallenge(w stdhttp.ResponseWriter, r *stdhttp.Request, sessionService *session.Service, id string) {
	switch r.Method {
	case stdhttp.MethodGet:
		data, err := sessionService.AuthChallenge(id)
		writeSessionResult(w, response.RiskReadOnly, "sessions/"+id+"/challenges/auth", data, err)
	case stdhttp.MethodPost:
		var body session.ChallengeResponse
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sessionService.RespondAuthChallenge(id, body)
		writeSessionResult(w, response.RiskLongRunning, "sessions/"+id+"/challenges/auth", data, err)
	default:
		requireMethod(w, r, stdhttp.MethodGet)
	}
}

func writeSessionCreatedResult(w stdhttp.ResponseWriter, data any, err error) {
	if err != nil {
		writeSessionError(w, response.RiskLongRunning, "sessions", err)
		return
	}
	response.JSON(w, stdhttp.StatusCreated, data)
}

func writeSessionResult(w stdhttp.ResponseWriter, risk response.Risk, resource string, data any, err error) {
	if err != nil {
		writeSessionError(w, risk, resource, err)
		return
	}
	response.JSON(w, stdhttp.StatusOK, data)
}

func writeSessionError(w stdhttp.ResponseWriter, risk response.Risk, resource string, err error) {
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeAPIError(w, stdhttp.StatusNotFound, risk, resource, "NOT_FOUND", "session resource was not found")
	case errors.Is(err, session.ErrConflict):
		writeAPIError(w, stdhttp.StatusConflict, risk, resource, "CONFLICT", err.Error())
	case errors.Is(err, session.ErrValidation):
		writeAPIError(w, stdhttp.StatusBadRequest, risk, resource, "VALIDATION_FAILED", err.Error())
	default:
		writeAPIError(w, stdhttp.StatusInternalServerError, risk, resource, "INTERNAL_ERROR", err.Error())
	}
}

func (s *Server) attachSessionWS(w stdhttp.ResponseWriter, r *stdhttp.Request, sessionService *session.Service, id string) {
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskLongRunning, "sessions/"+id+"/attach", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()
	stream, data, err := sessionService.AttachStream(id)
	if err != nil {
		_ = conn.WriteFrame(wsOpcodeText, []byte(fmt.Sprintf(`{"type":"error","code":"ATTACH_FAILED","message":%q}`, err.Error())))
		return
	}
	events, cancelEvents, _, err := sessionService.Subscribe(id)
	if err != nil {
		_ = conn.WriteFrame(wsOpcodeText, []byte(fmt.Sprintf(`{"type":"error","code":"ATTACH_FAILED","message":%q}`, err.Error())))
		return
	}
	defer cancelEvents()
	defer stream.Cancel()
	defer func() {
		_, _ = sessionService.Detach(id)
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	writeFrame := func(opcode int, payload []byte) error {
		return conn.WriteFrame(opcode, payload)
	}
	snapshotPayload, _ := json.Marshal(map[string]any{
		"type":    "session.snapshot",
		"session": data,
	})
	_ = writeFrame(wsOpcodeText, snapshotPayload)
	attachedPayload, _ := json.Marshal(map[string]any{
		"type":       "session.attached",
		"session_id": data.ID,
		"state":      data.State,
	})
	_ = writeFrame(wsOpcodeText, attachedPayload)
	stdinWrites := make(chan []byte, 32)
	go relayAttachInput(ctx, cancel, stream.Input, stdinWrites)
	go relayAttachOutput(ctx, cancel, writeFrame, stream.Stdout, wsOpcodeBinary)
	go relayAttachOutput(ctx, cancel, writeFrame, stream.Stderr, wsOpcodeBinary)
	go relayAttachEvents(ctx, cancel, writeFrame, events)
	go pingAttach(ctx, cancel, writeFrame)
	go func() {
		select {
		case err := <-stream.Done:
			current, getErr := sessionService.Get(id)
			payload := map[string]any{
				"type":       "session.exit",
				"session_id": data.ID,
			}
			if getErr == nil {
				payload["exit_code"] = current.ExitCode
				if current.FrameworkError != "" {
					payload["error"] = current.FrameworkError
				}
			} else if err != nil {
				payload["error"] = err.Error()
			}
			encoded, _ := json.Marshal(payload)
			_ = writeFrame(wsOpcodeText, encoded)
		case <-ctx.Done():
		}
		cancel()
	}()
	for {
		opcode, payload, err := conn.ReadFrame()
		if err != nil {
			return
		}
		switch opcode {
		case wsOpcodeBinary:
			if len(payload) == 0 {
				break
			}
			chunk := make([]byte, len(payload))
			copy(chunk, payload)
			select {
			case stdinWrites <- chunk:
			case <-ctx.Done():
				return
			}
		case wsOpcodeText:
			if err := handleAttachControl(sessionService, id, payload); err != nil {
				_ = writeFrame(wsOpcodeText, []byte(fmt.Sprintf(`{"type":"error","code":"CONTROL_FAILED","message":%q}`, err.Error())))
			}
		case wsOpcodePing:
			if err := writeFrame(wsOpcodePong, payload); err != nil {
				return
			}
		case wsOpcodeClose:
			_ = writeFrame(wsOpcodeClose, nil)
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func relayAttachEvents(ctx context.Context, cancel context.CancelFunc, writeFrame func(int, []byte) error, events <-chan session.Event) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				return
			}
			if err := writeFrame(wsOpcodeText, payload); err != nil {
				return
			}
		}
	}
}

func (s *Server) sessionEventsWS(w stdhttp.ResponseWriter, r *stdhttp.Request, sessionService *session.Service, id string) {
	events, cancel, data, err := sessionService.Subscribe(id)
	if err != nil {
		writeSessionError(w, response.RiskReadOnly, "sessions/"+id+"/events", err)
		return
	}
	defer cancel()
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, "sessions/"+id+"/events", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go conn.drainControlFrames(cancel)
	payload, _ := json.Marshal(map[string]any{
		"type":    "session.snapshot",
		"session": data,
	})
	_ = conn.WriteFrame(wsOpcodeText, payload)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				return
			}
			if err := conn.WriteFrame(wsOpcodeText, payload); err != nil {
				return
			}
		}
	}
}

const (
	wsOpcodeText   = 1
	wsOpcodeBinary = 2
	wsOpcodeClose  = 8
	wsOpcodePing   = 9
	wsOpcodePong   = 10
)

type websocketConn struct {
	rw      *bufio.ReadWriter
	c       io.Closer
	writeMu sync.Mutex
}

func upgradeWebSocket(w stdhttp.ResponseWriter, r *stdhttp.Request) (*websocketConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing websocket upgrade header")
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("missing connection upgrade header")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	hijacker, ok := w.(stdhttp.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
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
	return &websocketConn{rw: rw, c: conn}, nil
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (c *websocketConn) Close() error {
	return c.c.Close()
}

func (c *websocketConn) ReadFrame() (int, []byte, error) {
	first, err := c.rw.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if first&0x70 != 0 {
		return 0, nil, errors.New("websocket RSV bits are not supported")
	}
	second, err := c.rw.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode := int(first & 0x0f)
	masked := second&0x80 != 0
	if !masked {
		return 0, nil, errors.New("client websocket frames must be masked")
	}
	length := uint64(second & 0x7f)
	switch length {
	case 126:
		b1, err := c.rw.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		b2, err := c.rw.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		length = uint64(b1)<<8 | uint64(b2)
	case 127:
		var n uint64
		for range 8 {
			b, err := c.rw.ReadByte()
			if err != nil {
				return 0, nil, err
			}
			n = n<<8 | uint64(b)
		}
		length = n
	}
	if length > maxWSFrameSize {
		return 0, nil, errors.New("websocket frame is too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.rw, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func (c *websocketConn) WriteFrame(opcode int, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(payload) > maxWSFrameSize {
		return errors.New("websocket frame is too large")
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
			_ = c.WriteFrame(wsOpcodeClose, nil)
			return
		}
	}
}
