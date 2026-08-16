package knowledge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qPgVectorUpsertChunk            = "scout_knowledge_pgvector_upsert_chunk"
	qPgVectorRemoveDocument         = "scout_knowledge_pgvector_remove_document"
	qPgVectorTombstoneDocument      = "scout_knowledge_pgvector_tombstone_document"
	qPgVectorSearchText             = "scout_knowledge_pgvector_search_text"
	qPgVectorSearchVector           = "scout_knowledge_pgvector_search_vector"
	qPgVectorEnsureHNSWIndex        = "scout_knowledge_pgvector_ensure_hnsw_index"
	qPgVectorEnsureTextIndex        = "scout_knowledge_pgvector_ensure_text_index"
	qPgVectorEnsureEntitlementIndex = "scout_knowledge_pgvector_ensure_entitlement_index"
)

// pgVectorMaxDimensions is the widest vector pgvector's HNSW access method indexes.
const pgVectorMaxDimensions = 2000

// pgVectorDimensionsToken is replaced by the validated dimension count; the
// HNSW index is an expression index, so the query must spell the same
// vector(N) cast or the planner falls back to a sequential scan.
const pgVectorDimensionsToken = "{dims}"

const pgVectorMatchSelect = `
SELECT v.document_id, v.chunk_no, v.chunk_id, d.source_uri, v.source_version, v.start_offset, v.end_offset,`

const pgVectorMatchFrom = `
  FROM knowledge_chunk_vector v
  JOIN knowledge_document d
    ON d.tenant_id = v.tenant_id AND d.knowledge_base_id = v.knowledge_base_id
   AND d.knowledge_version = v.knowledge_version AND d.document_id = v.document_id`

// pgVectorScope is the authorization predicate both legs compile into the
// query: tenant partition, immutable version, tombstones from the row and the
// document manifest, and any-of entitlement labels via jsonb ?| (written ??|
// so keel's placeholder rewriter keeps it literal).
const pgVectorScope = `
 WHERE v.tenant_id = ? AND v.knowledge_base_id = ? AND v.knowledge_version = ?
   AND v.tombstoned = FALSE
   AND EXISTS (SELECT 1
                 FROM knowledge_document_manifest m
                WHERE m.tenant_id = v.tenant_id AND m.knowledge_base_id = v.knowledge_base_id
                  AND m.document_id = v.document_id AND m.active_version = v.knowledge_version
                  AND m.tombstoned = FALSE)
   AND v.entitlements ??| ARRAY(SELECT jsonb_array_elements_text(?::jsonb))`

var pgVectorQueries = map[string]string{
	qPgVectorUpsertChunk: `
INSERT INTO knowledge_chunk_vector (tenant_id, knowledge_base_id, knowledge_version, document_id, chunk_no,
                                    chunk_id, embedding, dimensions, content_tsv, entitlements,
                                    source_version, start_offset, end_offset, tombstoned)
VALUES (?, ?, ?, ?, ?, ?, ?::vector, ?, to_tsvector(?::regconfig, ?), ?::jsonb, ?, ?, ?, FALSE)
ON CONFLICT (tenant_id, knowledge_base_id, knowledge_version, document_id, chunk_no) DO UPDATE
   SET embedding = EXCLUDED.embedding, dimensions = EXCLUDED.dimensions, content_tsv = EXCLUDED.content_tsv,
       entitlements = EXCLUDED.entitlements, source_version = EXCLUDED.source_version,
       start_offset = EXCLUDED.start_offset, end_offset = EXCLUDED.end_offset
 WHERE knowledge_chunk_vector.chunk_id = EXCLUDED.chunk_id
RETURNING chunk_id`,
	qPgVectorRemoveDocument: `
DELETE FROM knowledge_chunk_vector
 WHERE tenant_id = ? AND knowledge_base_id = ? AND knowledge_version = ? AND document_id = ?`,
	qPgVectorTombstoneDocument: `
UPDATE knowledge_chunk_vector
   SET tombstoned = TRUE
 WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ? AND tombstoned = FALSE`,
	qPgVectorSearchText: `
WITH q AS (SELECT websearch_to_tsquery(?::regconfig, ?) AS tsq)` +
		pgVectorMatchSelect + `
       ts_rank_cd(v.content_tsv, q.tsq) AS score` +
		pgVectorMatchFrom + `
 CROSS JOIN q` +
		pgVectorScope + `
   AND v.content_tsv @@ q.tsq
 ORDER BY score DESC, v.document_id, v.chunk_no
 LIMIT ?`,
	qPgVectorEnsureTextIndex: `
CREATE INDEX IF NOT EXISTS knowledge_chunk_vector_tsv_ix ON knowledge_chunk_vector USING gin (content_tsv)`,
	qPgVectorEnsureEntitlementIndex: `
CREATE INDEX IF NOT EXISTS knowledge_chunk_vector_entitlements_ix ON knowledge_chunk_vector USING gin (entitlements)`,
}

// pgVectorDimensionQueries are instantiated per validated dimension count.
// ORDER BY carries only the distance so the HNSW index serves the scan;
// deterministic tie-breaking happens in Go on the fetched rows.
var pgVectorDimensionQueries = map[string]string{
	qPgVectorSearchVector: pgVectorMatchSelect + `
       1 - ((v.embedding::vector({dims})) <=> ?::vector({dims})) AS score` +
		pgVectorMatchFrom +
		pgVectorScope + `
   AND v.dimensions = {dims}
 ORDER BY (v.embedding::vector({dims})) <=> ?::vector({dims})
 LIMIT ?`,
	qPgVectorEnsureHNSWIndex: `
CREATE INDEX IF NOT EXISTS knowledge_chunk_vector_hnsw_{dims}_ix ON knowledge_chunk_vector
 USING hnsw ((embedding::vector({dims})) vector_cosine_ops)
 WHERE dimensions = {dims}`,
}

func pgVectorQueriesFor(dimensions int) map[string]string {
	queries := make(map[string]string, len(pgVectorQueries)+len(pgVectorDimensionQueries))
	for name, sql := range pgVectorQueries {
		queries[name] = sql
	}
	dims := strconv.Itoa(dimensions)
	for name, sql := range pgVectorDimensionQueries {
		queries[name] = strings.ReplaceAll(sql, pgVectorDimensionsToken, dims)
	}
	return queries
}

// PgVectorIndex is the reference KnowledgeVectorIndex over knowledge_chunk_vector:
// pgvector cosine search and tsvector keyword search under one compiled
// authorization scope. Deployment creates the vector extension; the same
// instance serves knowledge bases of different embedding widths because
// EnsureIndexes builds one partial HNSW index per dimension count.
type PgVectorIndex struct {
	DB keelport.DatabaseRepository
	// Embedder embeds the query text when KnowledgeQuery.Embedding is empty.
	Embedder contract.EmbeddingGateway
	// TextSearchConfig is the regconfig for to_tsvector and websearch_to_tsquery; default "simple".
	TextSearchConfig string
	// Overfetch multiplies the SQL LIMIT so MaxChunksPerDocument can trim without starving TopK; default 1.
	Overfetch int
	// MaxChunksPerDocument caps matches per document after fetch; 0 keeps every chunk.
	MaxChunksPerDocument int

	mu       sync.Mutex
	services map[int]keelport.QueryService
}

var _ contract.KnowledgeVectorIndex = (*PgVectorIndex)(nil)

func (index *PgVectorIndex) validate() error {
	if index.DB == nil {
		return fmt.Errorf("pgvector index: database is required")
	}
	if index.Overfetch < 0 || index.MaxChunksPerDocument < 0 {
		return fmt.Errorf("%w: overfetch and max chunks per document cannot be negative", domain.ErrValidation)
	}
	return nil
}

func (index *PgVectorIndex) textSearchConfig() string {
	if strings.TrimSpace(index.TextSearchConfig) == "" {
		return "simple"
	}
	return index.TextSearchConfig
}

func (index *PgVectorIndex) overfetch() int {
	if index.Overfetch <= 0 {
		return 1
	}
	return index.Overfetch
}

// service returns the query service compiled for one dimension count (0 for dimension-free queries).
func (index *PgVectorIndex) service(ctx context.Context, dimensions int) (keelport.QueryService, error) {
	if err := index.validate(); err != nil {
		return nil, err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if qs, ok := index.services[dimensions]; ok {
		return qs, nil
	}
	if index.services == nil {
		index.services = make(map[int]keelport.QueryService)
	}
	qs := index.DB.GetQueryService(ctx, pgVectorQueriesFor(dimensions))
	if qs == nil {
		return nil, fmt.Errorf("pgvector index: query service is required")
	}
	index.services[dimensions] = qs
	return qs, nil
}

func validateDimensions(dimensions int) error {
	if dimensions <= 0 || dimensions > pgVectorMaxDimensions {
		return fmt.Errorf("%w: embedding dimensions must be within 1..%d, got %d", domain.ErrValidation, pgVectorMaxDimensions, dimensions)
	}
	return nil
}

// EnsureIndexes creates the HNSW index for one embedding width plus the GIN
// indexes; idempotent, meant for deployment or first ingestion of a model.
func (index *PgVectorIndex) EnsureIndexes(ctx context.Context, dimensions int) error {
	if err := validateDimensions(dimensions); err != nil {
		return err
	}
	qs, err := index.service(ctx, dimensions)
	if err != nil {
		return err
	}
	for _, name := range []string{qPgVectorEnsureHNSWIndex, qPgVectorEnsureTextIndex, qPgVectorEnsureEntitlementIndex} {
		if _, err := qs.Query(ctx, name); err != nil {
			return fmt.Errorf("pgvector ensure index %s: %w", name, err)
		}
	}
	return nil
}

// Index upserts a batch of chunk embeddings in one transaction; a chunk whose
// identity differs from the row already stored under its position is a conflict.
func (index *PgVectorIndex) Index(ctx context.Context, items []domain.ChunkEmbedding) error {
	if err := index.validate(); err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("%w: at least one chunk embedding is required", domain.ErrValidation)
	}
	rows := make([][]any, 0, len(items))
	for i, item := range items {
		args, err := upsertArgs(item, index.textSearchConfig())
		if err != nil {
			return fmt.Errorf("pgvector index item %d: %w", i, err)
		}
		rows = append(rows, args)
	}
	tx, err := index.DB.BeginTx(ctx, pgVectorQueries)
	if err != nil {
		return fmt.Errorf("pgvector index: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	for i, args := range rows {
		result, err := tx.Query(ctx, qPgVectorUpsertChunk, args...)
		if err != nil {
			return fmt.Errorf("pgvector index item %d: %w", i, err)
		}
		if result == nil || len(result.Rows) == 0 {
			chunk := items[i].Chunk
			return fmt.Errorf("%w: chunk %s/%d of document %s already indexed with a different identity", domain.ErrConflict, chunk.KnowledgeVersion, chunk.ChunkNo, chunk.DocumentID)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgvector index: commit: %w", err)
	}
	committed = true
	return nil
}

func upsertArgs(item domain.ChunkEmbedding, textSearchConfig string) ([]any, error) {
	chunk := item.Chunk
	if chunk.TenantContext.TenantID <= 0 || strings.TrimSpace(chunk.KnowledgeBaseID) == "" ||
		strings.TrimSpace(chunk.KnowledgeVersion) == "" || strings.TrimSpace(chunk.DocumentID) == "" {
		return nil, fmt.Errorf("%w: chunk tenant, knowledge base, version, and document are required", domain.ErrValidation)
	}
	if chunk.ChunkNo < 0 || len(chunk.ChunkID) != 64 {
		return nil, fmt.Errorf("%w: chunk requires a non-negative number and a 64-hex chunk id", domain.ErrValidation)
	}
	if chunk.StartOffset < 0 || chunk.EndOffset < chunk.StartOffset {
		return nil, fmt.Errorf("%w: chunk offsets are invalid", domain.ErrValidation)
	}
	if strings.TrimSpace(chunk.SourceVersion) == "" {
		return nil, fmt.Errorf("%w: chunk source version is required", domain.ErrValidation)
	}
	labels, err := ParseEntitlements(chunk.Entitlements)
	if err != nil {
		return nil, fmt.Errorf("%w: chunk entitlements: %v", domain.ErrValidation, err)
	}
	entitlements, err := EncodeEntitlements(labels)
	if err != nil {
		return nil, err
	}
	if err := validateDimensions(len(item.Embedding.Values)); err != nil {
		return nil, err
	}
	vector, err := formatVector(item.Embedding.Values)
	if err != nil {
		return nil, err
	}
	return []any{
		chunk.TenantContext.TenantID, chunk.KnowledgeBaseID, chunk.KnowledgeVersion, chunk.DocumentID, chunk.ChunkNo,
		chunk.ChunkID, vector, len(item.Embedding.Values), textSearchConfig, string(chunk.Content), string(entitlements),
		chunk.SourceVersion, chunk.StartOffset, chunk.EndOffset,
	}, nil
}

// formatVector renders the pgvector text literal; the column stays untyped, so width is validated here.
func formatVector(values []float32) (string, error) {
	var builder strings.Builder
	builder.Grow(len(values) * 10)
	builder.WriteByte('[')
	for i, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return "", fmt.Errorf("%w: embedding contains a non-finite value", domain.ErrValidation)
		}
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	builder.WriteByte(']')
	return builder.String(), nil
}

// Remove drops every chunk of one document version.
func (index *PgVectorIndex) Remove(ctx context.Context, tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) error {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(knowledgeVersion) == "" || strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("%w: tenant, knowledge base, version, and document are required", domain.ErrValidation)
	}
	qs, err := index.service(ctx, 0)
	if err != nil {
		return err
	}
	if _, err := qs.Query(ctx, qPgVectorRemoveDocument, tenantID, knowledgeBaseID, knowledgeVersion, documentID); err != nil {
		return fmt.Errorf("pgvector remove document %s: %w", documentID, err)
	}
	return nil
}

// Tombstone hides every version of a document from search before Remove reclaims the rows.
func (index *PgVectorIndex) Tombstone(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) error {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("%w: tenant, knowledge base, and document are required", domain.ErrValidation)
	}
	qs, err := index.service(ctx, 0)
	if err != nil {
		return err
	}
	if _, err := qs.Query(ctx, qPgVectorTombstoneDocument, tenantID, knowledgeBaseID, documentID); err != nil {
		return fmt.Errorf("pgvector tombstone document %s: %w", documentID, err)
	}
	return nil
}

// Search is the vector leg; use PgTextRetriever for the keyword leg.
func (index *PgVectorIndex) Search(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	return index.SearchVector(ctx, query)
}

// SearchVector ranks authorized chunks by cosine similarity to the query embedding.
func (index *PgVectorIndex) SearchVector(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	if err := index.validate(); err != nil {
		return domain.KnowledgeResult{}, err
	}
	if _, err := authorizeQuery(query); err != nil {
		return domain.KnowledgeResult{}, err
	}
	ctx, cancel := budgetContext(ctx, query)
	defer cancel()
	embedding, usage, err := index.queryEmbedding(ctx, query)
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	vector, err := formatVector(embedding)
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	qs, err := index.service(ctx, len(embedding))
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	limit, err := index.limit(query.TopK)
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	result, err := qs.Query(ctx, qPgVectorSearchVector,
		vector, query.TenantContext.TenantID, query.KnowledgeBaseID, query.KnowledgeVersion, string(query.Entitlements), vector, limit)
	if err != nil {
		return domain.KnowledgeResult{}, fmt.Errorf("pgvector search: %w", err)
	}
	matches, err := decodeMatches(result)
	if err != nil {
		return domain.KnowledgeResult{}, fmt.Errorf("pgvector search: %w", err)
	}
	return domain.KnowledgeResult{Matches: index.trim(matches, query.TopK), Usage: usage}, nil
}

// SearchText ranks authorized chunks by ts_rank_cd against the websearch-parsed query text.
func (index *PgVectorIndex) SearchText(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	if err := index.validate(); err != nil {
		return domain.KnowledgeResult{}, err
	}
	if _, err := authorizeQuery(query); err != nil {
		return domain.KnowledgeResult{}, err
	}
	text := strings.TrimSpace(string(query.Query))
	if text == "" {
		return domain.KnowledgeResult{}, fmt.Errorf("%w: query text is required for keyword search", domain.ErrValidation)
	}
	ctx, cancel := budgetContext(ctx, query)
	defer cancel()
	qs, err := index.service(ctx, 0)
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	limit, err := index.limit(query.TopK)
	if err != nil {
		return domain.KnowledgeResult{}, err
	}
	result, err := qs.Query(ctx, qPgVectorSearchText,
		index.textSearchConfig(), text, query.TenantContext.TenantID, query.KnowledgeBaseID, query.KnowledgeVersion, string(query.Entitlements), limit)
	if err != nil {
		return domain.KnowledgeResult{}, fmt.Errorf("pgvector text search: %w", err)
	}
	matches, err := decodeMatches(result)
	if err != nil {
		return domain.KnowledgeResult{}, fmt.Errorf("pgvector text search: %w", err)
	}
	return domain.KnowledgeResult{Matches: index.trim(matches, query.TopK)}, nil
}

func (index *PgVectorIndex) queryEmbedding(ctx context.Context, query domain.KnowledgeQuery) ([]float32, domain.Usage, error) {
	if len(query.Embedding) > 0 {
		return query.Embedding, domain.Usage{}, validateDimensions(len(query.Embedding))
	}
	if index.Embedder == nil {
		return nil, domain.Usage{}, fmt.Errorf("%w: query embedding or an embedder is required", domain.ErrValidation)
	}
	if len(strings.TrimSpace(string(query.Query))) == 0 {
		return nil, domain.Usage{}, fmt.Errorf("%w: query text is required to embed", domain.ErrValidation)
	}
	embedding, err := index.Embedder.Embed(ctx, query.TenantContext, query.Query)
	if err != nil {
		return nil, domain.Usage{}, fmt.Errorf("pgvector search: embed query: %w", err)
	}
	if err := validateDimensions(len(embedding.Values)); err != nil {
		return nil, domain.Usage{}, err
	}
	return embedding.Values, embedding.Usage, nil
}

func (index *PgVectorIndex) limit(topK int) (int, error) {
	overfetch := index.overfetch()
	if topK > int(^uint(0)>>1)/overfetch {
		return 0, fmt.Errorf("%w: TopK overflows overfetch", domain.ErrValidation)
	}
	return topK * overfetch, nil
}

// trim orders ties deterministically, applies the per-document cap, and cuts to TopK.
func (index *PgVectorIndex) trim(matches []domain.KnowledgeMatch, topK int) []domain.KnowledgeMatch {
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		if matches[i].DocumentID != matches[j].DocumentID {
			return matches[i].DocumentID < matches[j].DocumentID
		}
		return matches[i].ChunkNo < matches[j].ChunkNo
	})
	perDocument := make(map[string]int, len(matches))
	kept := make([]domain.KnowledgeMatch, 0, min(len(matches), topK))
	for _, match := range matches {
		if len(kept) == topK {
			break
		}
		if index.MaxChunksPerDocument > 0 && perDocument[match.DocumentID] >= index.MaxChunksPerDocument {
			continue
		}
		perDocument[match.DocumentID]++
		kept = append(kept, match)
	}
	return kept
}

func budgetContext(ctx context.Context, query domain.KnowledgeQuery) (context.Context, context.CancelFunc) {
	if query.Budget > 0 {
		return context.WithTimeout(ctx, query.Budget)
	}
	return context.WithCancel(ctx)
}

func decodeMatches(result *keelmodel.QueryResult) ([]domain.KnowledgeMatch, error) {
	if result == nil {
		return nil, nil
	}
	matches := make([]domain.KnowledgeMatch, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 8 {
			return nil, fmt.Errorf("expected 8 columns, got %d", len(row))
		}
		score, ok := common.AsFloat64OK(row[7])
		if !ok {
			return nil, fmt.Errorf("invalid score %q", common.AsString(row[7]))
		}
		matches = append(matches, domain.KnowledgeMatch{
			DocumentID:    common.AsString(row[0]),
			ChunkNo:       int(common.AsInt64(row[1])),
			ChunkID:       common.AsString(row[2]),
			SourceURI:     common.AsString(row[3]),
			SourceVersion: common.AsString(row[4]),
			StartOffset:   int(common.AsInt64(row[5])),
			EndOffset:     int(common.AsInt64(row[6])),
			Score:         score,
		})
	}
	return matches, nil
}
