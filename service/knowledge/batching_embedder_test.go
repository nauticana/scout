package knowledge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type batchRecorder struct {
	mu      sync.Mutex
	batches [][][]byte
	tenants []int64
	respond func(contents [][]byte) ([]domain.Embedding, error)
}

func (recorder *batchRecorder) EmbedBatch(_ context.Context, tenant domain.TenantContext, contents [][]byte) ([]domain.Embedding, error) {
	recorder.mu.Lock()
	recorder.batches = append(recorder.batches, contents)
	recorder.tenants = append(recorder.tenants, tenant.TenantID)
	recorder.mu.Unlock()
	if recorder.respond != nil {
		return recorder.respond(contents)
	}
	embeddings := make([]domain.Embedding, len(contents))
	for i, content := range contents {
		embeddings[i] = domain.Embedding{Values: []float32{float32(len(content))}}
	}
	return embeddings, nil
}

func TestBatchingEmbedderCoalescesAndCorrelates(t *testing.T) {
	recorder := &batchRecorder{}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 3, MaxWait: time.Hour}
	tenant := domain.TenantContext{TenantID: 1}

	var wg sync.WaitGroup
	results := make([]domain.Embedding, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content := make([]byte, i+1)
			embedding, err := embedder.Embed(context.Background(), tenant, content)
			if err != nil {
				t.Error(err)
			}
			results[i] = embedding
		}()
	}
	wg.Wait()
	if len(recorder.batches) != 1 || len(recorder.batches[0]) != 3 {
		t.Fatalf("batches = %v", recorder.batches)
	}
	// Each caller received the embedding of its own input length.
	for i, embedding := range results {
		if len(embedding.Values) != 1 || embedding.Values[0] != float32(i+1) {
			t.Fatalf("result %d = %+v", i, embedding)
		}
	}
}

func TestBatchingEmbedderFlushesOnMaxWait(t *testing.T) {
	recorder := &batchRecorder{}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 100, MaxWait: 10 * time.Millisecond}
	embedding, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x"))
	if err != nil || embedding.Values[0] != 1 {
		t.Fatalf("embed = %+v, %v", embedding, err)
	}
}

func TestBatchingEmbedderNeverMixesTenants(t *testing.T) {
	recorder := &batchRecorder{}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 1, MaxWait: time.Hour}
	for tenantID := int64(1); tenantID <= 2; tenantID++ {
		if _, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: tenantID}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.batches) != 2 || recorder.tenants[0] == recorder.tenants[1] {
		t.Fatalf("tenants = %v", recorder.tenants)
	}
}

func TestBatchingEmbedderNeverExceedsMaxBatch(t *testing.T) {
	recorder := &batchRecorder{}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 4, MaxWait: 5 * time.Millisecond}
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x")); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for _, batch := range recorder.batches {
		if len(batch) > 4 {
			t.Fatalf("batch size = %d", len(batch))
		}
	}
}

func TestBatchingEmbedderCountMismatchFailsBatch(t *testing.T) {
	recorder := &batchRecorder{respond: func(contents [][]byte) ([]domain.Embedding, error) {
		return make([]domain.Embedding, len(contents)+1), nil
	}}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 1, MaxWait: time.Hour}
	if _, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("x")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mismatch = %v", err)
	}
}

func TestBatchingEmbedderCanceledCallerLeavesBatch(t *testing.T) {
	recorder := &batchRecorder{}
	embedder := &BatchingEmbedder{Batcher: recorder, MaxBatch: 100, MaxWait: 30 * time.Millisecond}
	canceled, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	if _, err := embedder.Embed(canceled, domain.TenantContext{TenantID: 1}, []byte("dropped")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	// The later caller still gets its batch; the canceled item was withdrawn.
	if _, err := embedder.Embed(context.Background(), domain.TenantContext{TenantID: 1}, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for _, batch := range recorder.batches {
		for _, content := range batch {
			if string(content) == "dropped" {
				t.Fatal("canceled item was flushed")
			}
		}
	}
}
