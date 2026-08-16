package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qIngestFindDocument   = "scout_knowledge_ingest_find_document"
	qIngestInsertDocument = "scout_knowledge_ingest_insert_document"
	qIngestInsertChunk    = "scout_knowledge_ingest_insert_chunk"
)

var ingestQueries = map[string]string{
	qIngestFindDocument: `
SELECT content_digest,
       (SELECT COUNT(*) FROM knowledge_chunk chunk
         WHERE chunk.tenant_id = doc.tenant_id AND chunk.knowledge_base_id = doc.knowledge_base_id
           AND chunk.knowledge_version = doc.knowledge_version AND chunk.document_id = doc.document_id)
  FROM knowledge_document doc
 WHERE tenant_id = ? AND knowledge_base_id = ? AND knowledge_version = ? AND document_id = ?`,
	qIngestInsertDocument: `
INSERT INTO knowledge_document (tenant_id, knowledge_base_id, knowledge_version, document_id, source_uri, content_digest, media_type)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
	qIngestInsertChunk: `
INSERT INTO knowledge_chunk (tenant_id, knowledge_base_id, knowledge_version, document_id, chunk_no, content_uri, content_digest, vector_ref, token_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
}

// IngestPipeline is a bounded synchronous batch executor: prepare (load,
// verify, decode, chunk, redact) → embed → publish (chunk store, vector
// index, then relational rows in one transaction, then manifest activation).
// Stages hand off through bounded channels so a slow index applies real
// backpressure; a systemic failure cancels the batch, an isolated bad
// document only records a terminal item result.
type IngestPipeline struct {
	Loader     contract.SourceLoader
	Decoder    contract.MediaDecoder
	Chunker    contract.Chunker
	Embedder   contract.EmbeddingGateway
	ChunkStore contract.KnowledgeChunkStore
	Index      contract.KnowledgeVectorIndex
	DB         keelport.DatabaseRepository
	// Redactor derives the embedded chunk from the source chunk; nil embeds the source chunk.
	Redactor contract.ChunkRedactor
	// Manifests switches the document's active pointer after publish; nil leaves activation to the caller (generation rebuilds).
	Manifests contract.KnowledgeManifestStore
	// PrepareWorkers, EmbedWorkers, PublishWorkers bound per-stage concurrency; default 1 each.
	PrepareWorkers int
	EmbedWorkers   int
	PublishWorkers int
	// EmbedFanOut bounds concurrent embeddings within one document; default 4.
	EmbedFanOut int
	// QueueDepth is the buffered handoff between stages; default 0 (rendezvous).
	QueueDepth int
	// IsSystemic classifies a failure as batch-fatal; nil treats circuit-open and degraded dependencies as systemic. Context errors are always systemic.
	IsSystemic func(error) bool

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.BulkKnowledgeIngestor = (*IngestPipeline)(nil)

// ChunkID is the deterministic chunk identity: SHA-256 over tenant, knowledge
// base, document, source version, chunker version, and chunk position.
func ChunkID(tenantID int64, knowledgeBaseID, documentID, sourceVersion, chunkerVersion string, chunkNo int) string {
	return sha256Fields(strconv.FormatInt(tenantID, 10), knowledgeBaseID, documentID, sourceVersion, chunkerVersion, strconv.Itoa(chunkNo))
}

// IngestIdempotencyKey identifies one immutable ingestion of a document.
func IngestIdempotencyKey(tenantID int64, knowledgeBaseID, knowledgeVersion, documentID, contentDigest string) string {
	return sha256Fields(strconv.FormatInt(tenantID, 10), knowledgeBaseID, knowledgeVersion, documentID, contentDigest)
}

func sha256Fields(fields ...string) string {
	hash := sha256.New()
	for i, field := range fields {
		if i > 0 {
			_, _ = hash.Write([]byte{'|'})
		}
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sha256Bytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

type ingestJob struct {
	index      int
	document   domain.KnowledgeDocument
	key        string
	chunks     []domain.KnowledgeChunk
	embeddings []domain.ChunkEmbedding
	usage      domain.Usage
	chunkCount int
	replay     bool
	published  bool
	err        error
	terminal   bool
}

func (job *ingestJob) fail(err error, terminal bool) {
	job.err, job.terminal = err, terminal
}

func (pipeline *IngestPipeline) validate() error {
	switch {
	case pipeline.Loader == nil, pipeline.Decoder == nil, pipeline.Chunker == nil, pipeline.Embedder == nil,
		pipeline.ChunkStore == nil, pipeline.Index == nil, pipeline.DB == nil:
		return fmt.Errorf("ingest pipeline: loader, decoder, chunker, embedder, chunk store, index, and database are required")
	case pipeline.PrepareWorkers < 0, pipeline.EmbedWorkers < 0, pipeline.PublishWorkers < 0, pipeline.EmbedFanOut < 0, pipeline.QueueDepth < 0:
		return fmt.Errorf("%w: ingest pipeline worker counts and queue depth cannot be negative", domain.ErrValidation)
	}
	return nil
}

func (pipeline *IngestPipeline) init(ctx context.Context) {
	pipeline.once.Do(func() { pipeline.qs = pipeline.DB.GetQueryService(ctx, ingestQueries) })
}

func atLeastOne(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}

func (pipeline *IngestPipeline) systemic(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if pipeline.IsSystemic != nil {
		return pipeline.IsSystemic(err)
	}
	return errors.Is(err, domain.ErrCircuitOpen) || errors.Is(err, domain.ErrDegraded)
}

// terminal failures do not change on retry of the same document version.
func terminalFailure(err error) bool {
	return errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrForbidden)
}

// Ingest publishes one document; a terminal or transient item failure is returned as its error.
func (pipeline *IngestPipeline) Ingest(ctx context.Context, document domain.KnowledgeDocument) error {
	jobs, err := pipeline.run(ctx, domain.IngestBatch{
		TenantContext:    document.TenantContext,
		KnowledgeBaseID:  document.KnowledgeBaseID,
		KnowledgeVersion: document.KnowledgeVersion,
		Documents:        []domain.KnowledgeDocument{document},
	})
	if err != nil {
		return err
	}
	return jobs[0].err
}

// IngestBatch returns one correlated item per document in input order. On a
// systemic failure the partial result is returned together with the error;
// items that never published carry that failure as non-terminal.
func (pipeline *IngestPipeline) IngestBatch(ctx context.Context, batch domain.IngestBatch) (domain.IngestBatchResult, error) {
	jobs, err := pipeline.run(ctx, batch)
	if jobs == nil {
		return domain.IngestBatchResult{}, err
	}
	result := domain.IngestBatchResult{Items: make([]domain.IngestItemResult, len(jobs))}
	for i, job := range jobs {
		item := domain.IngestItemResult{DocumentID: job.document.DocumentID, IdempotencyKey: job.key, ChunkCount: job.chunkCount, Usage: job.usage}
		if job.err != nil {
			item.Failure, item.Terminal = job.err.Error(), job.terminal
		}
		result.Items[i] = item
		result.Usage = addUsage(result.Usage, job.usage)
	}
	return result, err
}

func addUsage(total, usage domain.Usage) domain.Usage {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.ToolCalls += usage.ToolCalls
	total.CostMinorUnits += usage.CostMinorUnits
	if total.Currency == "" {
		total.Currency = usage.Currency
	}
	return total
}

func (pipeline *IngestPipeline) run(ctx context.Context, batch domain.IngestBatch) ([]*ingestJob, error) {
	if err := pipeline.validate(); err != nil {
		return nil, err
	}
	if batch.TenantContext.TenantID <= 0 || strings.TrimSpace(batch.KnowledgeBaseID) == "" || strings.TrimSpace(batch.KnowledgeVersion) == "" {
		return nil, fmt.Errorf("%w: tenant, knowledge base, and knowledge version are required", domain.ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobs := make([]*ingestJob, len(batch.Documents))
	seen := make(map[string]struct{}, len(batch.Documents))
	for i, document := range batch.Documents {
		if _, duplicate := seen[document.DocumentID]; duplicate {
			return nil, fmt.Errorf("%w: document %q appears twice in the batch", domain.ErrValidation, document.DocumentID)
		}
		seen[document.DocumentID] = struct{}{}
		jobs[i] = &ingestJob{index: i, document: document}
		if err := pipeline.validateDocument(batch, &jobs[i].document); err != nil {
			jobs[i].fail(err, true)
		}
	}
	if len(jobs) == 0 {
		return jobs, nil
	}
	pipeline.init(ctx)

	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var failOnce sync.Once
	var batchErr error
	fail := func(err error) {
		failOnce.Do(func() {
			batchErr = err
			cancel()
		})
	}

	prepared := make(chan *ingestJob, pipeline.QueueDepth)
	embedded := make(chan *ingestJob, pipeline.QueueDepth)
	done := make(chan *ingestJob, pipeline.QueueDepth)
	feed := make(chan *ingestJob)
	go func() {
		defer close(feed)
		for _, job := range jobs {
			if job.err != nil {
				continue
			}
			select {
			case feed <- job:
			case <-batchCtx.Done():
				return
			}
		}
	}()
	pipeline.stage(batchCtx, atLeastOne(pipeline.PrepareWorkers), feed, prepared, fail, pipeline.prepare)
	pipeline.stage(batchCtx, atLeastOne(pipeline.EmbedWorkers), prepared, embedded, fail, pipeline.embed)
	pipeline.stage(batchCtx, atLeastOne(pipeline.PublishWorkers), embedded, done, fail, pipeline.publish)
	// The last stage closes done only after every worker upstream has exited.
	for range done {
	}
	if batchErr == nil {
		batchErr = ctx.Err()
	}
	if batchErr != nil {
		for _, job := range jobs {
			if job.err == nil && !job.published {
				job.fail(fmt.Errorf("batch canceled: %w", batchErr), false)
			}
		}
	}
	return jobs, batchErr
}

func (pipeline *IngestPipeline) validateDocument(batch domain.IngestBatch, document *domain.KnowledgeDocument) error {
	if document.TenantContext.TenantID == 0 {
		document.TenantContext = batch.TenantContext
	}
	if document.KnowledgeBaseID == "" {
		document.KnowledgeBaseID = batch.KnowledgeBaseID
	}
	if document.KnowledgeVersion == "" {
		document.KnowledgeVersion = batch.KnowledgeVersion
	}
	switch {
	case document.TenantContext.TenantID != batch.TenantContext.TenantID,
		document.KnowledgeBaseID != batch.KnowledgeBaseID, document.KnowledgeVersion != batch.KnowledgeVersion:
		return fmt.Errorf("%w: document does not belong to the batch tenant, knowledge base, and version", domain.ErrValidation)
	case strings.TrimSpace(document.DocumentID) == "", strings.TrimSpace(document.SourceURI) == "":
		return fmt.Errorf("%w: document id and source URI are required", domain.ErrValidation)
	case !isSHA256Hex(document.ContentDigest):
		return fmt.Errorf("%w: content digest must be SHA-256 hex", domain.ErrValidation)
	case document.Entitlements == nil:
		return fmt.Errorf("%w: entitlements are required; nil fails closed", domain.ErrValidation)
	}
	document.ContentDigest = strings.ToLower(document.ContentDigest)
	return nil
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// stage runs workers over in and closes out once every worker has exited.
func (pipeline *IngestPipeline) stage(ctx context.Context, workers int, in <-chan *ingestJob, out chan<- *ingestJob, fail func(error), work func(context.Context, *ingestJob)) {
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for job := range in {
				if ctx.Err() != nil {
					continue
				}
				work(ctx, job)
				if job.err != nil {
					if pipeline.systemic(job.err) {
						fail(job.err)
					}
					continue
				}
				select {
				case out <- job:
				case <-ctx.Done():
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
}

func (pipeline *IngestPipeline) prepare(ctx context.Context, job *ingestJob) {
	document := job.document
	job.key = IngestIdempotencyKey(document.TenantContext.TenantID, document.KnowledgeBaseID, document.KnowledgeVersion, document.DocumentID, document.ContentDigest)
	existing, err := pipeline.qs.Query(ctx, qIngestFindDocument, document.TenantContext.TenantID, document.KnowledgeBaseID, document.KnowledgeVersion, document.DocumentID)
	if err != nil {
		job.fail(fmt.Errorf("find document: %w", err), false)
		return
	}
	if len(existing.Rows) > 0 {
		if common.AsString(existing.Rows[0][0]) != document.ContentDigest {
			job.fail(fmt.Errorf("%w: document %q is already published in version %q with different content", domain.ErrConflict, document.DocumentID, document.KnowledgeVersion), true)
			return
		}
		job.replay, job.chunkCount = true, int(common.AsInt64(existing.Rows[0][1]))
		return
	}
	raw, err := pipeline.Loader.Load(ctx, document)
	if err != nil {
		job.fail(fmt.Errorf("load source: %w", err), terminalFailure(err))
		return
	}
	if digest := sha256Bytes(raw); digest != document.ContentDigest {
		job.fail(fmt.Errorf("%w: source digest %s does not match declared %s", domain.ErrValidation, digest, document.ContentDigest), true)
		return
	}
	decoded, err := pipeline.Decoder.Decode(ctx, document, raw)
	if err != nil {
		job.fail(fmt.Errorf("decode source: %w", err), !pipeline.systemic(err))
		return
	}
	chunks, err := pipeline.Chunker.Chunk(ctx, document, decoded)
	if err != nil {
		job.fail(fmt.Errorf("chunk document: %w", err), !pipeline.systemic(err))
		return
	}
	if len(chunks) == 0 {
		job.fail(fmt.Errorf("%w: document %q produced no chunks", domain.ErrValidation, document.DocumentID), true)
		return
	}
	chunkerVersion := pipeline.Chunker.Version()
	for i := range chunks {
		chunk := &chunks[i]
		if len(chunk.Content) == 0 {
			job.fail(fmt.Errorf("%w: chunk %d of document %q is empty", domain.ErrValidation, i, document.DocumentID), true)
			return
		}
		chunk.TenantContext = document.TenantContext
		chunk.KnowledgeBaseID, chunk.KnowledgeVersion, chunk.DocumentID = document.KnowledgeBaseID, document.KnowledgeVersion, document.DocumentID
		chunk.SourceVersion, chunk.ChunkerVersion, chunk.ChunkNo = document.SourceVersion, chunkerVersion, i
		chunk.Entitlements, chunk.RedactionPolicyVersion = document.Entitlements, document.RedactionPolicyVersion
		chunk.ChunkID = ChunkID(document.TenantContext.TenantID, document.KnowledgeBaseID, document.DocumentID, document.SourceVersion, chunkerVersion, i)
		if pipeline.Redactor != nil {
			redacted, err := pipeline.Redactor.Redact(ctx, *chunk)
			if err != nil {
				job.fail(fmt.Errorf("redact chunk %d: %w", i, err), terminalFailure(err))
				return
			}
			redacted.ChunkID, redacted.ChunkNo = chunk.ChunkID, chunk.ChunkNo
			*chunk = redacted
		}
		chunk.ContentDigest = sha256Bytes(chunk.Content)
		if chunk.TokenCount <= 0 {
			chunk.TokenCount = 1
		}
	}
	job.chunks = chunks
}

func (pipeline *IngestPipeline) embed(ctx context.Context, job *ingestJob) {
	if job.replay {
		return
	}
	docCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	embeddings := make([]domain.ChunkEmbedding, len(job.chunks))
	failures := make([]error, len(job.chunks))
	semaphore := make(chan struct{}, atLeastOne(pipeline.EmbedFanOut))
	var wg sync.WaitGroup
	for i, chunk := range job.chunks {
		select {
		case semaphore <- struct{}{}:
		case <-docCtx.Done():
			failures[i] = docCtx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			embedding, err := pipeline.Embedder.Embed(docCtx, chunk.TenantContext, chunk.Content)
			if err != nil {
				failures[i] = err
				cancel()
				return
			}
			if len(embedding.Values) == 0 {
				failures[i] = fmt.Errorf("%w: empty embedding for chunk %d", domain.ErrValidation, i)
				cancel()
				return
			}
			embeddings[i] = domain.ChunkEmbedding{Chunk: chunk, Embedding: embedding}
		}()
	}
	wg.Wait()
	for i, err := range failures {
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, context.Canceled) {
			err = ctxErr
		}
		job.fail(fmt.Errorf("embed chunk %d: %w", i, err), terminalFailure(err))
		return
	}
	for _, embedding := range embeddings {
		job.usage = addUsage(job.usage, embedding.Embedding.Usage)
	}
	job.embeddings = embeddings
}

func (pipeline *IngestPipeline) publish(ctx context.Context, job *ingestJob) {
	document := job.document
	if !job.replay {
		refs := make([]domain.ObjectRef, len(job.embeddings))
		for i, embedding := range job.embeddings {
			ref, err := pipeline.ChunkStore.PutChunk(ctx, embedding.Chunk)
			if err != nil {
				job.fail(fmt.Errorf("store chunk %d: %w", i, err), terminalFailure(err))
				return
			}
			if ref.URI == "" || ref.Digest != embedding.Chunk.ContentDigest {
				job.fail(fmt.Errorf("%w: chunk store returned reference %q digest %q for chunk %d", domain.ErrValidation, ref.URI, ref.Digest, i), false)
				return
			}
			refs[i] = ref
		}
		if err := pipeline.Index.Index(ctx, job.embeddings); err != nil {
			job.fail(fmt.Errorf("index document %q: %w", document.DocumentID, err), terminalFailure(err))
			return
		}
		if err := pipeline.persist(ctx, job, refs); err != nil {
			// The vectors are live without rows: reclaim them so retrieval never sees an unpublished chunk.
			if removeErr := pipeline.Index.Remove(ctx, document.TenantContext.TenantID, document.KnowledgeBaseID, document.KnowledgeVersion, document.DocumentID); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("reconcile index for document %q: %w", document.DocumentID, removeErr))
			}
			job.fail(err, false)
			return
		}
		job.chunkCount = len(job.embeddings)
	}
	if pipeline.Manifests != nil {
		if _, err := pipeline.Manifests.Activate(ctx, domain.KnowledgeDocumentManifest{
			TenantID: document.TenantContext.TenantID, KnowledgeBaseID: document.KnowledgeBaseID, DocumentID: document.DocumentID,
			ActiveVersion: document.KnowledgeVersion, SourceVersion: document.SourceVersion,
			ContentDigest: document.ContentDigest, ChunkerVersion: pipeline.Chunker.Version(),
		}); err != nil {
			job.fail(fmt.Errorf("activate manifest for document %q: %w", document.DocumentID, err), false)
			return
		}
	}
	job.published = true
}

func (pipeline *IngestPipeline) persist(ctx context.Context, job *ingestJob, refs []domain.ObjectRef) error {
	document := job.document
	tx, err := pipeline.DB.BeginTx(ctx, ingestQueries)
	if err != nil {
		return fmt.Errorf("publish document %q: begin: %w", document.DocumentID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	if _, err = tx.Query(ctx, qIngestInsertDocument, document.TenantContext.TenantID, document.KnowledgeBaseID, document.KnowledgeVersion,
		document.DocumentID, document.SourceURI, document.ContentDigest, document.MediaType); err != nil {
		return fmt.Errorf("publish document %q: %w", document.DocumentID, err)
	}
	for i, embedding := range job.embeddings {
		chunk := embedding.Chunk
		if _, err = tx.Query(ctx, qIngestInsertChunk, chunk.TenantContext.TenantID, chunk.KnowledgeBaseID, chunk.KnowledgeVersion, chunk.DocumentID,
			chunk.ChunkNo, refs[i].URI, chunk.ContentDigest, chunk.ChunkID, chunk.TokenCount); err != nil {
			return fmt.Errorf("publish chunk %d of document %q: %w", chunk.ChunkNo, document.DocumentID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("publish document %q: commit: %w", document.DocumentID, err)
	}
	committed = true
	return nil
}
