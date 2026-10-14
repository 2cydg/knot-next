package proxytest

import (
	"context"
	"errors"
	"fmt"
	"knot-core/internal/logger"
	"knot-core/internal/proxyproto"
	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrValidation = errors.New("invalid proxy test")
var ErrLimit = errors.New("proxy test capacity reached")
var ErrStopped = errors.New("proxy test service stopped")

type Draft struct {
	Type     string  `json:"type"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Username string  `json:"username,omitempty"`
	Password *string `json:"password,omitempty"`
}
type Request struct {
	ProxyID   string `json:"proxy_id,omitempty"`
	Draft     *Draft `json:"draft,omitempty"`
	Target    string `json:"target,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}
type Stage struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	DurationMS int64  `json:"duration_ms"`
}
type Result struct {
	Passed      bool    `json:"passed"`
	Scope       string  `json:"scope"`
	Stages      []Stage `json:"stages"`
	FailureCode string  `json:"failure_code,omitempty"`
}
type Provider interface {
	RuntimeConfig() (config.RuntimeConfig, error)
}
type Service struct {
	mu      sync.Mutex
	stopped bool
	workers resourcepolicy.Group
	ctx     context.Context
	cancel  context.CancelFunc
	config  Provider
	slots   chan struct{}
}

func NewService(p Provider) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{config: p, slots: make(chan struct{}, 16), ctx: ctx, cancel: cancel}
}
func (s *Service) Test(ctx context.Context, req Request) (Result, error) {
	logger.RegisterSecrets(req)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return Result{}, ErrStopped
	}
	s.workers.Add()
	s.mu.Unlock()
	defer s.workers.Done()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Result{}, ErrLimit
	}
	if req.ProxyID == "" && req.Draft == nil {
		return Result{}, fmt.Errorf("%w: proxy_id or draft required", ErrValidation)
	}
	profile := config.ProxyProfile{}
	if req.ProxyID != "" {
		if s.config == nil {
			return Result{}, config.ErrNotFound
		}
		cfg, err := s.config.RuntimeConfig()
		if err != nil {
			return Result{}, err
		}
		var ok bool
		profile, ok = cfg.Proxies[req.ProxyID]
		if !ok {
			return Result{}, config.ErrNotFound
		}
	}
	if d := req.Draft; d != nil {
		// Drafts are complete ordinary-field snapshots. Secret reuse is confined to
		// the same protocol, endpoint and username; explicit empty clears for testing.
		password := profile.Password
		if profile.Type != d.Type || profile.Host != d.Host || profile.Port != d.Port || profile.Username != d.Username {
			password = ""
		}
		if d.Password != nil {
			password = *d.Password
		}
		profile = config.ProxyProfile{Type: d.Type, Host: d.Host, Port: d.Port, Username: d.Username, Password: password}
	}
	if (profile.Type != "http" && profile.Type != "socks5") || profile.Host == "" || profile.Port < 1 || profile.Port > 65535 || strings.ContainsAny(profile.Host, " /\t\x00\r\n") || strings.ContainsAny(profile.Username+profile.Password, "\x00\r\n") || (profile.Type == "socks5" && (len(profile.Username) > 255 || len(profile.Password) > 255)) {
		return Result{}, fmt.Errorf("%w: invalid proxy fields", ErrValidation)
	}
	if req.Target != "" {
		if err := proxyproto.ValidateTarget(req.Target); err != nil {
			return Result{}, fmt.Errorf("%w: invalid target", ErrValidation)
		}
	}
	if req.TimeoutMS < 0 || req.TimeoutMS > 60000 {
		return Result{}, fmt.Errorf("%w: timeout_ms must be 0..60000", ErrValidation)
	}

	timeout := 15 * time.Second
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	result := Result{Scope: "endpoint", Stages: []Stage{}}
	if req.Target != "" {
		result.Scope = "tunnel"
	} else if profile.Type == "socks5" {
		result.Scope = "negotiation"
	}
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port)))
	result.Stages = append(result.Stages, Stage{Name: "endpoint", Passed: err == nil, DurationMS: time.Since(start).Milliseconds()})
	if err == nil {
		defer conn.Close()
		if profile.Type == "socks5" || req.Target != "" {
			start = time.Now()
			_, err = proxyproto.Negotiate(ctx, conn, proxyproto.Options{Type: profile.Type, Username: profile.Username, Password: profile.Password}, req.Target, timeout)
			result.Stages = append(result.Stages, Stage{Name: result.Scope, Passed: err == nil, DurationMS: time.Since(start).Milliseconds()})
		}
	}
	result.Passed = err == nil
	if err != nil {
		result.FailureCode = "PROXY_UNREACHABLE"
		if len(result.Stages) > 1 {
			result.FailureCode = "PROXY_NEGOTIATION_FAILED"
		}
		var failure *proxyproto.Error
		if errors.As(err, &failure) {
			result.FailureCode = failure.Code
			return result, nil
		}
		var networkError net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			result.FailureCode = "PROXY_TEST_TIMEOUT"
		} else if ctx.Err() != nil {
			result.FailureCode = "PROXY_TEST_CANCELED"
		}
	}
	return result, nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
	return s.workers.Wait(ctx)
}
