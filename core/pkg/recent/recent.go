// Package recent owns the legacy state.json history independently of secrets.
// Model adapted from knot/pkg/config/state.go (e0b4d51eea6647e192371059381039b99fb301a2).
package recent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"knot-core/internal/fileutil"
	"knot-core/internal/logger"
	"knot-core/internal/paths"
)

type Entry struct {
	ServerID string    `json:"server_id"`
	LastUsed time.Time `json:"last_used"`
}
type State struct {
	Recent []Entry `json:"recent"`
}
type Service struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	write  func(string, []byte, os.FileMode) error
	rename func(string, string) error
}

func New(layout paths.Layout, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{path: filepath.Join(layout.StateDir, "state.json"), now: now, write: fileutil.AtomicWriteFile, rename: os.Rename}
}
func (s *Service) load() (State, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Recent: []Entry{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		if backupErr := s.quarantine(); backupErr != nil {
			return State{}, fmt.Errorf("invalid recent state (%v); quarantine failed: %w", err, backupErr)
		}
		logger.Diagnostic(slog.LevelWarn, "recent.state.quarantined", "reason", "invalid_json")
		return State{Recent: []Entry{}}, nil
	}
	return state, nil
}

// Keep at most three original files, without overwriting evidence. If slots are
// full or rename fails, writers refuse to replace the damaged source. Core owns
// a single process/Service; the lock also orders quarantine against its writers.
// A crash between reservation and rename can leave an empty occupied slot;
// preserve it like other backups until it is explicitly moved or removed.
func (s *Service) quarantine() error {
	for i := 1; i <= 3; i++ {
		backup := fmt.Sprintf("%s.corrupt.%d", s.path, i)
		f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = f.Close(); err == nil {
			err = s.rename(s.path, backup)
		}
		if err != nil {
			_ = os.Remove(backup)
		}
		return err
	}
	return errors.New("recent quarantine slots full; preserve or remove backups to recover history")
}

// Entries excludes deleted profiles, de-duplicates legacy records and is stable
// even when concurrent successful operations share a clock timestamp.
func (s *Service) Entries(valid map[string]bool, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return nil, err
	}
	return normalize(state.Recent, valid, limit), nil
}
func normalize(entries []Entry, valid map[string]bool, limit int) []Entry {
	if limit <= 0 {
		limit = 5
	}
	if limit > 1024 {
		limit = 1024
	}
	unique := map[string]time.Time{}
	for _, e := range entries {
		if e.ServerID == "" || !valid[e.ServerID] || e.LastUsed.IsZero() {
			continue
		}
		if e.LastUsed.After(unique[e.ServerID]) {
			unique[e.ServerID] = e.LastUsed.UTC()
		}
	}
	out := make([]Entry, 0, len(unique))
	for id, used := range unique {
		out = append(out, Entry{id, used})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastUsed.Equal(out[j].LastUsed) {
			return out[i].ServerID < out[j].ServerID
		}
		return out[i].LastUsed.After(out[j].LastUsed)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Record saves atomically under the same lock as the read. Failed writes leave
// no speculative in-memory result for later callers to publish.
func (s *Service) Record(id string, valid map[string]bool, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !valid[id] {
		return nil
	}
	state, err := s.load()
	if err != nil {
		return err
	}
	state.Recent = normalize(append(state.Recent, Entry{id, s.now().UTC()}), valid, limit)
	return s.save(state)
}
func (s *Service) Prune(valid map[string]bool, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return err
	}
	state.Recent = normalize(state.Recent, valid, limit)
	return s.save(state)
}
func (s *Service) save(state State) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return s.write(s.path, raw, 0600)
}
