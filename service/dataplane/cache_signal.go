package dataplane

import (
	"context"
	"sync"

	"github.com/nauticana/keel/cache"
)

// cacheSignal relays per-key wake-ups over one cache pub/sub channel, so a process
// holds one subscription however many keys it waits on. Signals are lossy by
// design: every waiter also polls.
type cacheSignal struct {
	cache   cache.CacheService
	channel string

	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
	stop    context.CancelFunc
}

func (signal *cacheSignal) notify(ctx context.Context, key string) error {
	return signal.cache.Publish(ctx, signal.channel, key)
}

// wait registers for wake-ups of key; release must be called.
func (signal *cacheSignal) wait(key string) (<-chan struct{}, func(), error) {
	signal.mu.Lock()
	defer signal.mu.Unlock()
	if signal.stop == nil {
		ctx, stop := context.WithCancel(context.Background())
		messages, err := signal.cache.Subscribe(ctx, signal.channel)
		if err != nil {
			stop()
			return nil, nil, err
		}
		signal.stop = stop
		go signal.relay(messages, stop)
	}
	if signal.waiters == nil {
		signal.waiters = make(map[string]map[chan struct{}]struct{})
	}
	if signal.waiters[key] == nil {
		signal.waiters[key] = make(map[chan struct{}]struct{})
	}
	wake := make(chan struct{}, 1)
	signal.waiters[key][wake] = struct{}{}
	return wake, func() {
		signal.mu.Lock()
		defer signal.mu.Unlock()
		delete(signal.waiters[key], wake)
		if len(signal.waiters[key]) == 0 {
			delete(signal.waiters, key)
		}
	}, nil
}

// relay ends when the backend drops the subscription; the next wait resubscribes.
func (signal *cacheSignal) relay(messages <-chan string, stop context.CancelFunc) {
	for key := range messages {
		signal.mu.Lock()
		for wake := range signal.waiters[key] {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		signal.mu.Unlock()
	}
	stop()
	signal.mu.Lock()
	signal.stop = nil
	signal.mu.Unlock()
}

func (signal *cacheSignal) close() {
	signal.mu.Lock()
	defer signal.mu.Unlock()
	if signal.stop != nil {
		signal.stop()
	}
}
