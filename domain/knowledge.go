package domain

import "time"

// KnowledgeDocument identifies immutable source content for ingestion.
type KnowledgeDocument struct {
	TenantContext    TenantContext
	KnowledgeBaseID  string
	KnowledgeVersion string
	DocumentID       string
	SourceURI        string
	ContentDigest    string
	MediaType        string
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
	TopK               int
	Budget             time.Duration
}

// KnowledgeMatch contains one authorized chunk returned by retrieval.
type KnowledgeMatch struct {
	DocumentID string
	ChunkNo    int
	Content    []byte
	SourceURI  string
	Score      float64
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
)

// Embedding contains a provider-neutral vector representation.
type Embedding struct {
	Values []float32
	Usage  Usage
}
