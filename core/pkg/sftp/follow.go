package sftp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"knot-core/pkg/session"
)

const maxFollowWorkers = 8

type ControlRequest struct {
	FollowSessionID    string `json:"follow_session_id,omitempty"`
	FollowConnectionID string `json:"follow_connection_id,omitempty"`
	Op                 string `json:"op"`
	Path               string `json:"path,omitempty"`
}

// Control changes only the SFTP directory/follow relationship, never the shell.
func (s *Service) Control(id string, req ControlRequest) (Session, error) {
	switch req.Op {
	case "set-follow":
		return s.setFollow(id, req.FollowSessionID, req.FollowConnectionID, false)
	case "clear-follow":
		return s.clearFollow(id)
	case "cd":
		if req.Path == "" {
			return Session{}, fmt.Errorf("%w: path is required", ErrValidation)
		}
		s.mu.Lock()
		res := s.sessions[id]
		if res == nil {
			s.mu.Unlock()
			return Session{}, ErrNotFound
		}
		if res.State != "open" {
			s.mu.Unlock()
			return Session{}, ErrConflict
		}
		target := req.Path
		if !path.IsAbs(target) {
			target = path.Join(res.CurrentDir, target)
		}
		target = path.Clean(target)
		// Validate first; a failed cd leaves the directory and follow state intact.
		generation := res.cwdGeneration
		s.mu.Unlock()
		if err := s.directoryAccessible(id, target); err != nil {
			return Session{}, err
		}
		s.mu.Lock()
		res = s.sessions[id]
		if res == nil || res.State != "open" || res.cwdGeneration != generation {
			s.mu.Unlock()
			return Session{}, ErrConflict
		}
		res.cwdGeneration++
		if res.FollowSessionID != "" && res.FollowState != "invalid" {
			res.FollowState = "paused"
		}
		now := s.policy.Now().UTC()
		res.CurrentDir = target
		res.CWDUpdatedAt = &now
		res.UpdatedAt = now
		res.FollowError = ""
		s.publishSessionLocked(res, Event{Type: "sftp.cwd.changed", SessionID: id, State: res.State, Path: target, Time: now})
		out := res.snapshot()
		s.mu.Unlock()
		return out, nil
	case "pause-follow":
		s.mu.Lock()
		defer s.mu.Unlock()
		res := s.sessions[id]
		if res == nil {
			return Session{}, ErrNotFound
		}
		if res.State != "open" || res.FollowSessionID == "" || res.FollowState == "invalid" {
			return Session{}, ErrConflict
		}
		res.FollowState = "paused"
		res.cwdGeneration++
		res.UpdatedAt = s.policy.Now().UTC()
		s.publishSessionLocked(res, Event{Type: "sftp.follow.paused", SessionID: id, State: res.State, Time: res.UpdatedAt})
		return res.snapshot(), nil
	case "resume-follow":
		s.mu.Lock()
		res := s.sessions[id]
		if res == nil {
			s.mu.Unlock()
			return Session{}, ErrNotFound
		}
		if res.State != "open" || res.FollowSessionID == "" || res.FollowState == "invalid" {
			s.mu.Unlock()
			return Session{}, ErrConflict
		}
		res.FollowState = "active"
		res.cwdGeneration++
		res.UpdatedAt = s.policy.Now().UTC()
		s.mu.Unlock()
		_ = s.refreshFollow(id) // The binding is committed; directory results live in the View.
		return s.Get(id)
	default:
		return Session{}, fmt.Errorf("%w: invalid control op", ErrValidation)
	}
}
func (s *Service) directoryAccessible(id, dir string) error {
	return s.directoryAccessibleContext(s.lifeCtx, id, dir)
}
func (s *Service) directoryAccessibleContext(ctx context.Context, id, dir string) error {
	if !path.IsAbs(dir) || strings.ContainsAny(dir, "\x00\r\n") {
		return fmt.Errorf("%w: invalid directory", ErrValidation)
	}
	res, clean, err := s.requireOpenSession(id, dir)
	if err != nil {
		return err
	}
	if res.client != nil {
		// Stat cannot prove listing permission. pkg/sftp exposes only whole-
		// directory reads; bypass cache, entry conversion, sorting and pagination.
		_, err = res.client.ReadDirContext(ctx, clean)
	} else {
		var local string
		local, _, err = s.resolve(id, clean)
		if err == nil {
			var f *os.File
			f, err = os.Open(local)
			if err == nil {
				_, err = f.ReadDir(1)
				if err == io.EOF {
					err = nil // An empty readable directory is valid.
				}
				_ = f.Close()
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%w: directory unavailable", ErrValidation)
	}
	return nil
}
func (s *Service) refreshFollow(id string) error {
	s.mu.RLock()
	res := s.sessions[id]
	if res == nil || res.FollowState != "active" {
		s.mu.RUnlock()
		return nil
	}
	provider, source, generation := s.session, res.FollowSessionID, res.followGeneration
	s.mu.RUnlock()
	if provider == nil {
		return ErrConflict
	}
	current, err := provider.Get(source)
	if err != nil || isSourceTerminal(current.State) {
		s.invalidateFollow(id, generation)
		return fmt.Errorf("%w: follow source closed", ErrConflict)
	}
	return s.applyFollow(id, generation, current.CurrentDir)
}
func isSourceTerminal(state string) bool {
	return state == "closed" || state == "failed" || state == "disconnected"
}
func (s *Service) invalidateFollow(id string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.sessions[id]
	if res == nil || res.followGeneration != generation || isTerminalSessionState(res.State) {
		return
	}
	res.FollowState = "invalid"
	res.cwdGeneration++
	res.UpdatedAt = s.policy.Now().UTC()
	s.cancelFollowLocked(res)
	s.publishSessionLocked(res, Event{Type: "sftp.follow.invalidated", SessionID: id, State: res.State, Time: res.UpdatedAt})
}
func (s *Service) applyFollow(id string, generation uint64, dir string) error {
	s.mu.Lock()
	res := s.sessions[id]
	if res == nil || res.followGeneration != generation || isTerminalSessionState(res.State) {
		s.mu.Unlock()
		return nil
	}
	res.followPath = dir
	res.FollowSourceDir = dir
	if res.FollowState != "active" || res.State != "open" || dir == "" {
		s.mu.Unlock()
		return nil
	}
	res.cwdGeneration++
	cwdGeneration := res.cwdGeneration
	ctx := res.followCtx
	if ctx == nil {
		ctx = s.lifeCtx
	}
	s.mu.Unlock()
	err := s.directoryAccessibleContext(ctx, id, dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	res = s.sessions[id]
	if res == nil || res.followGeneration != generation || isTerminalSessionState(res.State) || res.cwdGeneration != cwdGeneration || res.FollowState != "active" || res.State != "open" {
		return nil
	}
	now := s.policy.Now().UTC()
	res.UpdatedAt = now
	if err != nil {
		res.FollowError = "directory_unavailable"
		s.publishSessionLocked(res, Event{Type: "sftp.cwd.follow_error", SessionID: id, State: res.State, Path: dir, Error: res.FollowError, Time: now})
		return err
	}
	res.FollowError = ""
	if res.CurrentDir != dir || res.CWDUpdatedAt == nil {
		res.CurrentDir = dir
		res.CWDUpdatedAt = &now
		s.publishSessionLocked(res, Event{Type: "sftp.cwd.follow", SessionID: id, State: res.State, Path: dir, Time: now})
	}
	return nil
}
func (s *Service) followSessionCWD(id string, generation uint64, ch <-chan session.CWDNotify) {
	// Initial snapshot is validated through the same I/O path as later updates.
	s.mu.RLock()
	res := s.sessions[id]
	initial := ""
	if res != nil {
		initial = res.followPath
	}
	s.mu.RUnlock()
	_ = s.applyFollow(id, generation, initial)
	for notify := range ch {
		if notify.Closed {
			s.invalidateFollow(id, generation)
			return
		}
		_ = s.applyFollow(id, generation, notify.Path)
	}
	s.invalidateFollow(id, generation)
}

// validateFollowSource accepts every ready SSH session belonging to this server,
// independently of the SFTP transport's authenticated identity.
func (s *Service) validateFollowSource(serverRef, sourceID, connectionID string) (session.Resource, error) {
	cfgProvider, provider, _, testMode := s.dependencies()
	if provider == nil {
		return session.Resource{}, fmt.Errorf("%w: session service unavailable", ErrConflict)
	}
	source, err := provider.Get(sourceID)
	if err != nil {
		return session.Resource{}, fmt.Errorf("%w: follow source unavailable", ErrConflict)
	}
	if source.State != "connected" && source.State != "attached" && source.State != "detached" {
		return session.Resource{}, fmt.Errorf("%w: follow source is not ready", ErrConflict)
	}
	if connectionID != "" && source.ConnectionID != connectionID {
		return session.Resource{}, fmt.Errorf("%w: follow connection/session mismatch", ErrValidation)
	}
	if !testMode && source.ConnectionID == "" {
		return session.Resource{}, fmt.Errorf("%w: source has no authenticated connection", ErrConflict)
	}
	if cfgProvider != nil && !testMode {
		cfg, err := cfgProvider.RuntimeConfig()
		if err != nil {
			return session.Resource{}, err
		}
		target, err := resolveServer(cfg, serverRef)
		if err != nil {
			return session.Resource{}, err
		}
		if source.ServerID != target.ID {
			return session.Resource{}, fmt.Errorf("%w: follow source belongs to a different server", ErrValidation)
		}
	} else if source.ServerRef != serverRef && source.Alias != serverRef && source.ServerID != serverRef {
		return session.Resource{}, fmt.Errorf("%w: follow source belongs to a different server", ErrValidation)
	}
	return source, nil
}
func (s *Service) setFollow(id, sourceID, connectionID string, creating bool) (Session, error) {
	if sourceID == "" {
		return Session{}, fmt.Errorf("%w: follow_session_id is required", ErrValidation)
	}
	s.mu.RLock()
	r := s.sessions[id]
	if r == nil {
		s.mu.RUnlock()
		return Session{}, ErrNotFound
	}
	serverRef, generation, provider := r.ServerRef, r.followGeneration, s.session
	if s.stopped || (!creating && r.State != "open") || isTerminalSessionState(r.State) {
		s.mu.RUnlock()
		return Session{}, ErrConflict
	}
	s.mu.RUnlock()
	source, err := s.validateFollowSource(serverRef, sourceID, connectionID)
	if err != nil {
		return Session{}, err
	}
	if provider == nil {
		return Session{}, ErrConflict
	}
	s.mu.Lock()
	r = s.sessions[id]
	if r != nil && !s.stopped && !creating && r.State == "open" && r.FollowSessionID == sourceID && r.FollowConnectionID == source.ConnectionID && r.FollowState != "invalid" {
		out := r.snapshot()
		s.mu.Unlock()
		return out, nil
	}
	if r == nil || r.followWorkers >= maxFollowWorkers {
		s.mu.Unlock()
		return Session{}, fmt.Errorf("%w: follow cleanup capacity reached", ErrConflict)
	}
	s.mu.Unlock()
	ch, unsubscribe, snapshot, err := provider.SubscribeCWD(sourceID)
	if err != nil {
		return Session{}, fmt.Errorf("%w: follow subscription unavailable", ErrConflict)
	}
	followCtx, cancelWork := context.WithCancel(s.lifeCtx)
	cancel := func() { cancelWork(); unsubscribe() }

	// Validate the actual subscribed snapshot, not only the earlier GET.
	if snapshot.ID != source.ID || snapshot.ConnectionID != source.ConnectionID || isSourceTerminal(snapshot.State) {
		cancel()
		return Session{}, ErrConflict
	}
	current, sourceErr := provider.Get(sourceID)
	if sourceErr != nil || current.ConnectionID != snapshot.ConnectionID || current.State != "connected" && current.State != "attached" && current.State != "detached" {
		cancel()
		return Session{}, ErrConflict
	}
	s.mu.Lock()
	r = s.sessions[id]
	if r != nil && !s.stopped && !creating && r.State == "open" && r.FollowSessionID == sourceID && r.FollowConnectionID == source.ConnectionID && r.FollowState != "invalid" {
		out := r.snapshot()
		s.mu.Unlock()
		cancel()
		return out, nil
	}
	if r == nil || s.stopped || r.followWorkers >= maxFollowWorkers || r.followGeneration != generation || isTerminalSessionState(r.State) || (!creating && r.State != "open") {
		s.mu.Unlock()
		cancel()
		return Session{}, ErrConflict
	}
	if !creating && r.FollowSessionID == sourceID && r.FollowConnectionID == source.ConnectionID && r.FollowState != "invalid" {
		out := r.snapshot()
		s.mu.Unlock()
		cancel()
		return out, nil
	}
	oldCancel := r.followCancel
	r.followGeneration++
	r.cwdGeneration++
	generation = r.followGeneration
	r.FollowSessionID = sourceID
	r.FollowConnectionID = source.ConnectionID
	r.FollowState = "active"
	r.FollowError = ""
	r.followPath = snapshot.CurrentDir
	r.FollowSourceDir = snapshot.CurrentDir
	r.followCancel = cancel
	r.followCtx = followCtx
	r.followWorkers++
	r.UpdatedAt = s.policy.Now().UTC()
	s.runLocked(r, func() {
		defer func() { s.mu.Lock(); r.followWorkers--; s.mu.Unlock() }()
		s.followSessionCWD(id, generation, ch)
	})
	s.publishSessionLocked(r, Event{Type: "sftp.follow.changed", SessionID: id, State: r.State, Time: r.UpdatedAt})
	s.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	// Directory application is owned by the new subscription worker. Control
	// acknowledges the binding; GET/events report its eventual directory result.

	return s.Get(id)
}
func (s *Service) clearFollow(id string) (Session, error) {
	s.mu.Lock()
	r := s.sessions[id]
	if r == nil {
		s.mu.Unlock()
		return Session{}, ErrNotFound
	}
	if r.State != "open" || s.stopped {
		s.mu.Unlock()
		return Session{}, ErrConflict
	}
	cancel := r.followCancel
	r.followCancel = nil
	r.followCtx = nil
	r.followGeneration++
	r.cwdGeneration++
	r.FollowSessionID = ""
	r.FollowConnectionID = ""
	r.FollowState = ""
	r.FollowError = ""
	r.followPath = ""
	r.FollowSourceDir = ""
	r.UpdatedAt = s.policy.Now().UTC()
	s.publishSessionLocked(r, Event{Type: "sftp.follow.cleared", SessionID: id, State: r.State, Time: r.UpdatedAt})
	out := r.snapshot()
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return out, nil
}
