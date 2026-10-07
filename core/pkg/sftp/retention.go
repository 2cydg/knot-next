package sftp

import (
	"context"
	"errors"
	"time"

	"knot-core/internal/resourcepolicy"
	"knot-core/pkg/config"
	"knot-core/pkg/session"

	pkgsftp "github.com/pkg/sftp"
)

func (s *Service) activeLocked() int {
	count := 0
	for _, res := range s.sessions {
		if !isTerminalSessionState(res.State) {
			count++
		}
	}
	return count
}

// Pending cleanup is live work, separate from retained terminal history.
func (s *Service) pendingLocked() int {
	count := 0
	for _, res := range s.sessions {
		if isTerminalSessionState(res.State) && res.workers > 0 {
			count++
		}
	}
	return count
}
func (s *Service) pruneSessionsLocked() {
	var items []resourcepolicy.Item
	for id, res := range s.sessions {
		if isTerminalSessionState(res.State) && res.workers == 0 && res.ClosedAt != nil {
			items = append(items, resourcepolicy.Item{ID: id, End: *res.ClosedAt})
		}
	}
	for _, id := range s.policy.Expired(items) {
		s.closeSubscribersLocked(id)
		delete(s.sessions, id)
	}
}
func (s *Service) closeSubscribersLocked(id string) {
	for ch := range s.subs[id] {
		close(ch)
	}
	delete(s.subs, id)
	for ch := range s.transferSubs[id] {
		close(ch)
	}
	delete(s.transferSubs, id)
}
func (s *Service) beginWorkLocked(res *resource) func() {
	s.workers.Add()
	if res != nil {
		res.workers++
	}
	return func() {
		s.mu.Lock()
		if res != nil {
			res.workers--
			if res.workers == 0 && isTerminalSessionState(res.State) {
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
func (s *Service) cancelFollowLocked(res *resource) {
	cancel := res.followCancel
	res.followCancel = nil
	if cancel != nil {
		s.runLocked(res, cancel)
	}
}
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
			case <-s.lifeCtx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				s.pruneSessionsLocked()
				s.pruneTransfersLocked()
				s.mu.Unlock()
			}
		}
	})
}
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		s.lifeCancel()
		for _, res := range s.sessions {
			if isTerminalSessionState(res.State) {
				continue
			}
			client, keys := s.closeSessionLocked(res, "closed", "core shutdown", s.policy.Now().UTC())
			if client != nil || len(keys) > 0 {
				s.runLocked(res, func() {
					if err := s.releaseDetached(res, client, keys); err != nil && !errors.Is(err, session.ErrTeardownTimeout) {
						s.mu.Lock()
						s.releaseErrors = append(s.releaseErrors, err)
						s.mu.Unlock()
					}
				})
			}
		}
	}
	s.mu.Unlock()
	workerErr := s.workers.Wait(ctx)
	s.callbacks.Close()
	s.mu.RLock()
	errs := append([]error(nil), s.releaseErrors...)
	s.mu.RUnlock()
	return errors.Join(append(errs, workerErr)...)
}

// Each subsystem observes transport loss independently of advisory pool events.
func (s *Service) watchRemote(res *resource, client *pkgsftp.Client, cfg config.RuntimeConfig) {
	err := client.Wait()
	s.mu.Lock()
	if isTerminalSessionState(res.State) || res.client != client {
		s.mu.Unlock()
		return
	}
	detached, keys := s.closeSessionLocked(res, "disconnected", safeError(err, cfg), s.policy.Now().UTC())
	s.mu.Unlock()
	_ = s.releaseDetached(res, detached, keys)
}
