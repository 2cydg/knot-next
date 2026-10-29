package coreclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

type SessionRequest struct {
	ServerRef      string `json:"server_ref"`
	Term           string `json:"term"`
	Rows           int    `json:"rows"`
	Cols           int    `json:"cols"`
	ForwardAgent   bool   `json:"forward_agent"`
	HostKeyPolicy  string `json:"host_key_policy"`
	AllowAuthRetry bool   `json:"allow_auth_retry"`
}

type Session struct {
	ID              string     `json:"id"`
	ServerID        string     `json:"server_id"`
	ConnectionID    string     `json:"connection_id"`
	Alias           string     `json:"alias"`
	State           string     `json:"state"`
	ExitCode        *int       `json:"exit_code"`
	FrameworkCode   string     `json:"framework_code"`
	FailureCode     string     `json:"failure_code"`
	DisconnectCause string     `json:"disconnect_cause"`
	LastChallenge   *Challenge `json:"last_challenge"`
}

func (s Session) Ready() bool { return s.State == "connected" || s.State == "detached" }
func (s Session) Done() bool  { return s.State == "closed" || s.State == "failed" }

type Challenge struct {
	ID                 string    `json:"challenge_id"`
	SessionID          string    `json:"session_id"`
	Type               string    `json:"type"`
	Pending            bool      `json:"pending"`
	ExpiresAt          time.Time `json:"expires_at"`
	Outcome            string    `json:"outcome"`
	FailureCode        string    `json:"failure_code"`
	Host               string    `json:"host"`
	KeyType            string    `json:"key_type"`
	Fingerprint        string    `json:"fingerprint"`
	Risk               string    `json:"risk"`
	ServerAlias        string    `json:"server_alias"`
	FailedMethod       string    `json:"failed_method"`
	AllowedMethods     []string  `json:"allowed_methods"`
	PassphraseRequired bool      `json:"passphrase_required"`
}

type ChallengeAnswer struct {
	ChallengeID string `json:"challenge_id"`
	Accept      bool   `json:"accept,omitempty"`
	Remember    bool   `json:"remember,omitempty"`
	Password    string `json:"password,omitempty"`
	KeyID       string `json:"key_id,omitempty"`
	Passphrase  string `json:"passphrase,omitempty"`
}

type SessionControl struct {
	Type string `json:"type"`
	Rows int    `json:"rows,omitempty"`
	Cols int    `json:"cols,omitempty"`
}

func sessionPath(id string) string { return "/v1/sessions/" + url.PathEscape(id) }

func (c *Client) CreateSession(ctx context.Context, request SessionRequest) (Session, error) {
	var s Session
	err := c.request(ctx, http.MethodPost, "/v1/sessions", request, &s)
	if err == nil && s.ID == "" {
		err = Failure("PROTOCOL_ERROR")
	}
	return s, err
}
func (c *Client) Session(ctx context.Context, id string) (Session, error) {
	var s Session
	err := c.get(ctx, sessionPath(id), &s)
	if err == nil && s.ID != id {
		err = Failure("PROTOCOL_ERROR")
	}
	return s, err
}
func (c *Client) CloseSession(ctx context.Context, id string) error {
	return c.request(ctx, http.MethodDelete, sessionPath(id), nil, nil)
}
func (c *Client) SessionChallenge(ctx context.Context, id, kind string) (Challenge, error) {
	var challenge Challenge
	err := c.get(ctx, sessionPath(id)+"/challenges/"+kind, &challenge)
	return challenge, err
}
func (c *Client) AnswerSession(ctx context.Context, id, kind string, answer ChallengeAnswer) error {
	return c.request(ctx, http.MethodPost, sessionPath(id)+"/challenges/"+kind, answer, nil)
}
func (c *Client) Attach(ctx context.Context, id string) (*Socket, error) {
	return c.dial(ctx, sessionPath(id)+"/attach")
}
