package singleflight

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoCoalescesConcurrentLoads(t *testing.T) {
	var group Group[string, int]
	var loads atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		if loads.Add(1) == 1 {
			close(started)
		}
		<-release
		return 42, nil
	}

	var wg sync.WaitGroup
	results := make([]int, 8)
	join := func(i int) {
		defer wg.Done()
		value, err := group.Do(context.Background(), "k", load)
		if err != nil {
			t.Error(err)
		}
		results[i] = value
	}
	wg.Add(1)
	go join(0)
	<-started
	// The leader is blocked in load, so every join below finds its flight.
	for i := 1; i < len(results); i++ {
		wg.Add(1)
		go join(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if loads.Load() != 1 {
		t.Fatalf("loads = %d", loads.Load())
	}
	for _, value := range results {
		if value != 42 {
			t.Fatalf("results = %v", results)
		}
	}
}

func TestDoCanceledWaiterDoesNotCancelLoad(t *testing.T) {
	var group Group[string, string]
	started := make(chan struct{})
	release := make(chan struct{})
	loadCtxErr := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := group.Do(ctx, "k", func(loadCtx context.Context) (string, error) {
		close(started)
		<-release
		loadCtxErr <- loadCtx.Err()
		return "v", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter err = %v", err)
	}

	close(release)
	if err := <-loadCtxErr; err != nil {
		t.Fatalf("load context was canceled: %v", err)
	}
}
