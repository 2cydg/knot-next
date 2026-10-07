package session

import (
	"context"
	"errors"
	"time"

	"knot-core/internal/resourcepolicy"
)

func (s *Service) activeLocked() int {
	count := 0
	for _, res := range s.sessions {
		if !isTerminalState(res.State) {
			count++
		}
	}
	return count
}

// Pending cleanup is live work, separate from retained terminal history.
func (s *Service) pendingLocked() int {
	count := 0
	for _, res := range s.sessions {
		if isTerminalState(res.State) && res.workers > 0 {
			count++
		}
	}
	return count
}
func (s *Service) pruneSessionsLocked() {
	var items []resourcepolicy.Item
	for id, res := range s.sessions {
		if isTerminalState(res.State) && res.workers == 0 && res.ExitedAt != nil {
			items = append(items, resourcepolicy.Item{ID: id, End: *res.ExitedAt})
		}
	}
	for _, id := range s.policy.Expired(items) {
		res := s.sessions[id]
		s.closeSubscribersLocked(res)
		delete(s.sessions, id)
	}
}
func (s *Service) closeSubscribersLocked(res *resource) {
	for ch := range res.subscribers {
		close(ch)
		delete(res.subscribers, ch)
	}
	for ch := range res.cwdSubscribers {
		close(ch)
		delete(res.cwdSubscribers, ch)
	}
}

// beginWorkLocked registers ownership before launching work, including late cleanup.
func (s *Service) beginWorkLocked(res *resource) func() {
	s.workers.Add()
	if res != nil {
		res.workers++
	}
	return func() {
		s.mu.Lock()
		if res != nil {
			res.workers--
			if res.workers == 0 && isTerminalState(res.State) {
				s.pruneSessionsLocked()
			}
		}
		s.mu.Unlock()
		s.workers.Done()
	}
}
func (s *Service) runLocked(res *resource, fn func()) {
	done := s.beginWorkLocked(res)
	go func() { defer done(); fn() }()
}

// StartMaintenance binds the cleaner to the process lifecycle. Reads also prune
// so expiry is immediately observable even between ticks.
func (s *Service) StartMaintenance(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.maintenanceStarted {
		return
	}
	s.maintenanceStarted = true
	s.runLocked(nil, func() {
		ticker := time.NewTicker(s.policy.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.execCtx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				s.pruneSessionsLocked()
				s.pruneExecsLocked()
				s.mu.Unlock()
			}
		}
	})
}
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		for _, res := range s.sessions {
			if isTerminalState(res.State) {
				continue
			}
			backend := s.closeSessionLocked(res, "closed", ExitOutcome{DisconnectCause: causeClientDisconnected}, s.policy.Now().UTC())
			if backend != nil {
				s.runLocked(res, func() {
					if err := backend.Close(); err != nil && !errors.Is(err, ErrTeardownTimeout) {
						s.mu.Lock()
						s.releaseErrors = append(s.releaseErrors, err)
						s.mu.Unlock()
					}
				})
			}
		}
	}
	s.mu.Unlock()
	s.CancelExec()
	execErr := s.ShutdownExec(ctx)
	workerErr := s.workers.Wait(ctx)
	s.callbacks.Close()
	s.mu.RLock()
	errs := append([]error(nil), s.releaseErrors...)
	s.mu.RUnlock()
	return errors.Join(append(errs, execErr, workerErr)...)
}
