package knowledge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

// timerHooks counts scheduled and stopped flush timers so leaks are asserted without goroutine counts.
type timerHooks struct {
	mu        sync.Mutex
	scheduled int
	fired     int
}

func (hooks *timerHooks) afterFunc(wait time.Duration, flush func()) *time.Timer {
	hooks.mu.Lock()
	hooks.scheduled++
	hooks.mu.Unlock()
	return time.AfterFunc(wait, func() {
		hooks.mu.Lock()
		hooks.fired++
		hooks.mu.Unlock()
		flush()
	})
}

func (hooks *timerHooks) counts() (int, int) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return hooks.scheduled, hooks.fired
}

func TestBatchingEmbedderBlocksUntilCancel(t *testing.T) {
	embedder := &BatchingEmbedder{Batcher: &batchRecorder{}, MaxBatch: 100, MaxWait: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := embedder.Embed(ctx, domain.TenantContext{TenantID: 1}, []byte("x"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("returned before cancel: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if err := embedder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchingEmbedderProviderFailureFailsEveryCaller(t *testing.T) {
	failure := errors.New("provider down")
	recorder := &batchRecorder{respond: func([][]byte) ([]domain.Embedding, error) { return nil, failure }}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 3, MaxWait: time.Hour}
	defer embedder.Close()

	errs := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x"))
			errs <- err
		}()
	}
	for range 3 {
		if err := <-errs; !errors.Is(err, failure) {
			t.Fatalf("caller = %v", err)
		}
	}
}

func TestBatchingEmbedderRejectsEmptyInputAndInvalidConfig(t *testing.T) {
	cases := []struct {
		name     string
		embedder *BatchingEmbedder
		content  []byte
		tenant   int64
		wantErr  error
	}{
		{name: "empty content", embedder: &BatchingEmbedder{Batcher: &batchRecorder{}}, tenant: 1, wantErr: domain.ErrValidation},
		{name: "no tenant", embedder: &BatchingEmbedder{Batcher: &batchRecorder{}}, content: []byte("x"), wantErr: domain.ErrValidation},
		{name: "no batcher", embedder: &BatchingEmbedder{}, tenant: 1, content: []byte("x")},
		{name: "negative batch", embedder: &BatchingEmbedder{Batcher: &batchRecorder{}, MaxBatch: -1}, tenant: 1, content: []byte("x")},
		{name: "negative wait", embedder: &BatchingEmbedder{Batcher: &batchRecorder{}, MaxWait: -time.Second}, tenant: 1, content: []byte("x")},
		{name: "negative timeout", embedder: &BatchingEmbedder{Batcher: &batchRecorder{}, Timeout: -time.Second}, tenant: 1, content: []byte("x")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.embedder.Embed(context.Background(), domain.TenantContext{TenantID: tc.tenant}, tc.content)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestBatchingEmbedderCloseIsIdempotentAndLeavesNoTimer(t *testing.T) {
	hooks := &timerHooks{}
	embedder := &BatchingEmbedder{Batcher: &batchRecorder{}, MaxBatch: 100, MaxWait: time.Hour, AfterFunc: hooks.afterFunc}

	queued := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x"))
		queued <- err
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for {
		if scheduled, _ := hooks.counts(); scheduled == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flush timer was never scheduled")
		}
		time.Sleep(time.Millisecond)
	}

	if err := embedder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := embedder.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if err := <-queued; !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("queued caller = %v", err)
	}
	if _, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("after close = %v", err)
	}
	// Close stopped the pending timer, so nothing fired and no flush goroutine outlived the embedder.
	if scheduled, fired := hooks.counts(); scheduled != 1 || fired != 0 {
		t.Fatalf("timers scheduled=%d fired=%d", scheduled, fired)
	}
}

func TestBatchingEmbedderCloseWaitsForRunningFlush(t *testing.T) {
	release := make(chan struct{})
	recorder := &batchRecorder{respond: func(contents [][]byte) ([]domain.Embedding, error) {
		<-release
		return make([]domain.Embedding, len(contents)), nil
	}}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 100, MaxWait: 5 * time.Millisecond}
	embedded := make(chan error, 1)
	go func() {
		_, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x"))
		embedded <- err
	}()
	closed := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		closed <- embedder.Close()
	}()
	select {
	case err := <-closed:
		t.Fatalf("close returned while a flush was running: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-embedded; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
