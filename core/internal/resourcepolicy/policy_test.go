package resourcepolicy

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestHistoryPolicy(t *testing.T) {
	now := time.Unix(100, 0)
	p := Policy{History: 2, TTL: 10 * time.Second, Now: func() time.Time { return now }}
	if p.Interval() != time.Minute {
		t.Fatal("default sweep interval")
	}
	for _, tc := range []struct {
		name  string
		items []Item
		want  []string
	}{
		{"empty", nil, nil}, {"boundary", []Item{{"a", now.Add(-10 * time.Second)}, {"b", now.Add(-9 * time.Second)}}, []string{"a"}},
		{"capacity", []Item{{"c", now}, {"a", now}, {"b", now}}, []string{"a"}},
		{"ttl_and_capacity", []Item{{"a", now.Add(-20 * time.Second)}, {"b", now.Add(-15 * time.Second)}, {"c", now}}, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Expired(tc.items); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	for _, defaultPolicy := range []Policy{Sessions(), Transfers(), Execs()} {
		if defaultPolicy.Active <= 0 || defaultPolicy.History <= 0 || defaultPolicy.TTL < 10*time.Minute || defaultPolicy.Now().IsZero() || defaultPolicy.Interval() != time.Minute {
			t.Fatal("invalid default policy")
		}
	}
}
func TestWorkerOwnershipAndRepeatedWait(t *testing.T) {
	var g Group
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.Add()
	if g.Count() != 1 {
		t.Fatal("missing worker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(ctx); err == nil {
		t.Fatal("deadline hidden")
	}
	g.Done()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	ctx, local := Scope(WithWork(context.Background(), func() func() { g.Add(); return g.Done }))
	Go(ctx, func() { close(started); <-release })
	<-started
	if g.Count() != 1 || local.Count() != 1 {
		t.Fatal("late work lost owner")
	}
	close(release)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := local.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	Go(context.Background(), func() {})
}
func TestCallbackQueueBoundedAndPanicIsolated(t *testing.T) {
	var c Callbacks
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c.Send(func() { close(started); <-release })
	<-started
	for i := 0; i < 300; i++ {
		c.Send(func() {})
	}
	c.mu.Lock()
	n := len(c.queue)
	c.mu.Unlock()
	if n != 256 {
		t.Fatalf("queued=%d", n)
	}
	c.Close()
	c.Close()
	c.Send(func() { t.Error("closed callback accepted") })
	close(release)
	var other Callbacks
	var once sync.Once
	other.Send(func() { panic("callback panic") })
	other.Send(func() { once.Do(func() { close(finished) }) })
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("panic stopped next callback")
	}
	other.Close()
}

func TestCancellationCallbackOwnership(t *testing.T) {
	var g Group
	bind := func(ctx context.Context) context.Context {
		return WithWork(ctx, func() func() { g.Add(); return g.Done })
	}
	stop := AfterFunc(bind(context.Background()), func() { t.Error("stopped callback ran") })
	if !stop() || stop() {
		t.Fatal("stop is not idempotent")
	}
	if g.Count() != 0 {
		t.Fatal("stopped callback retained owner")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	stop = AfterFunc(bind(ctx), func() { close(started); <-release })
	cancel()
	<-started
	if stop() {
		t.Fatal("running callback unexpectedly stopped")
	}
	if g.Count() != 1 {
		t.Fatal("running callback lost owner")
	}
	close(release)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryWithinCapacityPreservesOrder(t *testing.T) {
	now := time.Unix(100, 0)
	p := Policy{History: 3, TTL: time.Minute, Now: func() time.Time { return now }}
	alive := []Item{{"new", now}, {"old", now.Add(-time.Second)}, {"middle", now}}
	expired := Item{"expired", now.Add(-time.Minute)}
	for _, tc := range []struct {
		name  string
		items []Item
		want  []string
	}{
		{"healthy", alive, nil},
		{"expired_first", []Item{expired, alive[0], alive[1], alive[2]}, []string{"expired"}},
		{"expired_middle", []Item{alive[0], expired, alive[1], alive[2]}, []string{"expired"}},
		{"expired_last", []Item{alive[0], alive[1], alive[2], expired}, []string{"expired"}},
		{"capacity", []Item{alive[0], alive[1], alive[2], {"extra", now}}, []string{"old"}},
		{"ttl_and_capacity", []Item{expired, alive[0], alive[1], alive[2], {"extra", now}}, []string{"expired", "old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]Item(nil), tc.items...)
			if ids := p.Expired(tc.items); !reflect.DeepEqual(ids, tc.want) || !reflect.DeepEqual(tc.items, before) {
				t.Fatalf("ids=%v want=%v; input=%+v want unchanged=%+v", ids, tc.want, tc.items, before)
			}
		})
	}
}

func BenchmarkHistoryWithinCapacity(b *testing.B) {
	now := time.Now()
	p := Transfers()
	p.Now = func() time.Time { return now }
	items := make([]Item, p.History)
	for i := range items {
		items[i] = Item{ID: "history", End: now}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Expired(items)
	}
}
