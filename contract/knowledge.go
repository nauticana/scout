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

// BulkKnowledgeIngestor ingests a bounded batch with correlated per-document results.
type BulkKnowledgeIngestor interface {
	KnowledgeIngestor
	// IngestBatch returns one item per document; a systemic error cancels the batch.
	IngestBatch(ctx context.Context, batch domain.IngestBatch) (domain.IngestBatchResult, error)
}

// SourceLoader fetches raw source bytes for a document; the pipeline verifies the digest.
type SourceLoader interface {
	Load(ctx context.Context, document domain.KnowledgeDocument) ([]byte, error)
}

// MediaDecoder turns raw source bytes into structure-preserving text.
type MediaDecoder interface {
	Decode(ctx context.Context, document domain.KnowledgeDocument, raw []byte) (domain.DecodedDocument, error)
}

// Chunker splits a decoded document deterministically; Version pins the algorithm in every chunk id.
type Chunker interface {
	Version() string
	Chunk(ctx context.Context, document domain.KnowledgeDocument, decoded domain.DecodedDocument) ([]domain.KnowledgeChunk, error)
}

// ChunkRedactor applies the column-level policy version and returns the derivative chunk that is embedded.
type ChunkRedactor interface {
	Redact(ctx context.Context, chunk domain.KnowledgeChunk) (domain.KnowledgeChunk, error)
}

// KnowledgeDocumentStore persists authorized source and chunk content.
type KnowledgeDocumentStore interface {
	// Put stores immutable tenant knowledge content.
	Put(ctx context.Context, document domain.KnowledgeDocument, content []byte) error
	// Get returns authorized immutable tenant knowledge content.
	Get(ctx context.Context, tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) ([]byte, error)
}

// KnowledgeChunkStore persists chunk content in object storage and returns the reference to commit.
type KnowledgeChunkStore interface {
	PutChunk(ctx context.Context, chunk domain.KnowledgeChunk) (domain.ObjectRef, error)
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

// KnowledgeVectorIndex stores and searches tenant-partitioned chunk embeddings.
type KnowledgeVectorIndex interface {
	// Index stores chunk embeddings under an immutable knowledge version; a batch is all-or-nothing.
	Index(ctx context.Context, items []domain.ChunkEmbedding) error
	// Remove drops every chunk of a document version so tombstones and GC can reclaim it.
	Remove(ctx context.Context, tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) error
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

// KnowledgeManifestStore owns the active-version pointer and tombstone per document.
type KnowledgeManifestStore interface {
	// Activate switches the document to a fully built version and reports the superseded one.
	Activate(ctx context.Context, manifest domain.KnowledgeDocumentManifest) (previousVersion string, err error)
	// Tombstone marks a document deleted so retrieval excludes it before bulk cleanup runs.
	Tombstone(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) error
	// Get returns the manifest for one document.
	Get(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) (domain.KnowledgeDocumentManifest, error)
	// ListSuperseded returns manifests whose old chunks still await garbage collection.
	ListSuperseded(ctx context.Context, tenantID int64, knowledgeBaseID string, limit int) ([]domain.KnowledgeDocumentManifest, error)
}

// KnowledgeVersionAliaser atomically repoints a knowledge base at a validated version generation.
type KnowledgeVersionAliaser interface {
	// Swap makes the version active only if it matches the expected current one.
	Swap(ctx context.Context, tenantID int64, knowledgeBaseID, expectedVersion, newVersion string) error
	// Active returns the version retrieval must read.
	Active(ctx context.Context, tenantID int64, knowledgeBaseID string) (string, error)
}

// SourceChangeSource delivers upstream change events for ingestion.
type SourceChangeSource interface {
	// Poll returns up to limit undelivered events in occurrence order.
	Poll(ctx context.Context, tenantID int64, knowledgeBaseID string, limit int) ([]domain.SourceChangeEvent, error)
	// Ack marks events applied so they are not redelivered.
	Ack(ctx context.Context, events []domain.SourceChangeEvent) error
}

// KnowledgeReconciler reports freshness lag and orphaned chunks for a knowledge base.
type KnowledgeReconciler interface {
	Reconcile(ctx context.Context, tenantID int64, knowledgeBaseID string) (domain.KnowledgeFreshnessReport, error)
}
