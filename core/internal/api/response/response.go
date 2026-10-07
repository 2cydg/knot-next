package response

import (
	"encoding/json"
	"net/http"
	"time"

	"knot-core/internal/logger"
)

type Risk string

const (
	RiskReadOnly       Risk = "READ_ONLY"
	RiskLocalMutation  Risk = "LOCAL_MUTATION"
	RiskRemoteRead     Risk = "REMOTE_READ"
	RiskRemoteMutation Risk = "REMOTE_MUTATION"
	RiskLongRunning    Risk = "LONG_RUNNING"
	RiskDestructive    Risk = "DESTRUCTIVE"
)

type Meta struct {
	Timestamp time.Time `json:"timestamp"`
}

type Envelope struct {
	Data any  `json:"data,omitempty"`
	Meta Meta `json:"meta"`
}

type Error struct {
	Code            string         `json:"code"`
	Message         string         `json:"message"`
	Details         map[string]any `json:"details,omitempty"`
	Retryable       bool           `json:"retryable"`
	Risk            Risk           `json:"risk"`
	Resource        string         `json:"resource,omitempty"`
	SuggestedAction string         `json:"suggested_action,omitempty"`
}

type ErrorEnvelope struct {
	Error Error `json:"error"`
	Meta  Meta  `json:"meta"`
}

func JSON(w http.ResponseWriter, status int, data any) {
	write(w, status, Envelope{
		Data: data,
		Meta: Meta{Timestamp: time.Now().UTC()},
	})
}

func JSONError(w http.ResponseWriter, status int, err Error) {
	err.Message = logger.Redact(err.Message)
	err.SuggestedAction = logger.Redact(err.SuggestedAction)
	if err.Details != nil {
		details, ok := logger.DefaultRedactor().Value("details", err.Details).(map[string]any)
		if !ok {
			details = map[string]any{"reason": "details_unavailable"}
		}
		err.Details = details
	}
	if err.Risk == "" {
		err.Risk = RiskReadOnly
	}
	write(w, status, ErrorEnvelope{
		Error: err,
		Meta:  Meta{Timestamp: time.Now().UTC()},
	})
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
