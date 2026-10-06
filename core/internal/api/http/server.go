package http

import (
	"errors"
	stdhttp "net/http"
	"strings"

	"knot-core/internal/api/response"
	"knot-core/internal/auth"
	coreruntime "knot-core/internal/runtime"
	"knot-core/pkg/core"
)

type Server struct {
	mux     *stdhttp.ServeMux
	core    *core.Service
	runtime coreruntime.Info
	token   auth.Verifier
	origins auth.OriginChecker
}

func NewServer(coreService *core.Service, runtimeInfo coreruntime.Info, token auth.Verifier, origins auth.OriginChecker) *Server {
	s := &Server{
		mux:     stdhttp.NewServeMux(),
		core:    coreService,
		runtime: runtimeInfo,
		token:   token,
		origins: origins,
	}
	s.routes()
	return s
}

func (s *Server) Handler() stdhttp.Handler {
	return s.cors(s.authenticate(s.mux))
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/version", s.getVersion)
	s.mux.HandleFunc("/v1/health", s.getHealth)
	s.mux.HandleFunc("/v1/capabilities", s.getCapabilities)
	s.mux.HandleFunc("/v1/runtime", s.getRuntime)
	s.mux.HandleFunc("/v1/status", s.getStatus)
	s.mux.HandleFunc("/v1/events", s.eventsWS)
	s.mux.HandleFunc("/v1/connections/clear", s.clearConnections)
	s.mux.HandleFunc("/v1/shutdown", s.shutdown)
	s.mux.HandleFunc("/v1/config", s.handleConfig)
	s.mux.HandleFunc("/v1/config/", s.handleConfig)
	s.mux.HandleFunc("/v1/secrets", s.handleSecrets)
	s.mux.HandleFunc("/v1/secrets/", s.handleSecrets)
	s.mux.HandleFunc("/v1/sessions", s.handleSessions)
	s.mux.HandleFunc("/v1/sessions/", s.handleSessions)
	s.mux.HandleFunc("/v1/sftp", s.handleSFTP)
	s.mux.HandleFunc("/v1/sftp/", s.handleSFTP)
	s.mux.HandleFunc("/v1/", s.notFound)
	s.mux.HandleFunc("/", s.notFound)
}

func (s *Server) authenticate(next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if !s.origins.Allowed(r.Header.Get("Origin")) {
			response.JSONError(w, stdhttp.StatusForbidden, response.Error{
				Code:            "PERMISSION_DENIED",
				Message:         "request origin is not allowed",
				Retryable:       false,
				Risk:            response.RiskReadOnly,
				Resource:        "core",
				SuggestedAction: "register the browser origin before using the local API",
			})
			return
		}
		if err := s.token.VerifyRequest(r); err != nil {
			status := stdhttp.StatusUnauthorized
			code := "AUTH_FAILED"
			message := "token authentication failed"
			action := "send a valid bearer token"
			if errors.Is(err, auth.ErrMissingToken) {
				code = "AUTH_REQUIRED"
				message = "token authentication is required"
				action = "send Authorization: Bearer <token>"
			}
			response.JSONError(w, status, response.Error{
				Code:            code,
				Message:         message,
				Retryable:       false,
				Risk:            response.RiskReadOnly,
				Resource:        "core",
				SuggestedAction: action,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.origins.Allowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", appendVary(w.Header().Get("Vary"), "Origin"))
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Knot-Token")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		}
		if r.Method == stdhttp.MethodOptions {
			if !s.origins.Allowed(origin) {
				response.JSONError(w, stdhttp.StatusForbidden, response.Error{
					Code:            "PERMISSION_DENIED",
					Message:         "request origin is not allowed",
					Retryable:       false,
					Risk:            response.RiskReadOnly,
					Resource:        "core",
					SuggestedAction: "register the browser origin before using the local API",
				})
				return
			}
			w.WriteHeader(stdhttp.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) getVersion(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodGet) {
		return
	}
	response.JSON(w, stdhttp.StatusOK, s.core.VersionInfo())
}

func (s *Server) getHealth(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodGet) {
		return
	}
	response.JSON(w, stdhttp.StatusOK, s.core.Health(true, core.HealthInput{
		TokenAvailable:  s.token.Available(),
		RuntimePath:     s.runtime.RuntimePath,
		ListenAddresses: s.runtime.ListenAddresses,
	}))
}

func (s *Server) getCapabilities(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodGet) {
		return
	}
	response.JSON(w, stdhttp.StatusOK, s.core.Capabilities())
}

func (s *Server) getRuntime(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodGet) {
		return
	}
	response.JSON(w, stdhttp.StatusOK, s.runtime)
}

func (s *Server) getStatus(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !requireMethod(w, r, stdhttp.MethodGet) {
		return
	}
	response.JSON(w, stdhttp.StatusOK, s.core.Status())
}

func (s *Server) notFound(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	response.JSONError(w, stdhttp.StatusNotFound, response.Error{
		Code:            "NOT_FOUND",
		Message:         "resource was not found",
		Retryable:       false,
		Risk:            response.RiskReadOnly,
		Resource:        r.URL.Path,
		SuggestedAction: "check the v1 resource path",
	})
}

func requireMethod(w stdhttp.ResponseWriter, r *stdhttp.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	response.JSONError(w, stdhttp.StatusMethodNotAllowed, response.Error{
		Code:            "METHOD_NOT_ALLOWED",
		Message:         "HTTP method is not allowed for this resource",
		Retryable:       false,
		Risk:            response.RiskReadOnly,
		Resource:        r.URL.Path,
		SuggestedAction: "use " + method,
	})
	return false
}

func appendVary(current, value string) string {
	if current == "" {
		return value
	}
	for _, part := range strings.Split(current, ",") {
		if strings.EqualFold(strings.TrimSpace(part), value) {
			return current
		}
	}
	return current + ", " + value
}
