package sftp

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"knot-core/pkg/session"
)

type ControlRequest struct {
	Op   string `json:"op"`
	Path string `json:"path,omitempty"`
}

// Control changes only the SFTP directory/follow relationship, never the shell.
func (s *Service) Control(id string, req ControlRequest) (Session, error) {
	switch req.Op {
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
		if err := s.refreshFollow(id); err != nil {
			return Session{}, err
		}
		return s.Get(id)
	default:
		return Session{}, fmt.Errorf("%w: invalid control op", ErrValidation)
	}
}
func (s *Service) directoryAccessible(id, dir string) error {
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
		_, err = res.client.ReadDirContext(s.lifeCtx, clean)
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
	if res == nil || res.followGeneration != generation {
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
	if res == nil || res.followGeneration != generation {
		s.mu.Unlock()
		return nil
	}
	res.followPath = dir
	if res.FollowState != "active" || res.State != "open" || dir == "" {
		s.mu.Unlock()
		return nil
	}
	res.cwdGeneration++
	cwdGeneration := res.cwdGeneration
	s.mu.Unlock()
	err := s.directoryAccessible(id, dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	res = s.sessions[id]
	if res == nil || res.followGeneration != generation || res.cwdGeneration != cwdGeneration || res.FollowState != "active" || res.State != "open" {
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
