package domain

import "time"

// KnowledgeDocument identifies immutable source content for ingestion.
type KnowledgeDocument struct {
	TenantContext    TenantContext
	KnowledgeBaseID  string
	KnowledgeVersion string
	DocumentID       string
	SourceURI        string
	// SourceVersion is the upstream revision the content was captured from.
	SourceVersion string
	ContentDigest string
	MediaType     string
	// Entitlements are the authorization attributes retrieval must match; nil fails closed.
	Entitlements []byte
	// RedactionPolicyVersion selects the column-level policy applied before embedding.
	RedactionPolicyVersion string
}

// DecodedDocument is media-neutral text with the structure the chunker preserves.
type DecodedDocument struct {
	Text     []byte
	Sections []DocumentSection
}

// DocumentSection is one structural unit (heading, paragraph, table) with byte offsets into Text.
type DocumentSection struct {
	Kind        string
	Title       string
	StartOffset int
	EndOffset   int
	Depth       int
}

// KnowledgeChunk is one embeddable, authorization-scoped slice of a document.
// ChunkID is deterministic over tenant, document, source version, chunker
// version, and position, so re-ingesting identical content is idempotent.
type KnowledgeChunk struct {
	TenantContext          TenantContext
	KnowledgeBaseID        string
	KnowledgeVersion       string
	DocumentID             string
	SourceVersion          string
	ChunkerVersion         string
	ChunkNo                int
	ChunkID                string
	StartOffset            int
	EndOffset              int
	Content                []byte
	ContentDigest          string
	TokenCount             int
	Entitlements           []byte
	RedactionPolicyVersion string
}

// ChunkEmbedding pairs a chunk with the vector produced for it.
type ChunkEmbedding struct {
	Chunk     KnowledgeChunk
	Embedding Embedding
}

// KnowledgeQuery is one retrieval request; the index applies Entitlements inside the search, never as a post-filter.
type KnowledgeQuery struct {
	TenantContext      TenantContext
	RequestID          string
	ConversationID     string
	KnowledgeBaseID    string
	KnowledgeVersion   string
	Principal          string
	Entitlements       []byte
	EntitlementsDigest string
	Query              []byte
	// Embedding is the query vector when the caller already embedded it; nil lets the index embed.
	Embedding []float32
	TopK      int
	Budget    time.Duration
}

// KnowledgeMatch contains one authorized chunk returned by retrieval; embeddings never leave the index.
type KnowledgeMatch struct {
	DocumentID    string
	ChunkNo       int
	ChunkID       string
	Content       []byte
	SourceURI     string
	SourceVersion string
	StartOffset   int
	EndOffset     int
	Score         float64
}

// KnowledgeResult contains ranked knowledge matches for a runtime step.
type KnowledgeResult struct {
	Matches      []KnowledgeMatch
	Usage        Usage
	Degradations []string
}

const (
	KnowledgeDegradationPartialRetrieval = "partial_retrieval"
	KnowledgeDegradationRerankerFailed   = "reranker_failed"
	KnowledgeDegradationBudgetExhausted  = "budget_exhausted"
	KnowledgeDegradationCacheBypassed    = "cache_bypassed"
)

// Embedding contains a provider-neutral vector representation.
type Embedding struct {
	Values []float32
	Usage  Usage
}

// IngestBatch is one bounded ingestion request for a knowledge version.
type IngestBatch struct {
	TenantContext    TenantContext
	KnowledgeBaseID  string
	KnowledgeVersion string
	Documents        []KnowledgeDocument
}

// IngestItemResult is the correlated outcome for one document of a batch.
type IngestItemResult struct {
	DocumentID string
	// IdempotencyKey is tenant+version+document+content digest; equal keys are replays.
	IdempotencyKey string
	ChunkCount     int
	Usage          Usage
	// Failure is empty on success; Terminal marks a document that must not be retried as-is.
	Failure  string
	Terminal bool
}

// IngestBatchResult correlates every document with its outcome.
type IngestBatchResult struct {
	Items []IngestItemResult
	Usage Usage
}

// KnowledgeDocumentManifest is the active version pointer for one document.
type KnowledgeDocumentManifest struct {
	TenantID         int64
	KnowledgeBaseID  string
	DocumentID       string
	ActiveVersion    string
	SourceVersion    string
	ContentDigest    string
	ChunkerVersion   string
	Tombstoned       bool
	TombstonedAt     time.Time
	ActivatedAt      time.Time
	SupersededChunks int
}

// SourceChangeOp is what happened to a source object upstream.
type SourceChangeOp string

const (
	SourceUpserted SourceChangeOp = "upsert"
	SourceDeleted  SourceChangeOp = "delete"
)

// SourceChangeEvent is one CDC/outbox record for a knowledge source object.
type SourceChangeEvent struct {
	TenantID        int64
	KnowledgeBaseID string
	ObjectID        string
	SourceVersion   string
	Op              SourceChangeOp
	Entitlements    []byte
	OccurredAt      time.Time
}

// KnowledgeFreshnessReport summarizes reconciliation of a knowledge base.
type KnowledgeFreshnessReport struct {
	TenantID        int64
	KnowledgeBaseID string
	ActiveVersion   string
	FreshnessLag    time.Duration
	OrphanChunks    int
	Tombstones      int
	CheckedAt       time.Time
}
