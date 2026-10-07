package http

import (
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"strconv"
	"strings"

	"knot-core/internal/api/response"
	"knot-core/internal/keyutil"
	"knot-core/pkg/config"
)

const maxJSONBody = 1 << 20

func (s *Server) handleConfig(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	configService := s.core.Config()
	if configService == nil {
		writeAPIError(w, stdhttp.StatusServiceUnavailable, response.RiskReadOnly, "config", "CAPABILITY_UNAVAILABLE", "config service is not available")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/config")
	path = strings.Trim(path, "/")
	parts := splitPath(path)
	switch {
	case len(parts) == 0:
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := configService.Summary()
		writeResult(w, response.RiskReadOnly, "config", data, err)
	case len(parts) == 1 && parts[0] == "validate":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var cfg config.Config
		if !decodeJSON(w, r, &cfg) {
			return
		}
		response.JSON(w, stdhttp.StatusOK, configService.ValidateConfig(cfg))
	case len(parts) == 1 && parts[0] == "metadata":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := configService.Metadata()
		writeResult(w, response.RiskReadOnly, "config/metadata", data, err)
	case len(parts) == 1 && parts[0] == "migration":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		response.JSON(w, stdhttp.StatusOK, configService.Migration())
	case len(parts) == 2 && parts[0] == "migration" && parts[1] == "plan":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		if path := r.URL.Query().Get("source_path"); path != "" {
			data, err := configService.PlanImport(config.MigrationApplyRequest{SourcePath: path, Mode: r.URL.Query().Get("mode")})
			writeResult(w, response.RiskReadOnly, "config/migration/plan", data, err)
		} else {
			response.JSON(w, stdhttp.StatusOK, configService.MigrationPlan())
		}
	case len(parts) == 2 && parts[0] == "migration" && parts[1] == "apply":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body config.MigrationApplyRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		var data config.Summary
		var err error
		if body.SourcePath != "" {
			data, err = configService.ApplyImport(body)
		} else {
			data, err = configService.ApplyMigration(body.Mode)
		}
		writeResult(w, response.RiskLocalMutation, "config/migration/apply", data, err)
	case len(parts) >= 1 && parts[0] == "settings":
		s.handleSettings(w, r, configService, parts[1:])
	case len(parts) >= 1 && parts[0] == "servers":
		s.handleServers(w, r, configService, parts[1:])
	case len(parts) >= 1 && parts[0] == "proxies":
		s.handleProxies(w, r, configService, parts[1:])
	case len(parts) >= 1 && parts[0] == "keys":
		s.handleKeys(w, r, configService, parts[1:])
	case len(parts) >= 1 && parts[0] == "sync-providers":
		s.handleSyncProviders(w, r, configService, parts[1:])
	default:
		s.notFound(w, r)
	}
}

func (s *Server) handleSettings(w stdhttp.ResponseWriter, r *stdhttp.Request, configService *config.Service, parts []string) {
	switch {
	case len(parts) == 0:
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := configService.Settings()
		writeResult(w, response.RiskReadOnly, "config/settings", data, err)
	case len(parts) == 1:
		key := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.Setting(key)
			writeResult(w, response.RiskReadOnly, "config/settings/"+key, data, err)
		case stdhttp.MethodPatch:
			var body struct {
				Value any `json:"value"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.UpdateSetting(key, body.Value)
			writeResult(w, response.RiskLocalMutation, "config/settings/"+key, data, err)
		case stdhttp.MethodDelete:
			data, err := configService.ResetSetting(key)
			writeResult(w, response.RiskLocalMutation, "config/settings/"+key, data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	default:
		s.notFound(w, r)
	}
}

func (s *Server) handleServers(w stdhttp.ResponseWriter, r *stdhttp.Request, configService *config.Service, parts []string) {
	if len(parts) == 1 && parts[0] == "resolve" {
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := configService.ResolveServer(r.URL.Query().Get("ref"))
		writeResult(w, response.RiskReadOnly, "config/servers/resolve", data, err)
		return
	}
	switch {
	case len(parts) == 0:
		switch r.Method {
		case stdhttp.MethodGet:
			if wantsPagedServers(r) {
				data, err := configService.ListServersPage(serverListOptionsFromRequest(r))
				writeResult(w, response.RiskReadOnly, "config/servers", data, err)
			} else {
				data, err := configService.ListServers()
				writeResult(w, response.RiskReadOnly, "config/servers", data, err)
			}
		case stdhttp.MethodPost:
			var body config.ServerProfile
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.CreateServer(body)
			writeCreatedResult(w, response.RiskLocalMutation, "config/servers", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 1:
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.GetServer(id)
			writeResult(w, response.RiskReadOnly, "config/servers/"+id, data, err)
		case stdhttp.MethodPut:
			var body config.ServerProfile
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.UpdateServer(id, body)
			writeResult(w, response.RiskLocalMutation, "config/servers/"+id, data, err)
		case stdhttp.MethodDelete:
			err := configService.DeleteServer(id)
			writeDeletedResult(w, response.RiskLocalMutation, "config/servers/"+id, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	default:
		s.notFound(w, r)
	}
}

func (s *Server) handleProxies(w stdhttp.ResponseWriter, r *stdhttp.Request, configService *config.Service, parts []string) {
	switch {
	case len(parts) == 0:
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.ListProxies()
			writeResult(w, response.RiskReadOnly, "config/proxies", data, err)
		case stdhttp.MethodPost:
			var body config.ProxyProfile
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.CreateProxy(body)
			writeCreatedResult(w, response.RiskLocalMutation, "config/proxies", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 1:
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.GetProxy(id)
			writeResult(w, response.RiskReadOnly, "config/proxies/"+id, data, err)
		case stdhttp.MethodPut:
			var body config.ProxyProfile
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.UpdateProxy(id, body)
			writeResult(w, response.RiskLocalMutation, "config/proxies/"+id, data, err)
		case stdhttp.MethodDelete:
			err := configService.DeleteProxy(id)
			writeDeletedResult(w, response.RiskLocalMutation, "config/proxies/"+id, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	default:
		s.notFound(w, r)
	}
}

func (s *Server) handleKeys(w stdhttp.ResponseWriter, r *stdhttp.Request, configService *config.Service, parts []string) {
	switch {
	case len(parts) == 0:
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.ListKeys()
			writeResult(w, response.RiskReadOnly, "config/keys", data, err)
		case stdhttp.MethodPost:
			var body config.KeyMetadata
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.CreateKey(body)
			writeCreatedResult(w, response.RiskLocalMutation, "config/keys", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 1:
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.GetKey(id)
			writeResult(w, response.RiskReadOnly, "config/keys/"+id, data, err)
		case stdhttp.MethodPut:
			var body config.KeyMetadata
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.UpdateKey(id, body)
			writeResult(w, response.RiskLocalMutation, "config/keys/"+id, data, err)
		case stdhttp.MethodDelete:
			err := configService.DeleteKey(id)
			writeDeletedResult(w, response.RiskLocalMutation, "config/keys/"+id, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	default:
		s.notFound(w, r)
	}
}

func (s *Server) handleSyncProviders(w stdhttp.ResponseWriter, r *stdhttp.Request, configService *config.Service, parts []string) {
	switch {
	case len(parts) == 0:
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.ListSyncProviders()
			writeResult(w, response.RiskReadOnly, "config/sync-providers", data, err)
		case stdhttp.MethodPost:
			var body config.SyncProviderConfig
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.CreateSyncProvider(body)
			writeCreatedResult(w, response.RiskLocalMutation, "config/sync-providers", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 1:
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := configService.GetSyncProvider(id)
			writeResult(w, response.RiskReadOnly, "config/sync-providers/"+id, data, err)
		case stdhttp.MethodPut:
			var body config.SyncProviderConfig
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := configService.UpdateSyncProvider(id, body)
			writeResult(w, response.RiskLocalMutation, "config/sync-providers/"+id, data, err)
		case stdhttp.MethodDelete:
			err := configService.DeleteSyncProvider(id)
			writeDeletedResult(w, response.RiskLocalMutation, "config/sync-providers/"+id, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 2 && parts[1] == "default":
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodPut:
			data, err := configService.SetDefaultSyncProvider(id)
			writeResult(w, response.RiskLocalMutation, "config/sync-providers/"+id+"/default", data, err)
		case stdhttp.MethodDelete:
			err := configService.ClearDefaultSyncProvider()
			writeDeletedResult(w, response.RiskLocalMutation, "config/sync-providers/"+id+"/default", err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	default:
		s.notFound(w, r)
	}
}

func (s *Server) handleSecrets(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	secretService := s.core.Secret()
	if secretService == nil {
		writeAPIError(w, stdhttp.StatusServiceUnavailable, response.RiskReadOnly, "secrets", "CAPABILITY_UNAVAILABLE", "secret service is not available")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/secrets")
	parts := splitPath(path)
	switch {
	case len(parts) == 0:
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := secretService.Summary()
		writeResult(w, response.RiskReadOnly, "secrets", data, err)
	case len(parts) == 1 && parts[0] == "crypto":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		response.JSON(w, stdhttp.StatusOK, secretService.CryptoCapability())
	case len(parts) == 3 && parts[0] == "servers" && parts[2] == "password":
		id := parts[1]
		switch r.Method {
		case stdhttp.MethodPut:
			var body struct {
				Password string `json:"password"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := secretService.SetServerPassword(id, body.Password)
			writeResult(w, response.RiskLocalMutation, "secrets/servers/"+id+"/password", data, err)
		case stdhttp.MethodDelete:
			data, err := secretService.ClearServerPassword(id)
			writeResult(w, response.RiskLocalMutation, "secrets/servers/"+id+"/password", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	case len(parts) == 3 && parts[0] == "keys" && parts[2] == "private":
		id := parts[1]
		switch r.Method {
		case stdhttp.MethodPut:
			var body struct {
				Passphrase string `json:"passphrase,omitempty"`
				PrivateKey string `json:"private_key"`
				SourcePath string `json:"source_path"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := secretService.SetKeyPrivateWithPassphrase(id, body.PrivateKey, body.SourcePath, body.Passphrase)
			writeResult(w, response.RiskLocalMutation, "secrets/keys/"+id+"/private", data, err)
		case stdhttp.MethodDelete:
			data, err := secretService.ClearKeyPrivate(id)
			writeResult(w, response.RiskLocalMutation, "secrets/keys/"+id+"/private", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	case len(parts) == 3 && parts[0] == "proxies" && parts[2] == "password":
		id := parts[1]
		switch r.Method {
		case stdhttp.MethodPut:
			var body struct {
				Password string `json:"password"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := secretService.SetProxyPassword(id, body.Password)
			writeResult(w, response.RiskLocalMutation, "secrets/proxies/"+id+"/password", data, err)
		case stdhttp.MethodDelete:
			data, err := secretService.ClearProxyPassword(id)
			writeResult(w, response.RiskLocalMutation, "secrets/proxies/"+id+"/password", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	case len(parts) == 2 && parts[0] == "sync" && parts[1] == "password":
		switch r.Method {
		case stdhttp.MethodPut:
			var body struct {
				Password string `json:"password"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := secretService.SetSyncPassword(body.Password)
			writeResult(w, response.RiskLocalMutation, "secrets/sync/password", data, err)
		case stdhttp.MethodDelete:
			data, err := secretService.ClearSyncPassword()
			writeResult(w, response.RiskLocalMutation, "secrets/sync/password", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	case len(parts) == 3 && parts[0] == "sync-providers" && parts[2] == "password":
		id := parts[1]
		switch r.Method {
		case stdhttp.MethodPut:
			var body struct {
				Password string `json:"password"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := secretService.SetSyncProviderPassword(id, body.Password)
			writeResult(w, response.RiskLocalMutation, "secrets/sync-providers/"+id+"/password", data, err)
		case stdhttp.MethodDelete:
			data, err := secretService.ClearSyncProviderPassword(id)
			writeResult(w, response.RiskLocalMutation, "secrets/sync-providers/"+id+"/password", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	case len(parts) == 3 && parts[0] == "sync-providers" && parts[2] == "s3-credentials":
		id := parts[1]
		switch r.Method {
		case stdhttp.MethodPut:
			var body struct {
				AccessKeyID     string `json:"access_key_id"`
				SecretAccessKey string `json:"secret_access_key"`
				SessionToken    string `json:"session_token"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			data, err := secretService.SetSyncProviderS3Credentials(id, body.AccessKeyID, body.SecretAccessKey, body.SessionToken)
			writeResult(w, response.RiskLocalMutation, "secrets/sync-providers/"+id+"/s3-credentials", data, err)
		case stdhttp.MethodDelete:
			data, err := secretService.ClearSyncProviderS3Credentials(id)
			writeResult(w, response.RiskLocalMutation, "secrets/sync-providers/"+id+"/s3-credentials", data, err)
		default:
			requireMethod(w, r, stdhttp.MethodPut)
		}
	default:
		s.notFound(w, r)
	}
}

func wantsPagedServers(r *stdhttp.Request) bool {
	q := r.URL.Query()
	return q.Get("page") == "true" || q.Get("alias") != "" || q.Get("tag") != "" || q.Get("auth_method") != "" || q.Get("proxy_id") != "" || q.Get("q") != "" || q.Get("limit") != "" || q.Get("offset") != "" || q.Get("sort") != ""
}

func serverListOptionsFromRequest(r *stdhttp.Request) config.ServerListOptions {
	q := r.URL.Query()
	return config.ServerListOptions{
		Alias:      q.Get("alias"),
		Tag:        q["tag"],
		AuthMethod: q.Get("auth_method"),
		ProxyID:    q.Get("proxy_id"),
		Query:      q.Get("q"),
		Limit:      parseIntQuery(q.Get("limit")),
		Offset:     parseIntQuery(q.Get("offset")),
		Sort:       q.Get("sort"),
	}
}

func parseIntQuery(raw string) int {
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func decodeJSON(w stdhttp.ResponseWriter, r *stdhttp.Request, dst any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, r.URL.Path, "INVALID_JSON", "request body must be valid JSON")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, r.URL.Path, "INVALID_JSON", "request body must contain a single JSON object")
		return false
	}
	return true
}

func writeResult(w stdhttp.ResponseWriter, risk response.Risk, resource string, data any, err error) {
	if err != nil {
		writeMappedError(w, risk, resource, err)
		return
	}
	response.JSON(w, stdhttp.StatusOK, data)
}

func writeCreatedResult(w stdhttp.ResponseWriter, risk response.Risk, resource string, data any, err error) {
	if err != nil {
		writeMappedError(w, risk, resource, err)
		return
	}
	response.JSON(w, stdhttp.StatusCreated, data)
}

func writeDeletedResult(w stdhttp.ResponseWriter, risk response.Risk, resource string, err error) {
	if err != nil {
		writeMappedError(w, risk, resource, err)
		return
	}
	response.JSON(w, stdhttp.StatusOK, map[string]bool{"deleted": true})
}

func writeMappedError(w stdhttp.ResponseWriter, risk response.Risk, resource string, err error) {
	switch {
	case errors.Is(err, config.ErrNotFound):
		writeAPIError(w, stdhttp.StatusNotFound, risk, resource, "NOT_FOUND", "resource was not found")
	case errors.Is(err, config.ErrConflict):
		writeAPIError(w, stdhttp.StatusConflict, risk, resource, "CONFLICT", "resource conflicts with an existing configuration item")
	case errors.Is(err, keyutil.ErrPassphraseRequired):
		writeAPIError(w, stdhttp.StatusBadRequest, risk, resource, "PASSPHRASE_REQUIRED", "private key passphrase required")
	case errors.Is(err, config.ErrValidation):
		writeAPIError(w, stdhttp.StatusBadRequest, risk, resource, "VALIDATION_FAILED", err.Error())
	default:
		writeAPIError(w, stdhttp.StatusInternalServerError, risk, resource, "INTERNAL_ERROR", "configuration operation failed")
	}
}

func writeAPIError(w stdhttp.ResponseWriter, status int, risk response.Risk, resource string, code string, message string) {
	response.JSONError(w, status, response.Error{
		Code:      code,
		Message:   message,
		Retryable: false,
		Risk:      risk,
		Resource:  resource,
	})
}
