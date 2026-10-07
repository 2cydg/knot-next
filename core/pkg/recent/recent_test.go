package recent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"knot-core/internal/paths"
)

func TestRecentStableDedupUTCAndLimits(t *testing.T) {
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.FixedZone("offset", 3600))
	for _, limit := range []int{-1, 0, 1, 2, 5000} {
		entries := normalize([]Entry{{"b", now}, {"a", now}, {"a", now.Add(-time.Hour)}, {"deleted", now}, {"empty", time.Time{}}}, map[string]bool{"a": true, "b": true, "empty": true}, limit)
		want := 2
		if limit == 1 {
			want = 1
		}
		if len(entries) != want || entries[0].ServerID != "a" || entries[0].LastUsed.Location() != time.UTC {
			t.Fatalf("limit %d: %+v", limit, entries)
		}
	}
}
func TestRecentConcurrentPersistenceAndFailedWrite(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(root, root)
	svc := New(layout, func() time.Time { return time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC) })
	valid := map[string]bool{}
	for i := 0; i < 50; i++ {
		valid[fmt.Sprint(i)] = true
	}
	var wg sync.WaitGroup
	for id := range valid {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := svc.Record(id, valid, 50); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	restarted := New(layout, nil)
	entries, err := restarted.Entries(valid, 50)
	if err != nil || len(entries) != 50 {
		t.Fatalf("lost concurrent successes: %d %v", len(entries), err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "state.json"))
	svc.write = func(string, []byte, os.FileMode) error { return errors.New("disk failure") }
	if err := svc.Record("0", valid, 1); err == nil {
		t.Fatal("write failure ignored")
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if string(before) != string(after) {
		t.Fatal("failed write changed history")
	}
	if err := restarted.Record("deleted", valid, 50); err != nil {
		t.Fatal(err)
	}
	delete(valid, "0")
	if err := restarted.Prune(valid, 50); err != nil {
		t.Fatal(err)
	}
	entries, _ = restarted.Entries(valid, 50)
	if len(entries) != 49 {
		t.Fatal("deletion not pruned")
	}
	for _, e := range entries {
		if e.ServerID == "0" {
			t.Fatal("dangling entry")
		}
	}
}
func TestRecentCorruptionIsQuarantinedAndRecovered(t *testing.T) {
	root := t.TempDir()
	svc := New(paths.NewLayout(root, root), nil)
	if err := os.WriteFile(svc.path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Record("a", map[string]bool{"a": true}, 5); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(svc.path + ".corrupt.1")
	if err != nil || string(raw) != "broken" {
		t.Fatalf("lost evidence: %q %v", raw, err)
	}
	entries, err := svc.Entries(map[string]bool{"a": true}, 5)
	if err != nil || len(entries) != 1 || entries[0].ServerID != "a" {
		t.Fatalf("failed recovery: %+v %v", entries, err)
	}
}

func TestRecentQuarantineFailurePreservesOriginal(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			root := t.TempDir()
			svc := New(paths.NewLayout(root, root), nil)
			if err := os.WriteFile(svc.path, []byte("broken"), 0600); err != nil {
				t.Fatal(err)
			}
			if full {
				for i := 1; i <= 3; i++ {
					if err := os.WriteFile(fmt.Sprintf("%s.corrupt.%d", svc.path, i), []byte("prior evidence"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				svc.rename = func(string, string) error { return errors.New("rename denied") }
			}
			if _, err := svc.Entries(map[string]bool{"a": true}, 5); err == nil {
				t.Fatal("quarantine error concealed")
			}
			if err := svc.Record("a", map[string]bool{"a": true}, 5); err == nil {
				t.Fatal("unsafe overwrite allowed")
			}
			if err := svc.Prune(map[string]bool{}, 5); err == nil {
				t.Fatal("unsafe prune allowed")
			}
			raw, _ := os.ReadFile(svc.path)
			if string(raw) != "broken" {
				t.Fatal("lost original")
			}
			backups, _ := filepath.Glob(svc.path + ".corrupt.*")
			want := 0
			if full {
				want = 3
			}
			if len(backups) != want {
				t.Fatalf("unexpected backups: %v", backups)
			}
		})
	}
}
