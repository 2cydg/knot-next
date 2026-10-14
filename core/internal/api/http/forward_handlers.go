package http

import (
	"context"
	"errors"
	"knot-core/internal/api/response"
	"knot-core/pkg/forward"
	"knot-core/pkg/proxytest"
	stdhttp "net/http"
	"strings"
	"time"
)

func (s *Server) handleProxyTests(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodPost) {
		return
	}
	service := s.core.ProxyTest()
	if service == nil {
		writeAPIError(w, 503, response.RiskRemoteRead, "proxy-tests", "CAPABILITY_UNAVAILABLE", "proxy test service unavailable")
		return
	}
	var body proxytest.Request
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := service.Test(r.Context(), body)
	if err != nil {
		switch {
		case errors.Is(err, proxytest.ErrValidation):
			writeAPIError(w, 400, response.RiskRemoteRead, "proxy-tests", "VALIDATION_FAILED", err.Error())
		case errors.Is(err, proxytest.ErrStopped):
			writeAPIError(w, 503, response.RiskRemoteRead, "proxy-tests", "CAPABILITY_UNAVAILABLE", err.Error())
		case errors.Is(err, proxytest.ErrLimit):
			writeAPIError(w, 409, response.RiskRemoteRead, "proxy-tests", "RESOURCE_LIMIT", err.Error())
		default:
			writeMappedError(w, response.RiskRemoteRead, "proxy-tests", err)
		}
		return
	}
	response.JSON(w, 200, result)
}
func (s *Server) handleForwards(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	service := s.core.Forward()
	if service == nil {
		writeAPIError(w, 503, response.RiskLongRunning, "forwards", "CAPABILITY_UNAVAILABLE", "forward service unavailable")
		return
	}
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/forwards"))
	switch {
	case len(parts) == 0:
		switch r.Method {
		case stdhttp.MethodGet:
			response.JSON(w, 200, service.List(r.URL.Query().Get("server_id"), r.URL.Query().Get("connection_id")))
		case stdhttp.MethodPost:
			var body forward.Request
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := service.Create(body)
			writeForwardResult(w, 202, data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 1:
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := service.Get(parts[0])
			writeForwardResult(w, 200, data, err)
		case stdhttp.MethodDelete:
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			data, err := service.Delete(ctx, parts[0])
			writeForwardResult(w, 200, data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 2 && parts[1] == "control":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body struct {
			Op string `json:"op"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		var data forward.View
		var err error
		switch body.Op {
		case "start":
			data, err = service.Start(parts[0])
		case "stop":
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			data, err = service.Stop(ctx, parts[0])
		default:
			err = forward.ErrValidation
		}
		writeForwardResult(w, 200, data, err)
	case len(parts) == 2 && parts[1] == "events":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		s.forwardEventsWS(w, r, service, parts[0])
	default:
		s.notFound(w, r)
	}
}
func writeForwardResult(w stdhttp.ResponseWriter, status int, data any, err error) {
	if err == nil {
		response.JSON(w, status, data)
		return
	}
	switch {
	case errors.Is(err, forward.ErrNotFound):
		writeAPIError(w, 404, response.RiskLongRunning, "forwards", "NOT_FOUND", err.Error())
	case errors.Is(err, forward.ErrValidation):
		writeAPIError(w, 400, response.RiskLongRunning, "forwards", "VALIDATION_FAILED", err.Error())
	case errors.Is(err, forward.ErrConflict):
		writeAPIError(w, 409, response.RiskLongRunning, "forwards", "CONFLICT", err.Error())
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		writeAPIError(w, 409, response.RiskLongRunning, "forwards", "FORWARD_STOP_INCOMPLETE", "forward cleanup is still pending; query its resource")
	default:
		writeAPIError(w, 500, response.RiskLongRunning, "forwards", "INTERNAL_ERROR", "forward operation failed")
	}
}
func (s *Server) forwardEventsWS(w stdhttp.ResponseWriter, r *stdhttp.Request, service *forward.Service, id string) {
	events, cancel, snapshot, err := service.Subscribe(id)
	if err != nil {
		writeForwardResult(w, 200, nil, err)
		return
	}
	defer cancel()
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, 400, response.RiskReadOnly, "forwards", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()
	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	closeWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer closeWatch()
	go conn.drainControlFrames(stop)
	go pingAttach(ctx, conn, func(op int, payload []byte) bool { return conn.WriteFrame(op, payload) == nil })
	if err := conn.WriteFrame(wsOpcodeText, mustMarshal(map[string]any{"type": "forward.snapshot", "forward": snapshot})); err != nil {
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
			if err := conn.WriteFrame(wsOpcodeText, mustMarshal(event)); err != nil {
				return
			}
		}
	}
}
