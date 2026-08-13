// Package knowledge implements the shared retrieval and ingestion services behind contract/knowledge.go.
package knowledge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BatchingEmbedder implements EmbeddingGateway by coalescing calls into
// bounded per-tenant provider batches; tenants never share a batch, and a
// response-count mismatch fails the whole batch rather than misassigning one.
type BatchingEmbedder struct {
	Batcher contract.BatchEmbedder
	// MaxBatch flushes on size; default 16.
	MaxBatch int
	// MaxWait flushes on age of the oldest queued item; default 25ms.
	MaxWait time.Duration
	// Timeout bounds one provider batch call; default 30s.
	Timeout time.Duration

	mu      sync.Mutex
	pending map[int64]*embedBatch
}

var _ contract.EmbeddingGateway = (*BatchingEmbedder)(nil)

type embedBatch struct {
	tenant  domain.TenantContext
	items   []*embedItem
	timer   *time.Timer
	flushed bool
}

type embedItem struct {
	content  []byte
	canceled bool
	result   chan embedResult
}

type embedResult struct {
	embedding domain.Embedding
	err       error
}

func (embedder *BatchingEmbedder) limits() (int, time.Duration, time.Duration) {
	maxBatch, maxWait, timeout := embedder.MaxBatch, embedder.MaxWait, embedder.Timeout
	if maxBatch <= 0 {
		maxBatch = 16
	}
	if maxWait <= 0 {
		maxWait = 25 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return maxBatch, maxWait, timeout
}

// Embed queues one input and returns its own embedding from the shared batch.
func (embedder *BatchingEmbedder) Embed(ctx context.Context, tenant domain.TenantContext, content []byte) (domain.Embedding, error) {
	if embedder.Batcher == nil {
		return domain.Embedding{}, fmt.Errorf("batching embedder: batcher is required")
	}
	if tenant.TenantID <= 0 || len(content) == 0 {
		return domain.Embedding{}, fmt.Errorf("%w: tenant and content are required", domain.ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return domain.Embedding{}, err
	}
	maxBatch, maxWait, _ := embedder.limits()
	item := &embedItem{content: append([]byte(nil), content...), result: make(chan embedResult, 1)}

	embedder.mu.Lock()
	if embedder.pending == nil {
		embedder.pending = make(map[int64]*embedBatch)
	}
	batch := embedder.pending[tenant.TenantID]
	if batch == nil {
		batch = &embedBatch{tenant: tenant}
		embedder.pending[tenant.TenantID] = batch
		batch.timer = time.AfterFunc(maxWait, func() { embedder.flush(tenant.TenantID, batch) })
	}
	batch.items = append(batch.items, item)
	var live []*embedItem
	if len(batch.items) >= maxBatch {
		live = embedder.claimLocked(tenant.TenantID, batch)
	}
	embedder.mu.Unlock()

	if live != nil {
		embedder.run(batch.tenant, live)
	}
	select {
	case result := <-item.result:
		return result.embedding, result.err
	case <-ctx.Done():
		// Withdraw before the flush claims the batch; a claimed item just expires.
		embedder.mu.Lock()
		item.canceled = true
		embedder.mu.Unlock()
		return domain.Embedding{}, ctx.Err()
	}
}

// flush claims the batch exactly once and answers every non-canceled caller.
func (embedder *BatchingEmbedder) flush(tenantID int64, batch *embedBatch) {
	embedder.mu.Lock()
	if batch.flushed {
		embedder.mu.Unlock()
		return
	}
	live := embedder.claimLocked(tenantID, batch)
	embedder.mu.Unlock()
	embedder.run(batch.tenant, live)
}

func (embedder *BatchingEmbedder) claimLocked(tenantID int64, batch *embedBatch) []*embedItem {
	batch.flushed = true
	batch.timer.Stop()
	if embedder.pending[tenantID] == batch {
		delete(embedder.pending, tenantID)
	}
	live := make([]*embedItem, 0, len(batch.items))
	for _, item := range batch.items {
		if !item.canceled {
			live = append(live, item)
		}
	}
	return live
}

func (embedder *BatchingEmbedder) run(tenant domain.TenantContext, live []*embedItem) {
	if len(live) == 0 {
		return
	}
	_, _, timeout := embedder.limits()

	contents := make([][]byte, len(live))
	for i, item := range live {
		contents[i] = item.content
	}
	// The batch outlives any single caller, so it runs on its own bounded context.
	callCtx, cancel := context.WithTimeout(context.Background(), timeout)
	embeddings, err := embedder.Batcher.EmbedBatch(callCtx, tenant, contents)
	cancel()
	if err == nil && len(embeddings) != len(live) {
		err = fmt.Errorf("%w: batch returned %d embeddings for %d inputs", domain.ErrValidation, len(embeddings), len(live))
	}
	for i, item := range live {
		result := embedResult{err: err}
		if err == nil {
			result.embedding = embeddings[i]
		}
		item.result <- result
	}
}
