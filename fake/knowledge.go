package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// KnowledgeVectorIndex contains configurable index callbacks.
type KnowledgeVectorIndex struct {
	IndexFunc  func(context.Context, []domain.ChunkEmbedding) error
	RemoveFunc func(context.Context, int64, string, string, string) error
	SearchFunc func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error)
}

// Index invokes IndexFunc.
func (index *KnowledgeVectorIndex) Index(ctx context.Context, items []domain.ChunkEmbedding) error {
	return index.IndexFunc(ctx, items)
}

// Remove invokes RemoveFunc.
func (index *KnowledgeVectorIndex) Remove(ctx context.Context, tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) error {
	return index.RemoveFunc(ctx, tenantID, knowledgeBaseID, knowledgeVersion, documentID)
}

// Search invokes SearchFunc.
func (index *KnowledgeVectorIndex) Search(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	return index.SearchFunc(ctx, query)
}

// KnowledgeRetriever contains a configurable retrieval callback.
type KnowledgeRetriever struct {
	RetrieveFunc func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error)
}

// Retrieve invokes RetrieveFunc.
func (retriever *KnowledgeRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	return retriever.RetrieveFunc(ctx, query)
}

// KnowledgeReranker contains a configurable rerank callback.
type KnowledgeReranker struct {
	RerankFunc func(context.Context, domain.KnowledgeQuery, []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error)
}

// Rerank invokes RerankFunc.
func (reranker *KnowledgeReranker) Rerank(ctx context.Context, query domain.KnowledgeQuery, matches []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error) {
	return reranker.RerankFunc(ctx, query, matches)
}

// EmbeddingGateway contains a configurable embed callback.
type EmbeddingGateway struct {
	EmbedFunc func(context.Context, domain.TenantContext, []byte) (domain.Embedding, error)
}

// Embed invokes EmbedFunc.
func (gateway *EmbeddingGateway) Embed(ctx context.Context, tenant domain.TenantContext, content []byte) (domain.Embedding, error) {
	return gateway.EmbedFunc(ctx, tenant, content)
}

// BatchEmbedder contains a configurable batch embed callback.
type BatchEmbedder struct {
	EmbedBatchFunc func(context.Context, domain.TenantContext, [][]byte) ([]domain.Embedding, error)
}

// EmbedBatch invokes EmbedBatchFunc.
func (embedder *BatchEmbedder) EmbedBatch(ctx context.Context, tenant domain.TenantContext, contents [][]byte) ([]domain.Embedding, error) {
	return embedder.EmbedBatchFunc(ctx, tenant, contents)
}

var _ contract.KnowledgeVectorIndex = (*KnowledgeVectorIndex)(nil)
var _ contract.KnowledgeRetriever = (*KnowledgeRetriever)(nil)
var _ contract.KnowledgeReranker = (*KnowledgeReranker)(nil)
var _ contract.EmbeddingGateway = (*EmbeddingGateway)(nil)
var _ contract.BatchEmbedder = (*BatchEmbedder)(nil)
