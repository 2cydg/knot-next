package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knot-core/pkg/recent"
)

func TestServerRecentSortDeletionAndConfigUnchanged(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	svc.recent = recent.New(svc.layout, func() time.Time { return now })
	for _, id := range []string{"a", "b", "c"} {
		if _, err := svc.CreateServer(ServerProfile{ID: id, Alias: id, Host: "host", Port: 22, User: "user", AuthMethod: AuthMethodAgent}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(svc.path)
	svc.RecordUse("b")
	now = now.Add(time.Second)
	svc.RecordUse("c")
	page, err := svc.ListServersPage(ServerListOptions{Sort: "recent"})
	if err != nil || page.Items[0].ID != "c" || page.Items[1].ID != "b" || page.Items[2].LastUsed != nil {
		t.Fatalf("wrong recent ordering: %+v %v", page, err)
	}
	after, _ := os.ReadFile(svc.path)
	if string(before) != string(after) {
		t.Fatal("recent touched secret config")
	}
	got, err := svc.GetServer("c")
	if err != nil || got.LastUsed == nil {
		t.Fatalf("GET lacks history: %+v %v", got, err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); svc.RecordUse("b") }()
	go func() {
		defer wg.Done()
		if err := svc.DeleteServer("b"); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	cfg, err := svc.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := svc.recent.Entries(validServers(cfg), 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ServerID == "b" {
			t.Fatal("dangling deleted server")
		}
	}
}

func TestHistoryFailuresDoNotBlockConfiguration(t *testing.T) {
	for _, unrecoverable := range []bool{false, true} {
		t.Run(fmt.Sprint(unrecoverable), func(t *testing.T) {
			svc := newTestService(t)
			if _, err := svc.CreateServer(ServerProfile{ID: "a", Alias: "web", Host: "host", Port: 22, User: "user", AuthMethod: AuthMethodAgent}); err != nil {
				t.Fatal(err)
			}
			history := filepath.Join(svc.layout.StateDir, "state.json")
			if err := os.WriteFile(history, []byte("corrupt-state"), 0600); err != nil {
				t.Fatal(err)
			}
			if unrecoverable {
				for i := 1; i <= 3; i++ {
					if err := os.WriteFile(fmt.Sprintf("%s.corrupt.%d", history, i), []byte("old"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := svc.Summary(); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ListServers(); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ListServersPage(ServerListOptions{}); err != nil {
				t.Fatal(err)
			}
			for _, ref := range []string{"a", "web"} {
				view, err := svc.ResolveServer(ref)
				if err != nil || view.ID != "a" || view.LastUsed != nil {
					t.Fatalf("resolve: %+v %v", view, err)
				}
			}
			if view, err := svc.GetServer("a"); err != nil || view.LastUsed != nil {
				t.Fatalf("get: %+v %v", view, err)
			}
			svc.RecordUse("a")
			raw, _ := os.ReadFile(history)
			if unrecoverable {
				if string(raw) != "corrupt-state" {
					t.Fatal("lost source")
				}
			} else {
				if view, err := svc.GetServer("a"); err != nil || view.LastUsed == nil {
					t.Fatalf("history not rebuilt: %+v %v", view, err)
				}
			}
			if err := os.WriteFile(svc.path, []byte("invalid TOML !"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ListServers(); err == nil {
				t.Fatal("business config corruption concealed")
			}
			svc.RecordUse("a")
		})
	}
}

func TestResolveServerExactIDWinsAliasCollision(t *testing.T) {
	svc := newTestService(t)
	for _, p := range []ServerProfile{{ID: "a", Alias: "z"}, {ID: "z", Alias: "target"}} {
		p.Host = "host"
		p.Port = 22
		p.User = "user"
		p.AuthMethod = AuthMethodAgent
		if _, err := svc.CreateServer(p); err != nil {
			t.Fatal(err)
		}
	}
	svc.RecordUse("z")
	for i := 0; i < 20; i++ {
		view, err := svc.ResolveServer("z")
		if err != nil || view.ID != "z" || view.LastUsed == nil {
			t.Fatalf("wrong target: %+v %v", view, err)
		}
	}
}

func TestResolveDuplicateLegacyAliasHasStableIDOrder(t *testing.T) {
	svc := newTestService(t)
	for _, id := range []string{"z", "a"} {
		if _, err := svc.CreateServer(ServerProfile{ID: id, Alias: id, Host: "host", Port: 22, User: "user", AuthMethod: AuthMethodAgent}); err != nil {
			t.Fatal(err)
		}
	}
	// Legacy files may contain duplicate aliases although current CRUD rejects them.
	raw, err := os.ReadFile(svc.path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), `alias = "a"`, `alias = "legacy"`), `alias = "z"`, `alias = "legacy"`)
	if err := os.WriteFile(svc.path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		view, err := svc.ResolveServer("legacy")
		if err != nil || view.ID != "a" {
			t.Fatalf("unstable alias: %+v %v", view, err)
		}
	}
}
