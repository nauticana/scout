package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// SourceLoader contains a configurable load callback.
type SourceLoader struct {
	LoadFunc func(context.Context, domain.KnowledgeDocument) ([]byte, error)
}

// Load invokes LoadFunc.
func (loader *SourceLoader) Load(ctx context.Context, document domain.KnowledgeDocument) ([]byte, error) {
	return loader.LoadFunc(ctx, document)
}

// MediaDecoder contains a configurable decode callback.
type MediaDecoder struct {
	DecodeFunc func(context.Context, domain.KnowledgeDocument, []byte) (domain.DecodedDocument, error)
}

// Decode invokes DecodeFunc.
func (decoder *MediaDecoder) Decode(ctx context.Context, document domain.KnowledgeDocument, raw []byte) (domain.DecodedDocument, error) {
	return decoder.DecodeFunc(ctx, document, raw)
}

// Chunker contains a configurable chunk callback and a fixed version.
type Chunker struct {
	ChunkerVersion string
	ChunkFunc      func(context.Context, domain.KnowledgeDocument, domain.DecodedDocument) ([]domain.KnowledgeChunk, error)
}

// Version returns ChunkerVersion.
func (chunker *Chunker) Version() string { return chunker.ChunkerVersion }

// Chunk invokes ChunkFunc.
func (chunker *Chunker) Chunk(ctx context.Context, document domain.KnowledgeDocument, decoded domain.DecodedDocument) ([]domain.KnowledgeChunk, error) {
	return chunker.ChunkFunc(ctx, document, decoded)
}

// ChunkRedactor contains a configurable redact callback.
type ChunkRedactor struct {
	RedactFunc func(context.Context, domain.KnowledgeChunk) (domain.KnowledgeChunk, error)
}

// Redact invokes RedactFunc.
func (redactor *ChunkRedactor) Redact(ctx context.Context, chunk domain.KnowledgeChunk) (domain.KnowledgeChunk, error) {
	return redactor.RedactFunc(ctx, chunk)
}

// KnowledgeChunkStore contains a configurable put callback.
type KnowledgeChunkStore struct {
	PutChunkFunc func(context.Context, domain.KnowledgeChunk) (domain.ObjectRef, error)
}

// PutChunk invokes PutChunkFunc.
func (store *KnowledgeChunkStore) PutChunk(ctx context.Context, chunk domain.KnowledgeChunk) (domain.ObjectRef, error) {
	return store.PutChunkFunc(ctx, chunk)
}

// KnowledgeManifestStore contains configurable manifest callbacks.
type KnowledgeManifestStore struct {
	ActivateFunc       func(context.Context, domain.KnowledgeDocumentManifest) (string, error)
	TombstoneFunc      func(context.Context, int64, string, string) error
	GetFunc            func(context.Context, int64, string, string) (domain.KnowledgeDocumentManifest, error)
	ListSupersededFunc func(context.Context, int64, string, int) ([]domain.KnowledgeDocumentManifest, error)
}

// Activate invokes ActivateFunc.
func (store *KnowledgeManifestStore) Activate(ctx context.Context, manifest domain.KnowledgeDocumentManifest) (string, error) {
	return store.ActivateFunc(ctx, manifest)
}

// Tombstone invokes TombstoneFunc.
func (store *KnowledgeManifestStore) Tombstone(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) error {
	return store.TombstoneFunc(ctx, tenantID, knowledgeBaseID, documentID)
}

// Get invokes GetFunc.
func (store *KnowledgeManifestStore) Get(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) (domain.KnowledgeDocumentManifest, error) {
	return store.GetFunc(ctx, tenantID, knowledgeBaseID, documentID)
}

// ListSuperseded invokes ListSupersededFunc.
func (store *KnowledgeManifestStore) ListSuperseded(ctx context.Context, tenantID int64, knowledgeBaseID string, limit int) ([]domain.KnowledgeDocumentManifest, error) {
	return store.ListSupersededFunc(ctx, tenantID, knowledgeBaseID, limit)
}

// KnowledgeVersionAliaser contains configurable alias callbacks.
type KnowledgeVersionAliaser struct {
	SwapFunc   func(context.Context, int64, string, string, string) error
	ActiveFunc func(context.Context, int64, string) (string, error)
}

// Swap invokes SwapFunc.
func (aliaser *KnowledgeVersionAliaser) Swap(ctx context.Context, tenantID int64, knowledgeBaseID, expectedVersion, newVersion string) error {
	return aliaser.SwapFunc(ctx, tenantID, knowledgeBaseID, expectedVersion, newVersion)
}

// Active invokes ActiveFunc.
func (aliaser *KnowledgeVersionAliaser) Active(ctx context.Context, tenantID int64, knowledgeBaseID string) (string, error) {
	return aliaser.ActiveFunc(ctx, tenantID, knowledgeBaseID)
}

// SourceChangeSource contains configurable poll and ack callbacks.
type SourceChangeSource struct {
	PollFunc func(context.Context, int64, string, int) ([]domain.SourceChangeEvent, error)
	AckFunc  func(context.Context, []domain.SourceChangeEvent) error
}

// Poll invokes PollFunc.
func (source *SourceChangeSource) Poll(ctx context.Context, tenantID int64, knowledgeBaseID string, limit int) ([]domain.SourceChangeEvent, error) {
	return source.PollFunc(ctx, tenantID, knowledgeBaseID, limit)
}

// Ack invokes AckFunc.
func (source *SourceChangeSource) Ack(ctx context.Context, events []domain.SourceChangeEvent) error {
	return source.AckFunc(ctx, events)
}

var _ contract.SourceLoader = (*SourceLoader)(nil)
var _ contract.MediaDecoder = (*MediaDecoder)(nil)
var _ contract.Chunker = (*Chunker)(nil)
var _ contract.ChunkRedactor = (*ChunkRedactor)(nil)
var _ contract.KnowledgeChunkStore = (*KnowledgeChunkStore)(nil)
var _ contract.KnowledgeManifestStore = (*KnowledgeManifestStore)(nil)
var _ contract.KnowledgeVersionAliaser = (*KnowledgeVersionAliaser)(nil)
var _ contract.SourceChangeSource = (*SourceChangeSource)(nil)
