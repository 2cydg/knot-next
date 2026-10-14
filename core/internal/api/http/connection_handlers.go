package http

import (
	"knot-core/internal/api/response"
	stdhttp "net/http"
	"strings"
)

func (s *Server) handleConnections(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodGet) {
		return
	}
	pool := s.core.SSHPool()
	if pool == nil {
		writeAPIError(w, 503, response.RiskReadOnly, "connections", "CAPABILITY_UNAVAILABLE", "SSH pool is not available")
		return
	}
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/connections"))
	if len(parts) == 0 {
		response.JSON(w, 200, pool.Connections(r.URL.Query().Get("server_id")))
		return
	}
	if len(parts) != 1 {
		s.notFound(w, r)
		return
	}
	data, err := pool.Connection(parts[0])
	if err != nil {
		writeAPIError(w, 404, response.RiskReadOnly, "connections", "NOT_FOUND", "connection was not found")
		return
	}
	response.JSON(w, 200, data)
}
