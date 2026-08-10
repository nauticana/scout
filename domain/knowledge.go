package domain

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

// KnowledgeQuery contains one tenant-scoped semantic retrieval request.
type KnowledgeQuery struct {
	TenantContext    TenantContext
	RequestID        string
	ConversationID   string
	KnowledgeBaseID  string
	KnowledgeVersion string
	Query            []byte
	TopK             int
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
	Matches []KnowledgeMatch
	Usage   Usage
}

// Embedding contains a provider-neutral vector representation.
type Embedding struct {
	Values []float32
	Usage  Usage
}
