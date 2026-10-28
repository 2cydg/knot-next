package coreclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponse = 4 << 20

// Error retains machine fields for routing; Error deliberately excludes backend
// text and request credentials. UI translations must be selected by Code.
type Error struct {
	Status          int             `json:"-"`
	Code            string          `json:"code"`
	Message         string          `json:"message"`
	Details         json.RawMessage `json:"details,omitempty"`
	Retryable       bool            `json:"retryable"`
	Risk            string          `json:"risk"`
	Resource        string          `json:"resource,omitempty"`
	SuggestedAction string          `json:"suggested_action,omitempty"`
}

func (e *Error) Error() string  { return e.Code }
func Failure(code string) error { return &Error{Code: code} }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TIMEOUT"
	}
	return "OFFLINE"
}

type Client struct {
	base  string
	token string
	http  *http.Client
}

// New rejects remote destinations and redirects; local credentials never pass
// through ambient HTTP proxies or a redirected request.
func New(base, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, Failure("INVALID_RUNTIME")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, Failure("INVALID_RUNTIME")
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, Failure("AUTH_FILE_INVALID")
	}
	transport := &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second,
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{
		// Allow the default 15-second proxy probe and forward stop to finish.
		// UI operations also carry their own bounded contexts.
		Transport: transport, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) SameEndpoint(other *Client) bool {
	return other != nil && c.base == other.base && c.token == other.token
}

func (c *Client) get(ctx context.Context, path string, dest any) error {
	return c.request(ctx, http.MethodGet, path, nil, dest)
}

func (c *Client) request(ctx context.Context, method, path string, value, dest any) error {
	var input []byte
	if value != nil {
		var err error
		input, err = json.Marshal(value)
		if err != nil {
			return Failure("PROTOCOL_ERROR")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(input))
	if err != nil {
		return Failure("PROTOCOL_ERROR")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var n net.Error
		if errors.As(err, &n) && n.Timeout() {
			return Failure("TIMEOUT")
		}
		return Failure("OFFLINE")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return Failure("PROTOCOL_ERROR")
	}
	if len(body) > maxResponse {
		return Failure("RESPONSE_TOO_LARGE")
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *Error          `json:"error"`
	}
	if res.StatusCode == 401 {
		return &Error{Code: "UNAUTHORIZED", Status: 401}
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Failure("PROTOCOL_ERROR")
	}
	if envelope.Error != nil {
		envelope.Error.Status = res.StatusCode
		if envelope.Error.Code == "" {
			envelope.Error.Code = "PROTOCOL_ERROR"
		}
		return envelope.Error
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return Failure("PROTOCOL_ERROR")
	}
	if dest != nil {
		if err := json.Unmarshal(envelope.Data, dest); err != nil {
			return Failure("PROTOCOL_ERROR")
		}
	}
	return nil
}

func (c *Client) Runtime(ctx context.Context) (Runtime, error) {
	var v Runtime
	err := c.get(ctx, "/v1/runtime", &v)
	return v, err
}
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.get(ctx, "/v1/version", &v)
	return v, err
}
func (c *Client) Health(ctx context.Context) (Health, error) {
	var v Health
	err := c.get(ctx, "/v1/health", &v)
	return v, err
}
func (c *Client) Capabilities(ctx context.Context) ([]Capability, error) {
	var v []Capability
	err := c.get(ctx, "/v1/capabilities", &v)
	return v, err
}
func (c *Client) Summary(ctx context.Context) (Summary, error) {
	var v Summary
	err := c.get(ctx, "/v1/config", &v)
	return v, err
}
func (c *Client) Resolve(ctx context.Context, ref string) (Server, error) {
	var v Server
	err := c.get(ctx, "/v1/config/servers/resolve?ref="+url.QueryEscape(ref), &v)
	if err == nil && v.ID == "" {
		err = Failure("PROTOCOL_ERROR")
	}
	return v, err
}
