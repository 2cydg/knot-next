// Package connection owns an SSH attempt independently of a renderer's lifetime.
package connection

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"knot-cli/internal/coreclient"
)

type Result struct {
	Session   coreclient.Session
	Challenge *coreclient.Challenge
	Err       error
}

type Attempt struct {
	Client    *coreclient.Client
	Instance  string
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	id        string
	warnings  []string
	watchDone chan struct{}
}

func New(ctx context.Context, client *coreclient.Client, instance string) *Attempt {
	ctx, cancel := context.WithCancel(ctx)
	return &Attempt{Client: client, Instance: instance, ctx: ctx, cancel: cancel}
}

func (a *Attempt) ID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

func (a *Attempt) Start(request coreclient.SessionRequest) Result {
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	if err := a.Client.CheckInstance(ctx, a.Instance); err != nil {
		return Result{Err: err}
	}
	// Observe warnings before creating the session; remembering credentials can
	// fail immediately after authentication, before the renderer hands off.
	created := make(chan struct{})
	if stream, err := a.Client.Events(a.ctx); err == nil {
		a.mu.Lock()
		a.watchDone = make(chan struct{})
		done := a.watchDone
		a.mu.Unlock()
		go func() {
			defer close(done)
			defer stream.Close()
			// Events buffers early warnings until the response identifies this
			// attempt. Never collect failures or warnings from other sessions.
			select {
			case <-created:
			case <-a.ctx.Done():
				return
			}
			id := a.ID()
			if id == "" {
				return
			}
			for item := range stream.Items {
				e := item.Event
				if e.Type != "session.warning" || (e.Level != "warn" && e.Level != "warning" && e.Level != "error") {
					continue
				}
				var data struct {
					SessionID string `json:"session_id"`
					Warning   struct {
						Kind string `json:"kind"`
					} `json:"warning"`
				}
				_ = json.Unmarshal(e.Data, &data)
				sessionID := e.ResourceID
				if sessionID == "" {
					sessionID = data.SessionID
				}
				if sessionID != id {
					continue
				}
				code := data.Warning.Kind
				if code == "" {
					code = e.Type
				}
				a.mu.Lock()
				if len(a.warnings) == 16 {
					a.warnings = a.warnings[1:]
				}
				a.warnings = append(a.warnings, code)
				a.mu.Unlock()
			}
		}()
	}
	s, err := a.Client.CreateSession(ctx, request)
	a.mu.Lock()
	a.id = s.ID
	a.mu.Unlock()
	close(created)
	// Cancellation can race the 201 response. Close the accepted resource even
	// if the UI has already moved on; never leave a known ID connecting.
	if a.ctx.Err() != nil {
		a.Close()
		return Result{Err: a.ctx.Err()}
	}
	return Result{Session: s, Err: err}
}

// Finish releases application-side observers without deleting the SSH resource.
func (a *Attempt) Finish() []string {
	a.cancel()
	a.mu.Lock()
	done := a.watchDone
	a.mu.Unlock()
	if done != nil {
		<-done
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.warnings...)
}

func (a *Attempt) Poll() Result {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	if err := a.Client.CheckInstance(ctx, a.Instance); err != nil {
		return Result{Err: err}
	}
	s, err := a.Client.Session(ctx, a.ID())
	r := Result{Session: s, Err: err}
	if err != nil {
		return r
	}
	kind := ""
	if s.State == "host_key_pending" {
		kind = "host-key"
	} else if s.State == "auth_pending" {
		kind = "auth"
	}
	if kind != "" {
		c, err := a.Client.SessionChallenge(ctx, s.ID, kind)
		r.Err = err
		if err == nil && c.Pending {
			if c.ID == "" || c.SessionID != s.ID {
				r.Err = coreclient.Failure("PROTOCOL_ERROR")
			} else {
				r.Challenge = &c
			}
		}
	}
	return r
}

func (a *Attempt) Answer(kind string, answer coreclient.ChallengeAnswer) error {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	if err := a.Client.CheckInstance(ctx, a.Instance); err != nil {
		return err
	}
	return a.Client.AnswerSession(ctx, a.ID(), kind, answer)
}

// Close explicitly disconnects this attempt. A dropped attachment alone is a
// detach and is deliberately not routed through Close by the terminal bridge.
func (a *Attempt) Close() {
	a.Finish()
	if id := a.ID(); id != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if a.Client.CheckInstance(ctx, a.Instance) == nil {
			_ = a.Client.CloseSession(ctx, id)
		}
	}
}
