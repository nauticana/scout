package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// KnowledgeIngestor coordinates document loading, embedding, and indexing.
type KnowledgeIngestor interface {
	// Ingest publishes one immutable document into a knowledge-base version.
	Ingest(ctx context.Context, document domain.KnowledgeDocument) error
}

// KnowledgeDocumentStore persists authorized source and chunk content.
type KnowledgeDocumentStore interface {
	// Put stores immutable tenant knowledge content.
	Put(ctx context.Context, document domain.KnowledgeDocument, content []byte) error
	// Get returns authorized immutable tenant knowledge content.
	Get(ctx context.Context, tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) ([]byte, error)
}

// EmbeddingGateway is the governed entry point for embedding generation.
type EmbeddingGateway interface {
	// Embed creates a vector under tenant quotas and provider controls.
	Embed(ctx context.Context, tenant domain.TenantContext, content []byte) (domain.Embedding, error)
}

// BatchEmbedder embeds several inputs of one tenant in a single provider call.
type BatchEmbedder interface {
	// EmbedBatch returns exactly one embedding per input, in input order.
	EmbedBatch(ctx context.Context, tenant domain.TenantContext, contents [][]byte) ([]domain.Embedding, error)
}

// KnowledgeVectorIndex stores and searches tenant-partitioned embeddings.
type KnowledgeVectorIndex interface {
	// Index stores one document embedding under an immutable knowledge version.
	Index(ctx context.Context, document domain.KnowledgeDocument, embedding domain.Embedding) error
	// Search returns ranked authorized matches for a knowledge query.
	Search(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error)
}

// KnowledgeRetriever governs semantic retrieval for a runtime knowledge step.
type KnowledgeRetriever interface {
	// Retrieve returns ranked tenant-scoped knowledge with bounded latency.
	Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error)
}

// KnowledgeReranker reorders already-authorized candidates.
type KnowledgeReranker interface {
	Rerank(ctx context.Context, query domain.KnowledgeQuery, matches []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error)
}
