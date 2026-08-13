// Package singleflight coalesces concurrent loads of the same key.
package singleflight

import (
	"context"
	"sync"
)

type flight[V any] struct {
	done  chan struct{}
	value V
	err   error
}

// Group runs one load per key; waiters share its result.
type Group[K comparable, V any] struct {
	mu      sync.Mutex
	flights map[K]*flight[V]
}

// Do joins or starts the load for key; the load runs detached from any single caller's cancellation.
func (g *Group[K, V]) Do(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	g.mu.Lock()
	if g.flights == nil {
		g.flights = make(map[K]*flight[V])
	}
	f := g.flights[key]
	if f == nil {
		f = &flight[V]{done: make(chan struct{})}
		g.flights[key] = f
		go func() {
			// The result is published before done closes, giving waiters a happens-before edge.
			f.value, f.err = load(context.WithoutCancel(ctx))
			g.mu.Lock()
			delete(g.flights, key)
			g.mu.Unlock()
			close(f.done)
		}()
	}
	g.mu.Unlock()

	select {
	case <-f.done:
		return f.value, f.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}
