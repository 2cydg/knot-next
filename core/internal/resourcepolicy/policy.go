// Package resourcepolicy holds the daemon's internal resource limits.
package resourcepolicy

import (
	"context"
	"sort"
	"sync"
	"time"
)

type Policy struct {
	Active     int
	History    int
	TTL        time.Duration
	Now        func() time.Time
	SweepEvery time.Duration
}

func Sessions() Policy {
	return Policy{Active: 1024, History: 1024, TTL: 10 * time.Minute, Now: time.Now, SweepEvery: time.Minute}
}
func Transfers() Policy {
	return Policy{Active: 4096, History: 4096, TTL: 10 * time.Minute, Now: time.Now, SweepEvery: time.Minute}
}
func Execs() Policy {
	return Policy{Active: 4096, History: 4096, TTL: 10 * time.Minute, Now: time.Now, SweepEvery: time.Minute}
}

type Item struct {
	ID  string
	End time.Time
}

// Expired selects only settled terminal records supplied by the owner. Capacity
// pressure may shorten retention; ties are deterministic. It never modifies
// the caller's items, including their order.
func (p Policy) Expired(items []Item) []string {
	now := p.Now()
	var ids []string
	retainedCount := 0
	for _, item := range items {
		if now.Sub(item.End) >= p.TTL {
			ids = append(ids, item.ID)
		} else {
			retainedCount++
		}
	}
	// TTL needs only a scan. Sort only when surviving history exceeds capacity;
	// healthy reads and worker completions never pay for a full sort.
	if retainedCount > p.History {
		retained := make([]Item, 0, retainedCount)
		for _, item := range items {
			if now.Sub(item.End) < p.TTL {
				retained = append(retained, item)
			}
		}
		sort.Slice(retained, func(i, j int) bool {
			if retained[i].End.Equal(retained[j].End) {
				return retained[i].ID < retained[j].ID
			}
			return retained[i].End.Before(retained[j].End)
		})
		for _, item := range retained[:len(retained)-p.History] {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// Group can be waited repeatedly with a deadline without spawning waiters.
// Owners prevent new Add calls once shutdown starts under their own state lock.
type Group struct {
	mu    sync.Mutex
	count int
	idle  chan struct{}
}

func (g *Group) Add() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.count == 0 {
		g.idle = make(chan struct{})
	}
	g.count++
}
func (g *Group) Done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.count--
	if g.count == 0 {
		close(g.idle)
	}
}
func (g *Group) Count() int { g.mu.Lock(); defer g.mu.Unlock(); return g.count }
func (g *Group) Wait(ctx context.Context) error {
	g.mu.Lock()
	if g.count == 0 {
		g.mu.Unlock()
		return ctx.Err()
	}
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p Policy) Interval() time.Duration {
	if p.SweepEvery > 0 {
		return p.SweepEvery
	}
	return time.Minute
}
