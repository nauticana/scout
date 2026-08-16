package knowledge

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

type ingestCall struct {
	name string
	args []any
}

// ingestQueryFake records every named query in order; respond overrides rows/errors per call.
type ingestQueryFake struct {
	mu        sync.Mutex
	rows      map[string][][]any
	calls     []ingestCall
	respond   func(name string, args []any) ([][]any, error)
	commits   int
	rollbacks int
	commitErr error
}

func (query *ingestQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	query.mu.Lock()
	defer query.mu.Unlock()
	query.calls = append(query.calls, ingestCall{name: name, args: append([]any(nil), args...)})
	if query.respond != nil {
		rows, err := query.respond(name, args)
		if rows != nil || err != nil {
			return &keelmodel.QueryResult{Rows: rows}, err
		}
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (query *ingestQueryFake) GenID() int64 { return 42 }

func (query *ingestQueryFake) Commit(context.Context) error {
	query.mu.Lock()
	defer query.mu.Unlock()
	query.commits++
	return query.commitErr
}

func (query *ingestQueryFake) Rollback(context.Context) error {
	query.mu.Lock()
	defer query.mu.Unlock()
	query.rollbacks++
	return nil
}

func (query *ingestQueryFake) named(name string) []ingestCall {
	query.mu.Lock()
	defer query.mu.Unlock()
	var calls []ingestCall
	for _, call := range query.calls {
		if call.name == name {
			calls = append(calls, call)
		}
	}
	return calls
}

func (query *ingestQueryFake) names() []string {
	query.mu.Lock()
	defer query.mu.Unlock()
	names := make([]string, len(query.calls))
	for i, call := range query.calls {
		names[i] = call.name
	}
	return names
}

type ingestDBFake struct {
	keelport.DatabaseRepository
	query *ingestQueryFake
}

func (db ingestDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db ingestDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testDocument(id string, content []byte) domain.KnowledgeDocument {
	return domain.KnowledgeDocument{
		TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1", DocumentID: id,
		SourceURI: "object://bucket/" + id, SourceVersion: "s1", ContentDigest: sha256Bytes(content), MediaType: "text/plain",
		Entitlements: []byte(`{"dept":"sales"}`), RedactionPolicyVersion: "",
	}
}

type pipelineHarness struct {
	pipeline *IngestPipeline
	query    *ingestQueryFake
	sources  map[string][]byte
	indexed  atomic.Int32
	removed  atomic.Int32
	embeds   atomic.Int32
	stored   atomic.Int32
	activate atomic.Int32
}

func newPipelineHarness(sources map[string][]byte) *pipelineHarness {
	harness := &pipelineHarness{query: &ingestQueryFake{}, sources: sources}
	harness.pipeline = &IngestPipeline{
		Loader: &fake.SourceLoader{LoadFunc: func(_ context.Context, document domain.KnowledgeDocument) ([]byte, error) {
			raw, ok := sources[document.DocumentID]
			if !ok {
				return nil, fmt.Errorf("missing source %s", document.DocumentID)
			}
			return raw, nil
		}},
		Decoder: PlainTextDecoder{},
		Chunker: &SectionChunker{MaxTokens: 8, Overlap: 2},
		Embedder: &fake.EmbeddingGateway{EmbedFunc: func(_ context.Context, _ domain.TenantContext, content []byte) (domain.Embedding, error) {
			harness.embeds.Add(1)
			return domain.Embedding{Values: []float32{float32(len(content))}, Usage: domain.Usage{InputTokens: 1}}, nil
		}},
		ChunkStore: &fake.KnowledgeChunkStore{PutChunkFunc: func(_ context.Context, chunk domain.KnowledgeChunk) (domain.ObjectRef, error) {
			harness.stored.Add(1)
			return domain.ObjectRef{URI: "object://chunks/" + chunk.ChunkID, Digest: chunk.ContentDigest}, nil
		}},
		Index: &fake.KnowledgeVectorIndex{
			IndexFunc:  func(context.Context, []domain.ChunkEmbedding) error { harness.indexed.Add(1); return nil },
			RemoveFunc: func(context.Context, int64, string, string, string) error { harness.removed.Add(1); return nil },
		},
		Manifests: &fake.KnowledgeManifestStore{ActivateFunc: func(context.Context, domain.KnowledgeDocumentManifest) (string, error) {
			harness.activate.Add(1)
			return "", nil
		}},
		DB: ingestDBFake{query: harness.query},
	}
	return harness
}

func TestIngestPipelinePublishesInOrderAndCorrelates(t *testing.T) {
	sources := map[string][]byte{
		"a": []byte("alpha one two three four five six seven\n\nsecond paragraph here now"),
		"b": []byte("bravo"),
		"c": []byte("charlie has a bit more text than bravo"),
	}
	harness := newPipelineHarness(sources)
	harness.pipeline.PrepareWorkers, harness.pipeline.EmbedWorkers, harness.pipeline.PublishWorkers, harness.pipeline.QueueDepth = 3, 2, 2, 1
	batch := domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1"}
	for _, id := range []string{"a", "b", "c"} {
		batch.Documents = append(batch.Documents, testDocument(id, sources[id]))
	}
	result, err := harness.pipeline.IngestBatch(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 3 {
		t.Fatalf("items = %d", len(result.Items))
	}
	for i, id := range []string{"a", "b", "c"} {
		item := result.Items[i]
		if item.DocumentID != id || item.Failure != "" || item.ChunkCount == 0 || item.IdempotencyKey != IngestIdempotencyKey(7, "kb", "v1", id, sha256Bytes(sources[id])) {
			t.Fatalf("item %d = %+v", i, item)
		}
	}
	if result.Usage.InputTokens != int64(harness.embeds.Load()) || harness.indexed.Load() != 3 || harness.activate.Load() != 3 {
		t.Fatalf("usage %+v indexed %d activated %d", result.Usage, harness.indexed.Load(), harness.activate.Load())
	}
	docs := harness.query.named(qIngestInsertDocument)
	if len(docs) != 3 || harness.query.commits != 3 {
		t.Fatalf("document inserts = %d commits = %d", len(docs), harness.query.commits)
	}
	for _, call := range docs {
		if call.args[0] != int64(7) || call.args[1] != "kb" || call.args[2] != "v1" || call.args[4] != "object://bucket/"+call.args[3].(string) || call.args[6] != "text/plain" {
			t.Fatalf("document args = %v", call.args)
		}
	}
	chunks := harness.query.named(qIngestInsertChunk)
	if len(chunks) != result.Items[0].ChunkCount+result.Items[1].ChunkCount+result.Items[2].ChunkCount {
		t.Fatalf("chunk inserts = %d", len(chunks))
	}
	for _, call := range chunks {
		documentID, chunkNo := call.args[3].(string), call.args[4].(int)
		wantID := ChunkID(7, "kb", documentID, "s1", defaultChunkerVersion, chunkNo)
		if call.args[0] != int64(7) || call.args[5] != "object://chunks/"+wantID || call.args[7] != wantID || call.args[8].(int) <= 0 || len(call.args[6].(string)) != 64 {
			t.Fatalf("chunk args = %v", call.args)
		}
	}
	// Publish ordering: index before the relational transaction for every document.
	names := harness.query.names()
	for i := 1; i < len(names); i++ {
		if names[i] == qIngestInsertChunk && names[i-1] != qIngestInsertChunk && names[i-1] != qIngestInsertDocument {
			t.Fatalf("chunk insert outside a document transaction: %v", names)
		}
	}
}

func TestIngestPipelineIsolatesTerminalDocuments(t *testing.T) {
	sources := map[string][]byte{"good": []byte("good content"), "tampered": []byte("real bytes"), "missing": nil}
	harness := newPipelineHarness(sources)
	tampered := testDocument("tampered", []byte("declared bytes"))
	nilEntitlements := testDocument("closed", []byte("x"))
	nilEntitlements.Entitlements = nil
	sources["closed"] = []byte("x")
	batch := domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1",
		Documents: []domain.KnowledgeDocument{testDocument("good", sources["good"]), tampered, testDocument("missing", []byte("m")), nilEntitlements}}
	delete(sources, "missing")
	result, err := harness.pipeline.IngestBatch(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if result.Items[0].Failure != "" || result.Items[0].ChunkCount != 1 {
		t.Fatalf("good = %+v", result.Items[0])
	}
	if !result.Items[1].Terminal || !strings.Contains(result.Items[1].Failure, "digest") {
		t.Fatalf("tampered = %+v", result.Items[1])
	}
	if result.Items[2].Terminal || !strings.Contains(result.Items[2].Failure, "load source") {
		t.Fatalf("missing = %+v", result.Items[2])
	}
	if !result.Items[3].Terminal || !strings.Contains(result.Items[3].Failure, "fails closed") {
		t.Fatalf("closed = %+v", result.Items[3])
	}
	if harness.indexed.Load() != 1 {
		t.Fatalf("indexed = %d", harness.indexed.Load())
	}
}

func TestIngestPipelineReplaysIdenticalDocument(t *testing.T) {
	sources := map[string][]byte{"a": []byte("same content")}
	harness := newPipelineHarness(sources)
	document := testDocument("a", sources["a"])
	harness.query.rows = map[string][][]any{qIngestFindDocument: {{document.ContentDigest, int64(4)}}}
	result, err := harness.pipeline.IngestBatch(context.Background(), domain.IngestBatch{TenantContext: document.TenantContext, KnowledgeBaseID: "kb", KnowledgeVersion: "v1", Documents: []domain.KnowledgeDocument{document}})
	if err != nil || result.Items[0].Failure != "" || result.Items[0].ChunkCount != 4 {
		t.Fatalf("replay = %+v, %v", result, err)
	}
	if harness.embeds.Load() != 0 || harness.indexed.Load() != 0 || harness.activate.Load() != 1 {
		t.Fatalf("replay did work: embeds %d indexed %d activated %d", harness.embeds.Load(), harness.indexed.Load(), harness.activate.Load())
	}
	find := harness.query.named(qIngestFindDocument)
	if len(find) != 1 || find[0].args[0] != int64(7) || find[0].args[1] != "kb" || find[0].args[2] != "v1" || find[0].args[3] != "a" {
		t.Fatalf("find args = %v", find)
	}

	harness.query.rows[qIngestFindDocument] = [][]any{{testDigest, int64(4)}}
	result, err = harness.pipeline.IngestBatch(context.Background(), domain.IngestBatch{TenantContext: document.TenantContext, KnowledgeBaseID: "kb", KnowledgeVersion: "v1", Documents: []domain.KnowledgeDocument{document}})
	if err != nil || !result.Items[0].Terminal || !strings.Contains(result.Items[0].Failure, "different content") {
		t.Fatalf("conflict = %+v, %v", result, err)
	}
}

func TestIngestPipelineReconcilesIndexWhenPublishFails(t *testing.T) {
	sources := map[string][]byte{"a": []byte("content")}
	harness := newPipelineHarness(sources)
	harness.query.commitErr = errors.New("commit failed")
	result, err := harness.pipeline.IngestBatch(context.Background(), domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1", Documents: []domain.KnowledgeDocument{testDocument("a", sources["a"])}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Items[0].Terminal || !strings.Contains(result.Items[0].Failure, "commit failed") || harness.removed.Load() != 1 || harness.query.rollbacks != 1 || harness.activate.Load() != 0 {
		t.Fatalf("publish failure = %+v removed %d rollbacks %d", result.Items[0], harness.removed.Load(), harness.query.rollbacks)
	}

	harness = newPipelineHarness(sources)
	harness.query.commitErr = errors.New("commit failed")
	harness.pipeline.Index.(*fake.KnowledgeVectorIndex).RemoveFunc = func(context.Context, int64, string, string, string) error { return errors.New("remove failed") }
	result, _ = harness.pipeline.IngestBatch(context.Background(), domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1", Documents: []domain.KnowledgeDocument{testDocument("a", sources["a"])}})
	if !strings.Contains(result.Items[0].Failure, "commit failed") || !strings.Contains(result.Items[0].Failure, "remove failed") {
		t.Fatalf("joined failure = %q", result.Items[0].Failure)
	}
}

func TestIngestPipelineSystemicFailureCancelsBatch(t *testing.T) {
	sources := map[string][]byte{}
	batch := domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1"}
	for i := range 20 {
		id := fmt.Sprintf("doc-%02d", i)
		sources[id] = []byte("content of " + id)
		batch.Documents = append(batch.Documents, testDocument(id, sources[id]))
	}
	harness := newPipelineHarness(sources)
	harness.pipeline.Embedder = &fake.EmbeddingGateway{EmbedFunc: func(context.Context, domain.TenantContext, []byte) (domain.Embedding, error) {
		return domain.Embedding{}, fmt.Errorf("provider: %w", domain.ErrCircuitOpen)
	}}
	result, err := harness.pipeline.IngestBatch(context.Background(), batch)
	if !errors.Is(err, domain.ErrCircuitOpen) {
		t.Fatalf("err = %v", err)
	}
	if len(result.Items) != 20 || harness.indexed.Load() != 0 {
		t.Fatalf("items = %d indexed = %d", len(result.Items), harness.indexed.Load())
	}
	for _, item := range result.Items {
		if item.Failure == "" || item.Terminal {
			t.Fatalf("item = %+v", item)
		}
	}
}

func TestIngestPipelineLifecycle(t *testing.T) {
	sources := map[string][]byte{"a": []byte("content a"), "b": []byte("content b")}
	batch := domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1",
		Documents: []domain.KnowledgeDocument{testDocument("a", sources["a"]), testDocument("b", sources["b"])}}

	t.Run("empty input", func(t *testing.T) {
		harness := newPipelineHarness(sources)
		before := runtime.NumGoroutine()
		result, err := harness.pipeline.IngestBatch(context.Background(), domain.IngestBatch{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1"})
		if err != nil || len(result.Items) != 0 || runtime.NumGoroutine() > before {
			t.Fatalf("empty = %+v, %v", result, err)
		}
	})
	t.Run("invalid worker count", func(t *testing.T) {
		harness := newPipelineHarness(sources)
		harness.pipeline.EmbedWorkers = -1
		if _, err := harness.pipeline.IngestBatch(context.Background(), batch); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v", err)
		}
		harness.pipeline.EmbedWorkers, harness.pipeline.QueueDepth = 1, -1
		if _, err := harness.pipeline.IngestBatch(context.Background(), batch); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("blocking loader unblocks on cancel", func(t *testing.T) {
		harness := newPipelineHarness(sources)
		entered := make(chan struct{}, 2)
		exited := make(chan struct{}, 2)
		harness.pipeline.Loader = &fake.SourceLoader{LoadFunc: func(ctx context.Context, _ domain.KnowledgeDocument) ([]byte, error) {
			entered <- struct{}{}
			defer func() { exited <- struct{}{} }()
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		ctx, cancel := context.WithCancel(context.Background())
		type outcome struct {
			result domain.IngestBatchResult
			err    error
		}
		finished := make(chan outcome, 1)
		go func() {
			result, err := harness.pipeline.IngestBatch(ctx, batch)
			finished <- outcome{result, err}
		}()
		<-entered
		cancel()
		select {
		case done := <-finished:
			if !errors.Is(done.err, context.Canceled) || len(done.result.Items) != 2 || done.result.Items[0].Failure == "" || done.result.Items[1].Failure == "" {
				t.Fatalf("canceled = %+v, %v", done.result, done.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("batch did not return after cancellation")
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("loader worker leaked")
		}
	})
	t.Run("early failure stops later work", func(t *testing.T) {
		harness := newPipelineHarness(sources)
		harness.pipeline.PrepareWorkers = 1
		var loads atomic.Int32
		harness.pipeline.Loader = &fake.SourceLoader{LoadFunc: func(_ context.Context, document domain.KnowledgeDocument) ([]byte, error) {
			loads.Add(1)
			return sources[document.DocumentID], nil
		}}
		harness.pipeline.Embedder = &fake.EmbeddingGateway{EmbedFunc: func(context.Context, domain.TenantContext, []byte) (domain.Embedding, error) {
			return domain.Embedding{}, context.DeadlineExceeded
		}}
		result, err := harness.pipeline.IngestBatch(context.Background(), batch)
		if !errors.Is(err, context.DeadlineExceeded) || harness.indexed.Load() != 0 || len(result.Items) != 2 {
			t.Fatalf("early failure = %+v, %v", result, err)
		}
	})
}

func TestIngestPipelineIngestSingleDocument(t *testing.T) {
	sources := map[string][]byte{"a": []byte("single")}
	harness := newPipelineHarness(sources)
	if err := harness.pipeline.Ingest(context.Background(), testDocument("a", sources["a"])); err != nil {
		t.Fatal(err)
	}
	bad := testDocument("a", []byte("other"))
	if err := harness.pipeline.Ingest(context.Background(), bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("digest mismatch = %v", err)
	}
	if err := (&IngestPipeline{}).Ingest(context.Background(), bad); err == nil {
		t.Fatal("missing dependencies accepted")
	}
}

func TestIngestPipelineRedactsBeforeEmbedding(t *testing.T) {
	sources := map[string][]byte{"a": []byte("name: Ada\nssn: 123-45-6789")}
	harness := newPipelineHarness(sources)
	harness.pipeline.Chunker = &SectionChunker{MaxTokens: 100, Overlap: 0}
	harness.pipeline.Redactor = &PolicyRedactor{Policies: []RedactionPolicy{&AllowlistPolicy{PolicyVersion: "p1", Fields: []string{"name"}}}}
	var embedded []byte
	harness.pipeline.Embedder = &fake.EmbeddingGateway{EmbedFunc: func(_ context.Context, _ domain.TenantContext, content []byte) (domain.Embedding, error) {
		embedded = append([]byte(nil), content...)
		return domain.Embedding{Values: []float32{1}}, nil
	}}
	document := testDocument("a", sources["a"])
	document.RedactionPolicyVersion = "p1"
	if err := harness.pipeline.Ingest(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	if string(embedded) != "name: Ada\nssn: [redacted]" {
		t.Fatalf("embedded = %q", embedded)
	}
	chunks := harness.query.named(qIngestInsertChunk)
	if len(chunks) != 1 || chunks[0].args[6] != sha256Bytes(embedded) {
		t.Fatalf("chunk digest = %v", chunks)
	}
}
