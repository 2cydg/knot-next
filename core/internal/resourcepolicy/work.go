package resourcepolicy

import "context"

type workKey struct{}

// WithWork propagates late channel/subsystem cleanup ownership through helpers.
func WithWork(ctx context.Context, begin func() func()) context.Context {
	return context.WithValue(ctx, workKey{}, begin)
}
func Go(ctx context.Context, fn func()) {
	done := func() {}
	if begin, ok := ctx.Value(workKey{}).(func() func()); ok {
		done = begin()
	}
	go func() { defer done(); fn() }()
}

// Scope adds local ownership while preserving the resource's late-work tracker.
func Scope(ctx context.Context) (context.Context, *Group) {
	group := &Group{}
	parent, _ := ctx.Value(workKey{}).(func() func())
	scoped := WithWork(ctx, func() func() {
		finish := func() {}
		if parent != nil {
			finish = parent()
		}
		group.Add()
		return func() { group.Done(); finish() }
	})
	return scoped, group
}

// AfterFunc owns a cancellation callback even when stopping it races its start.
func AfterFunc(ctx context.Context, fn func()) func() bool {
	done := func() {}
	if begin, ok := ctx.Value(workKey{}).(func() func()); ok {
		done = begin()
	}
	stop := context.AfterFunc(ctx, func() { defer done(); fn() })
	return func() bool {
		if stop() {
			done()
			return true
		}
		return false
	}
}
