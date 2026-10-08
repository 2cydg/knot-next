package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"strings"
	"sync"
	"time"

	"knot-core/internal/api/response"
	"knot-core/pkg/session"
)

const maxWSFrameSize = 1 << 20

const (
	// attachQueueDepth bounds server->client frames queued but not yet written.
	// A client that cannot keep up is disconnected rather than buffered without
	// limit.
	attachQueueDepth = 64
	// attachDrainTimeout bounds waiting for queued frames to flush on a terminal
	// path. It is a failure bound, not a tail-flush delay.
	attachDrainTimeout = 5 * time.Second
)

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
		data, err := sessionService.ExecContext(r.Context(), body)
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

// pingAttach keeps a WebSocket connection alive. send reports whether the frame
// was accepted; on failure the connection is closed so blocked readers return.
func pingAttach(ctx context.Context, conn io.Closer, send func(opcode int, payload []byte) bool) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !send(wsOpcodePing, nil) {
				_ = conn.Close()
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
	// Claim the attachment before upgrading: a conflict is then a plain HTTP
	// status the client can act on, and a failed upgrade leaves no ownership
	// behind for the next attempt to trip over.
	stream, data, err := sessionService.AttachStream(id)
	if err != nil {
		writeSessionError(w, response.RiskLongRunning, "sessions/"+id+"/attach", err)
		return
	}
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		stream.Cancel()
		stream.Release()
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskLongRunning, "sessions/"+id+"/attach", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()

	events, cancelEvents, _, err := sessionService.Subscribe(id)
	if err != nil {
		stream.Cancel()
		stream.Release()
		if err := conn.WriteFrame(wsOpcodeText, errorFrame("ATTACH_FAILED", err.Error())); err != nil {
			return
		}
		return
	}
	defer cancelEvents()
	defer stream.Cancel()
	defer stream.Release()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	writer := newWSFrameWriter(conn, attachQueueDepth)
	defer writer.close()

	// An attachment ends in exactly one of two ways: a normal completion that
	// flushes the exit message first, or a failure that drops the connection. The
	// lifecycle mutex makes the two mutually exclusive, so a late failure (a
	// relay noticing a full queue while the exit message is being flushed) cannot
	// cut the final message off mid-write.
	// attachAbortFlush bounds delivering an abort reason before the connection is
	// dropped. A client that stopped reading will not receive it; for those cases
	// the connection close is the authoritative signal and the session state is
	// still accurate.
	const attachAbortFlush = time.Second

	var lifecycleMu sync.Mutex
	finishing := false
	terminated := false
	closeNow := func() {
		cancel()
		_ = conn.Close()
	}
	terminate := func(reason string) {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		if finishing || terminated {
			return
		}
		terminated = true
		// Best effort: tell a still-reading client why its attachment ended.
		if writer.enqueue(wsOpcodeText, errorFrame("ATTACH_ABORTED", reason)) {
			writer.close()
			writer.wait(attachAbortFlush)
		}
		closeNow()
	}
	// beginFinish takes over the connection for a normal completion. It reports
	// false when the attachment was already torn down.
	beginFinish := func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		if finishing || terminated {
			return false
		}
		finishing = true
		return true
	}

	enqueueOrTerminate := func(opcode int, payload []byte) bool {
		if writer.enqueue(opcode, payload) {
			return true
		}
		terminate("client is not reading")
		return false
	}

	if !enqueueOrTerminate(wsOpcodeText, mustMarshal(map[string]any{
		"type":    "session.snapshot",
		"session": data,
	})) {
		return
	}
	if !enqueueOrTerminate(wsOpcodeText, mustMarshal(map[string]any{
		"type":       "session.attached",
		"session_id": data.ID,
		"state":      data.State,
	})) {
		return
	}
	// Output produced before this attachment may have been dropped when the
	// retained backlog overflowed; the client is told before it sees the rest.
	if stream.BacklogTruncated {
		if !enqueueOrTerminate(wsOpcodeText, mustMarshal(map[string]any{
			"type":       "session.attach.truncated",
			"session_id": data.ID,
			"detail":     "pre-attach output exceeded the retained backlog",
		})) {
			return
		}
	}

	stdinWrites := make(chan []byte, 32)
	go func() {
		input := stream.Input
		for {
			select {
			case <-ctx.Done():
				return
			case chunk := <-stdinWrites:
				if len(chunk) == 0 {
					continue
				}
				if _, err := input.Write(chunk); err != nil {
					if errors.Is(err, session.ErrAttachmentClosed) {
						// This attachment no longer owns the session; the reason is
						// reported by the revocation or exit path, so end quietly.
						return
					}
					terminate("stdin write failed: " + err.Error())
					return
				}
			}
		}
	}()

	// The attachment can lose ownership under us: an explicit detach, or a newer
	// client taking over. The old connection must stop sending and receiving and
	// end, rather than lingering as a second owner of the same session.
	go func() {
		select {
		case <-ctx.Done():
		case <-stream.Revoked:
			terminate("attachment was detached")
		}
	}()

	// A failure writing to the remote's stdin is terminal for this attachment:
	// reporting it keeps the client from typing into a stream that no longer
	// reaches anything.
	go func() {
		select {
		case <-ctx.Done():
		case err := <-stream.InputError:
			if err != nil {
				terminate("stdin write failed: " + err.Error())
			}
		}
	}()

	// Output relays. A relay ends when its stream ends; if the stream ended
	// because this attachment fell behind, the attachment is terminated with an
	// explicit reason instead of pretending the output was complete.
	var outputs sync.WaitGroup
	outputs.Add(2)
	relayOutput := func(ch <-chan []byte) {
		defer outputs.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-ch:
				if !ok {
					if isClosed(stream.Overflow) {
						terminate("attach output truncated: client fell behind")
					}
					return
				}
				if len(chunk) == 0 {
					continue
				}
				if !enqueueOrTerminate(wsOpcodeBinary, chunk) {
					return
				}
			}
		}
	}
	go relayOutput(stream.Stdout)
	go relayOutput(stream.Stderr)

	go relayAttachEvents(ctx, terminate, writer, events)
	go pingAttach(ctx, conn, writer.enqueue)

	// Exit watcher: deliver trailing output, then the single final exit message,
	// then close. Waiting for the output relays is what guarantees the client
	// sees every received byte before session.exit.
	go func() {
		var outcome session.ExitOutcome
		select {
		case <-ctx.Done():
			return
		case got, ok := <-stream.Exit:
			if !ok {
				return
			}
			outcome = got
		}

		drained := make(chan struct{})
		go func() {
			outputs.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(attachDrainTimeout):
			terminate("output drain timed out")
			return
		case <-ctx.Done():
			return
		}

		// Take over the connection for a normal completion: from here on the exit
		// message is flushed and then the connection is closed.
		if !beginFinish() {
			return
		}
		if !writer.enqueue(wsOpcodeText, mustMarshal(exitPayload(data.ID, outcome))) {
			// The client stopped reading, so the final message cannot be
			// delivered: end the attachment instead of pretending it completed.
			closeNow()
			return
		}
		_ = writer.enqueue(wsOpcodeClose, nil)
		writer.close()
		if !writer.wait(attachDrainTimeout) {
			closeNow()
			return
		}
		// Closing the connection ends the reader loop below so the handler
		// returns and the attachment is released for the next client.
		closeNow()
	}()

	for {
		opcode, payload, err := conn.ReadFrame()
		if err != nil {
			break
		}
		switch opcode {
		case wsOpcodeBinary:
			if len(payload) == 0 {
				break
			}
			chunk := make([]byte, len(payload))
			copy(chunk, payload)
			// Blocking here is deliberate backpressure for stdin: the client is
			// the producer, and output delivery runs on separate goroutines.
			select {
			case stdinWrites <- chunk:
			case <-ctx.Done():
				break
			}
		case wsOpcodeText:
			if err := handleAttachControl(sessionService, id, payload); err != nil {
				if !enqueueOrTerminate(wsOpcodeText, errorFrame("CONTROL_FAILED", err.Error())) {
					return
				}
			}
		case wsOpcodePing:
			if !enqueueOrTerminate(wsOpcodePong, payload) {
				return
			}
		case wsOpcodeClose:
			if !beginFinish() {
				// A remote exit may already own the final frames. Let that
				// writer finish before this handler's deferred TCP close.
				writer.wait(attachDrainTimeout)
				return
			}
			cancel()
			// Stop producers and drain prior frames before echoing Close. Keep
			// the connection open until the echo has actually been written.
			writer.close()
			if writer.wait(attachDrainTimeout) {
				_ = conn.WriteFrame(wsOpcodeClose, payload)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	// The reader loop only exits once the connection is closed or the client sent
	// a close frame; flush whatever is queued so trailing bytes and session.exit
	// are not cut off.
	writer.close()
	writer.wait(attachDrainTimeout)
}

func errorFrame(code, message string) []byte {
	return mustMarshal(map[string]any{"type": "error", "code": code, "message": message})
}

func exitPayload(sessionID string, outcome session.ExitOutcome) map[string]any {
	payload := map[string]any{
		"type":       "session.exit",
		"session_id": sessionID,
	}
	if outcome.Code != nil {
		payload["exit_code"] = *outcome.Code
	} else {
		payload["exit_code"] = nil
	}
	if outcome.FrameworkError != "" {
		payload["error"] = outcome.FrameworkError
	}
	if outcome.DisconnectCause != "" {
		payload["disconnect_cause"] = outcome.DisconnectCause
	}
	return payload
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"type":"error","code":"ENCODE_FAILED"}`)
	}
	return encoded
}

func relayAttachEvents(ctx context.Context, terminate func(string), writer *wsFrameWriter, events <-chan session.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if !writer.enqueue(wsOpcodeText, mustMarshal(event)) {
				terminate("client is not reading")
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
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, "sessions/"+id+"/events", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	go conn.drainControlFrames(cancel)
	payload, _ := json.Marshal(map[string]any{
		"type":    "session.snapshot",
		"session": data,
	})
	if err := conn.WriteFrame(wsOpcodeText, payload); err != nil {
		return
	}
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
