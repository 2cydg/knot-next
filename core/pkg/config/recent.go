package config

import (
	"log/slog"
	"time"

	"knot-core/internal/logger"
)

func validServers(cfg Config) map[string]bool {
	valid := make(map[string]bool, len(cfg.Servers))
	for id := range cfg.Servers {
		valid[id] = true
	}
	return valid
}
func (s *Service) recentTimes(cfg Config) map[string]time.Time {
	entries, err := s.recent.Entries(validServers(cfg), cfg.Settings.RecentLimit)
	if err != nil {
		// Optional history must never disable configuration or alias resolution.
		logger.Diagnostic(slog.LevelWarn, "recent history unavailable", "error", err)
		return nil
	}
	used := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		used[e.ServerID] = e.LastUsed
	}
	return used
}
func (s *Service) serverViewWithRecent(cfg Config, server ServerProfile) ServerProfileView {
	view := serverView(server)
	if used, ok := s.recentTimes(cfg)[server.ID]; ok {
		view.LastUsed = &used
	}
	return view
}
func (s *Service) serverViewsWithRecent(cfg Config) []ServerProfileView {
	used := s.recentTimes(cfg)
	views := serverViews(cfg)
	for i := range views {
		if t, ok := used[views[i].ID]; ok {
			views[i].LastUsed = &t
		}
	}
	return views
}

// RecordUse is called only after the selected target's backend/channel succeeds.
// Holding config.mu also orders deletion against a concurrent success callback.
func (s *Service) RecordUse(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	if err := s.loadLocked(); err != nil {
		logger.Diagnostic(slog.LevelWarn, "recent history config unavailable", "resource_id", id, "error", err)
		return
	}
	if err := s.recent.Record(id, validServers(s.cfg), s.cfg.Settings.RecentLimit); err != nil {
		logger.Diagnostic(slog.LevelWarn, "recent history write failed", "resource_id", id, "error", err)
	}
}
