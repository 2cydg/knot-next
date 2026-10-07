package http

import (
	"context"
	"encoding/json"
	stdhttp "net/http"

	"knot-core/internal/api/response"
)

func (s *Server) eventsWS(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if r.Method != stdhttp.MethodGet {
		requireMethod(w, r, stdhttp.MethodGet)
		return
	}
	events, cancel := s.core.SubscribeEvents()
	defer cancel()
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, "events", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go conn.drainControlFrames(cancel)
	go pingAttach(ctx, conn, func(opcode int, payload []byte) bool {
		return conn.WriteFrame(opcode, payload) == nil
	})
	_ = conn.WriteFrame(wsOpcodeText, []byte(`{"type":"core.snapshot","api_version":"v1"}`))
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

func (s *Server) clearConnections(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodPost) {
		return
	}
	response.JSON(w, stdhttp.StatusOK, map[string]int{"closed": s.core.ClearConnections()})
}

func (s *Server) shutdown(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodPost) {
		return
	}
	// Respond before triggering: the client gets its acknowledgement, and the
	// runner then owns the single teardown flow that every trigger shares.
	response.JSON(w, stdhttp.StatusOK, map[string]bool{"shutdown": true})
	s.core.RequestShutdown()
}
