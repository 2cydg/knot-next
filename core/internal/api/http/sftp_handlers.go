package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"os"
	"strconv"
	"strings"

	"knot-core/internal/api/response"
	"knot-core/pkg/sftp"
)

func (s *Server) handleSFTP(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	sftpService := s.core.SFTP()
	if sftpService == nil {
		writeAPIError(w, stdhttp.StatusServiceUnavailable, response.RiskReadOnly, "sftp", "CAPABILITY_UNAVAILABLE", "sftp service is not available")
		return
	}
	parts := splitPath(r.URL.Path[len("/v1/sftp"):])
	switch {
	case len(parts) == 0:
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body sftp.CreateRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.Create(body)
		writeSFTPResult(w, stdhttp.StatusCreated, response.RiskLongRunning, "sftp", data, err)
	case len(parts) == 1:
		id := parts[0]
		switch r.Method {
		case stdhttp.MethodGet:
			data, err := sftpService.Get(id)
			writeSFTPResult(w, stdhttp.StatusOK, response.RiskReadOnly, "sftp/"+id, data, err)
		case stdhttp.MethodDelete:
			data, err := sftpService.Close(id)
			writeSFTPResult(w, stdhttp.StatusOK, response.RiskLongRunning, "sftp/"+id, data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	case len(parts) == 2 && parts[1] == "events":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		handleSFTPSessionEvents(w, r, sftpService, parts[0])
	case len(parts) == 2 && parts[1] == "files":
		handleSFTPFiles(w, r, sftpService, parts[0])
	case len(parts) == 2 && parts[1] == "dirs":
		handleSFTPDirs(w, r, sftpService, parts[0])
	case len(parts) == 2 && parts[1] == "rename":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.Rename(parts[0], body.OldPath, body.NewPath)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskRemoteMutation, "sftp/"+parts[0]+"/rename", data, err)
	case len(parts) == 2 && parts[1] == "upload":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body sftp.TransferRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.Upload(parts[0], body)
		writeSFTPResult(w, stdhttp.StatusAccepted, response.RiskRemoteMutation, "sftp/"+parts[0]+"/upload", data, err)
	case len(parts) == 2 && parts[1] == "download":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body sftp.TransferRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.Download(parts[0], body)
		writeSFTPResult(w, stdhttp.StatusAccepted, response.RiskRemoteRead, "sftp/"+parts[0]+"/download", data, err)
	case len(parts) == 2 && parts[1] == "batch-upload":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body sftp.BatchTransferRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.BatchUpload(parts[0], body)
		writeSFTPResult(w, stdhttp.StatusAccepted, response.RiskRemoteMutation, "sftp/"+parts[0]+"/batch-upload", data, err)
	case len(parts) == 2 && parts[1] == "batch-download":
		if !requireMethod(w, r, stdhttp.MethodPost) {
			return
		}
		var body sftp.BatchTransferRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.BatchDownload(parts[0], body)
		writeSFTPResult(w, stdhttp.StatusAccepted, response.RiskRemoteRead, "sftp/"+parts[0]+"/batch-download", data, err)
	case len(parts) == 2 && parts[1] == "matches":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := sftpService.GlobMatches(parts[0], r.URL.Query().Get("pattern"), r.URL.Query().Get("include_dirs") == "true", queryBoolDefault(r, "cache", true))
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskRemoteRead, "sftp/"+parts[0]+"/matches", map[string]any{"entries": data}, err)
	case len(parts) == 3 && parts[1] == "challenges" && parts[2] == "host-key":
		handleSFTPHostKeyChallenge(w, r, sftpService, parts[0])
	case len(parts) == 3 && parts[1] == "challenges" && parts[2] == "auth":
		handleSFTPAuthChallenge(w, r, sftpService, parts[0])
	case len(parts) == 3 && parts[1] == "transfers" && parts[2] == "events":
		handleSFTPTransferEvents(w, r, sftpService, parts[0])
	case len(parts) == 2 && parts[1] == "transfers":
		if !requireMethod(w, r, stdhttp.MethodGet) {
			return
		}
		data, err := sftpService.ListTransfers(parts[0])
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskReadOnly, "sftp/"+parts[0]+"/transfers", data, err)
	case len(parts) == 3 && parts[1] == "transfers":
		switch r.Method {
		case stdhttp.MethodDelete:
			data, err := sftpService.CancelTransfer(parts[0], parts[2])
			writeSFTPResult(w, stdhttp.StatusOK, response.RiskLongRunning, "sftp/"+parts[0]+"/transfers/"+parts[2], data, err)
		case stdhttp.MethodGet:
			data, err := sftpService.Transfer(parts[0], parts[2])
			writeSFTPResult(w, stdhttp.StatusOK, response.RiskReadOnly, "sftp/"+parts[0]+"/transfers/"+parts[2], data, err)
		default:
			requireMethod(w, r, stdhttp.MethodGet)
		}
	default:
		s.notFound(w, r)
	}
}

func handleSFTPFiles(w stdhttp.ResponseWriter, r *stdhttp.Request, sftpService *sftp.Service, id string) {
	remotePath := r.URL.Query().Get("path")
	switch r.Method {
	case stdhttp.MethodGet:
		if queryBoolDefault(r, "stat", false) || r.URL.Query().Get("op") == "stat" {
			data, err := sftpService.Stat(id, remotePath)
			writeSFTPResult(w, stdhttp.StatusOK, response.RiskRemoteRead, "sftp/"+id+"/files", data, err)
			return
		}
		data, err := sftpService.List(id, remotePath, sftp.ListOptions{
			ShowHidden: queryBoolDefault(r, "show_hidden", false),
			Sort:       r.URL.Query().Get("sort"),
			Limit:      queryIntDefault(r, "limit", 0),
			Offset:     queryIntDefault(r, "offset", 0),
			Cache:      queryBoolDefault(r, "cache", true),
		})
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskRemoteRead, "sftp/"+id+"/files", map[string]any{"entries": data}, err)
	case stdhttp.MethodPost:
		var body struct {
			Path string `json:"path"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.Stat(id, body.Path)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskRemoteRead, "sftp/"+id+"/files", data, err)
	case stdhttp.MethodHead:
		data, err := sftpService.Stat(id, remotePath)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskRemoteRead, "sftp/"+id+"/files", data, err)
	case stdhttp.MethodDelete:
		err := sftpService.RemoveFile(id, remotePath)
		writeSFTPDeleted(w, response.RiskRemoteMutation, "sftp/"+id+"/files", err)
	default:
		requireMethod(w, r, stdhttp.MethodGet)
	}
}

func handleSFTPDirs(w stdhttp.ResponseWriter, r *stdhttp.Request, sftpService *sftp.Service, id string) {
	remotePath := r.URL.Query().Get("path")
	switch r.Method {
	case stdhttp.MethodPost:
		var body struct {
			Path      string `json:"path"`
			Recursive bool   `json:"recursive"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.Mkdir(id, body.Path, body.Recursive)
		writeSFTPResult(w, stdhttp.StatusCreated, response.RiskRemoteMutation, "sftp/"+id+"/dirs", data, err)
	case stdhttp.MethodDelete:
		err := sftpService.RemoveDir(id, remotePath)
		writeSFTPDeleted(w, response.RiskRemoteMutation, "sftp/"+id+"/dirs", err)
	default:
		requireMethod(w, r, stdhttp.MethodPost)
	}
}

func handleSFTPSessionEvents(w stdhttp.ResponseWriter, r *stdhttp.Request, sftpService *sftp.Service, sessionID string) {
	events, cancel, data, err := sftpService.Subscribe(sessionID)
	if err != nil {
		writeSFTPError(w, response.RiskReadOnly, "sftp/"+sessionID+"/events", err)
		return
	}
	defer cancel()
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, "sftp/"+sessionID+"/events", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go conn.drainControlFrames(cancel)
	payload, _ := json.Marshal(map[string]any{
		"type":    "sftp.session.snapshot",
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

func handleSFTPTransferEvents(w stdhttp.ResponseWriter, r *stdhttp.Request, sftpService *sftp.Service, sessionID string) {
	events, cancel, err := sftpService.SubscribeTransfers(sessionID)
	if err != nil {
		writeSFTPError(w, response.RiskReadOnly, "sftp/"+sessionID+"/transfers/events", err)
		return
	}
	defer cancel()
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		writeAPIError(w, stdhttp.StatusBadRequest, response.RiskReadOnly, "sftp/"+sessionID+"/transfers/events", "WEBSOCKET_UPGRADE_FAILED", err.Error())
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go conn.drainControlFrames(cancel)
	_ = conn.WriteFrame(wsOpcodeText, []byte(fmt.Sprintf(`{"type":"sftp.transfer.snapshot","session_id":%q}`, sessionID)))
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

func handleSFTPHostKeyChallenge(w stdhttp.ResponseWriter, r *stdhttp.Request, sftpService *sftp.Service, id string) {
	switch r.Method {
	case stdhttp.MethodGet:
		data, err := sftpService.HostKeyChallenge(id)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskReadOnly, "sftp/"+id+"/challenges/host-key", data, err)
	case stdhttp.MethodPost:
		var body sftp.ChallengeResponse
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.RespondHostKeyChallenge(id, body)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskLongRunning, "sftp/"+id+"/challenges/host-key", data, err)
	default:
		requireMethod(w, r, stdhttp.MethodGet)
	}
}

func handleSFTPAuthChallenge(w stdhttp.ResponseWriter, r *stdhttp.Request, sftpService *sftp.Service, id string) {
	switch r.Method {
	case stdhttp.MethodGet:
		data, err := sftpService.AuthChallenge(id)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskReadOnly, "sftp/"+id+"/challenges/auth", data, err)
	case stdhttp.MethodPost:
		var body sftp.ChallengeResponse
		if !decodeJSON(w, r, &body) {
			return
		}
		data, err := sftpService.RespondAuthChallenge(id, body)
		writeSFTPResult(w, stdhttp.StatusOK, response.RiskLongRunning, "sftp/"+id+"/challenges/auth", data, err)
	default:
		requireMethod(w, r, stdhttp.MethodGet)
	}
}

func queryIntDefault(r *stdhttp.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func queryBoolDefault(r *stdhttp.Request, key string, fallback bool) bool {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func writeSFTPResult(w stdhttp.ResponseWriter, status int, risk response.Risk, resource string, data any, err error) {
	if err != nil {
		writeSFTPError(w, risk, resource, err)
		return
	}
	response.JSON(w, status, data)
}

func writeSFTPDeleted(w stdhttp.ResponseWriter, risk response.Risk, resource string, err error) {
	if err != nil {
		writeSFTPError(w, risk, resource, err)
		return
	}
	response.JSON(w, stdhttp.StatusOK, map[string]bool{"deleted": true})
}

func writeSFTPError(w stdhttp.ResponseWriter, risk response.Risk, resource string, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeAPIError(w, stdhttp.StatusNotFound, risk, resource, "NOT_FOUND", "remote path was not found")
	case isSFTPPermissionError(err):
		writeAPIError(w, stdhttp.StatusForbidden, risk, resource, "PERMISSION_DENIED", err.Error())
	case errors.Is(err, sftp.ErrNotFound):
		writeAPIError(w, stdhttp.StatusNotFound, risk, resource, "NOT_FOUND", "sftp resource was not found")
	case errors.Is(err, sftp.ErrConflict):
		writeAPIError(w, stdhttp.StatusConflict, risk, resource, "CONFLICT", err.Error())
	case errors.Is(err, sftp.ErrValidation):
		writeAPIError(w, stdhttp.StatusBadRequest, risk, resource, "VALIDATION_FAILED", err.Error())
	default:
		writeAPIError(w, stdhttp.StatusInternalServerError, risk, resource, "INTERNAL_ERROR", err.Error())
	}
}

func isSFTPPermissionError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "access is denied")
}
