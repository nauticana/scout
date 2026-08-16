package knowledge

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

type pgQueryFake struct {
	rows      map[string][][]any
	errs      map[string]error
	calls     map[string][][]any
	committed int
	rolled    int
}

func (query *pgQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.calls == nil {
		query.calls = make(map[string][][]any)
	}
	query.calls[name] = append(query.calls[name], append([]any(nil), args...))
	if err := query.errs[name]; err != nil {
		return nil, err
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*pgQueryFake) GenID() int64                         { return 0 }
func (query *pgQueryFake) Commit(context.Context) error   { query.committed++; return nil }
func (query *pgQueryFake) Rollback(context.Context) error { query.rolled++; return nil }

type pgDBFake struct {
	keelport.DatabaseRepository
	query    *pgQueryFake
	catalogs []map[string]string
}

func (db *pgDBFake) GetQueryService(_ context.Context, queries map[string]string) keelport.QueryService {
	db.catalogs = append(db.catalogs, queries)
	return db.query
}

func (db *pgDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

func (query *pgQueryFake) last(name string) []any {
	calls := query.calls[name]
	if len(calls) == 0 {
		return nil
	}
	return calls[len(calls)-1]
}

func newPgIndex(query *pgQueryFake) (*PgVectorIndex, *pgDBFake) {
	db := &pgDBFake{query: query}
	return &PgVectorIndex{DB: db}, db
}

const testChunkID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func entitledQuery(labels ...string) domain.KnowledgeQuery {
	raw, err := EncodeEntitlements(labels)
	if err != nil {
		panic(err)
	}
	return domain.KnowledgeQuery{
		TenantContext:      domain.TenantContext{TenantID: 7},
		KnowledgeBaseID:    "kb",
		KnowledgeVersion:   "v1",
		Principal:          "user:a",
		Entitlements:       raw,
		EntitlementsDigest: EntitlementsDigest(raw),
		Query:              []byte("quarterly revenue"),
		Embedding:          []float32{0.5, -0.25, 1},
		TopK:               2,
	}
}

func chunkEmbedding(documentID string, chunkNo int, labels ...string) domain.ChunkEmbedding {
	raw, _ := EncodeEntitlements(labels)
	return domain.ChunkEmbedding{
		Chunk: domain.KnowledgeChunk{
			TenantContext:    domain.TenantContext{TenantID: 7},
			KnowledgeBaseID:  "kb",
			KnowledgeVersion: "v1",
			DocumentID:       documentID,
			SourceVersion:    "etag-1",
			ChunkNo:          chunkNo,
			ChunkID:          testChunkID,
			StartOffset:      10,
			EndOffset:        20,
			Content:          []byte("revenue grew"),
			Entitlements:     raw,
		},
		Embedding: domain.Embedding{Values: []float32{0.5, -0.25, 1}},
	}
}

func matchRow(documentID string, chunkNo int, score float64) []any {
	return []any{documentID, int64(chunkNo), testChunkID, "s3://doc/" + documentID, "etag-1", int64(10), int64(20), score}
}

func TestPgVectorIndexIndexUpsertsInOneTransaction(t *testing.T) {
	query := &pgQueryFake{rows: map[string][][]any{qPgVectorUpsertChunk: {{testChunkID}}}}
	index, _ := newPgIndex(query)
	items := []domain.ChunkEmbedding{chunkEmbedding("doc-a", 0, "group:finance"), chunkEmbedding("doc-a", 1, "user:42", "group:finance")}
	if err := index.Index(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	if query.committed != 1 || query.rolled != 0 || len(query.calls[qPgVectorUpsertChunk]) != 2 {
		t.Fatalf("commits=%d rollbacks=%d upserts=%d", query.committed, query.rolled, len(query.calls[qPgVectorUpsertChunk]))
	}
	args := query.calls[qPgVectorUpsertChunk][1]
	want := []any{int64(7), "kb", "v1", "doc-a", 1, testChunkID, "[0.5,-0.25,1]", 3, "simple", "revenue grew", `["group:finance","user:42"]`, "etag-1", 10, 20}
	if len(args) != len(want) {
		t.Fatalf("upsert args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("upsert arg %d = %v, want %v", i, args[i], want[i])
		}
	}
}

func TestPgVectorIndexIndexRollsBackOnConflictOrError(t *testing.T) {
	cases := []struct {
		name  string
		query *pgQueryFake
		want  error
	}{
		{"identity conflict", &pgQueryFake{}, domain.ErrConflict},
		{"database error", &pgQueryFake{errs: map[string]error{qPgVectorUpsertChunk: errors.New("boom")}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index, _ := newPgIndex(tc.query)
			err := index.Index(context.Background(), []domain.ChunkEmbedding{chunkEmbedding("doc-a", 0, "group:finance"), chunkEmbedding("doc-a", 1, "group:finance")})
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v", err)
			}
			if tc.query.committed != 0 || tc.query.rolled != 1 {
				t.Fatalf("commits=%d rollbacks=%d", tc.query.committed, tc.query.rolled)
			}
		})
	}
}

func TestPgVectorIndexIndexValidation(t *testing.T) {
	valid := chunkEmbedding("doc-a", 0, "group:finance")
	mutate := func(edit func(*domain.ChunkEmbedding)) domain.ChunkEmbedding {
		item := valid
		item.Chunk.Entitlements = append([]byte(nil), valid.Chunk.Entitlements...)
		item.Embedding.Values = append([]float32(nil), valid.Embedding.Values...)
		edit(&item)
		return item
	}
	cases := []struct {
		name string
		item domain.ChunkEmbedding
	}{
		{"no entitlements", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.Entitlements = nil })},
		{"empty entitlements", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.Entitlements = []byte("[]") })},
		{"short chunk id", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.ChunkID = "abc" })},
		{"negative chunk", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.ChunkNo = -1 })},
		{"bad offsets", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.EndOffset = 1 })},
		{"no source version", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.SourceVersion = " " })},
		{"empty embedding", mutate(func(item *domain.ChunkEmbedding) { item.Embedding.Values = nil })},
		{"too wide", mutate(func(item *domain.ChunkEmbedding) { item.Embedding.Values = make([]float32, pgVectorMaxDimensions+1) })},
		{"non-finite", mutate(func(item *domain.ChunkEmbedding) { item.Embedding.Values[0] = float32(math.NaN()) })},
		{"no tenant", mutate(func(item *domain.ChunkEmbedding) { item.Chunk.TenantContext.TenantID = 0 })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := &pgQueryFake{}
			index, _ := newPgIndex(query)
			err := index.Index(context.Background(), []domain.ChunkEmbedding{tc.item})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v", err)
			}
			if len(query.calls) != 0 {
				t.Fatal("invalid batch reached the database")
			}
		})
	}
	index, _ := newPgIndex(&pgQueryFake{})
	if err := index.Index(context.Background(), nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty batch = %v", err)
	}
}

func TestPgVectorIndexRemoveAndTombstoneArgOrder(t *testing.T) {
	query := &pgQueryFake{}
	index, _ := newPgIndex(query)
	if err := index.Remove(context.Background(), 7, "kb", "v1", "doc-a"); err != nil {
		t.Fatal(err)
	}
	if args := query.last(qPgVectorRemoveDocument); len(args) != 4 || args[0] != int64(7) || args[1] != "kb" || args[2] != "v1" || args[3] != "doc-a" {
		t.Fatalf("remove args = %v", args)
	}
	if err := index.Tombstone(context.Background(), 7, "kb", "doc-a"); err != nil {
		t.Fatal(err)
	}
	if args := query.last(qPgVectorTombstoneDocument); len(args) != 3 || args[0] != int64(7) || args[1] != "kb" || args[2] != "doc-a" {
		t.Fatalf("tombstone args = %v", args)
	}
	if err := index.Remove(context.Background(), 0, "", "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("remove validation = %v", err)
	}
	if err := index.Tombstone(context.Background(), 7, "", "doc"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tombstone validation = %v", err)
	}
}

func TestPgVectorIndexSearchVectorArgOrderAndDecoding(t *testing.T) {
	query := &pgQueryFake{rows: map[string][][]any{qPgVectorSearchVector: {matchRow("doc-b", 3, 0.91), matchRow("doc-a", 1, 0.97)}}}
	index, db := newPgIndex(query)
	index.Overfetch = 3
	result, err := index.Search(context.Background(), entitledQuery("user:a", "group:finance"))
	if err != nil {
		t.Fatal(err)
	}
	args := query.last(qPgVectorSearchVector)
	want := []any{"[0.5,-0.25,1]", int64(7), "kb", "v1", `["group:finance","user:a"]`, "[0.5,-0.25,1]", 6}
	if len(args) != len(want) {
		t.Fatalf("search args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("search arg %d = %v, want %v", i, args[i], want[i])
		}
	}
	if len(result.Matches) != 2 || result.Matches[0].DocumentID != "doc-a" || result.Matches[0].Score != 0.97 ||
		result.Matches[1].DocumentID != "doc-b" || result.Matches[1].ChunkNo != 3 || result.Matches[1].SourceURI != "s3://doc/doc-b" ||
		result.Matches[1].StartOffset != 10 || result.Matches[1].EndOffset != 20 || result.Matches[1].ChunkID != testChunkID || result.Matches[1].SourceVersion != "etag-1" {
		t.Fatalf("matches = %+v", result.Matches)
	}
	if len(db.catalogs) != 1 {
		t.Fatalf("catalogs built = %d", len(db.catalogs))
	}
	sql := db.catalogs[0][qPgVectorSearchVector]
	for _, fragment := range []string{"vector(3)", "v.dimensions = 3", "tombstoned = FALSE", "m.active_version = v.knowledge_version", "knowledge_document_manifest", "??|"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("vector search SQL lacks %q:\n%s", fragment, sql)
		}
	}
	if strings.Contains(sql, pgVectorDimensionsToken) {
		t.Fatal("dimension token was not substituted")
	}
	// The same width reuses the compiled catalog; a new width compiles another.
	if _, err := index.Search(context.Background(), entitledQuery("user:a")); err != nil || len(db.catalogs) != 1 {
		t.Fatalf("catalog reuse: %d, %v", len(db.catalogs), err)
	}
	wide := entitledQuery("user:a")
	wide.Embedding = []float32{1, 2, 3, 4}
	if _, err := index.Search(context.Background(), wide); err != nil || len(db.catalogs) != 2 {
		t.Fatalf("new width catalog: %d, %v", len(db.catalogs), err)
	}
}

func TestPgVectorIndexSearchEmbedsThroughGateway(t *testing.T) {
	query := &pgQueryFake{}
	index, _ := newPgIndex(query)
	var embedded []byte
	index.Embedder = &fake.EmbeddingGateway{EmbedFunc: func(_ context.Context, tenant domain.TenantContext, content []byte) (domain.Embedding, error) {
		embedded = content
		if tenant.TenantID != 7 {
			t.Errorf("tenant = %d", tenant.TenantID)
		}
		return domain.Embedding{Values: []float32{1, 0}, Usage: domain.Usage{InputTokens: 4}}, nil
	}}
	q := entitledQuery("user:a")
	q.Embedding = nil
	result, err := index.Search(context.Background(), q)
	if err != nil || string(embedded) != "quarterly revenue" || result.Usage.InputTokens != 4 {
		t.Fatalf("result = %+v, embedded = %q, err = %v", result, embedded, err)
	}
	if args := query.last(qPgVectorSearchVector); args[0] != "[1,0]" {
		t.Fatalf("embedding literal = %v", args[0])
	}

	index.Embedder = nil
	if _, err := index.Search(context.Background(), q); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no embedder = %v", err)
	}
	index.Embedder = &fake.EmbeddingGateway{EmbedFunc: func(context.Context, domain.TenantContext, []byte) (domain.Embedding, error) {
		return domain.Embedding{}, errors.New("provider down")
	}}
	if _, err := index.Search(context.Background(), q); err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("embed failure = %v", err)
	}
}

func TestPgVectorIndexSearchTextArgOrder(t *testing.T) {
	query := &pgQueryFake{rows: map[string][][]any{qPgVectorSearchText: {matchRow("doc-a", 0, 0.4)}}}
	index, _ := newPgIndex(query)
	index.TextSearchConfig = "english"
	retriever := &PgTextRetriever{Index: index}
	result, err := retriever.Retrieve(context.Background(), entitledQuery("user:a"))
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	args := query.last(qPgVectorSearchText)
	want := []any{"english", "quarterly revenue", int64(7), "kb", "v1", `["user:a"]`, 2}
	if len(args) != len(want) {
		t.Fatalf("text args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("text arg %d = %v, want %v", i, args[i], want[i])
		}
	}
	blank := entitledQuery("user:a")
	blank.Query = []byte("  ")
	if _, err := retriever.Retrieve(context.Background(), blank); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank text = %v", err)
	}
}

func TestPgVectorIndexSearchFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		edit func(*domain.KnowledgeQuery)
		want error
	}{
		{"no entitlements", func(q *domain.KnowledgeQuery) { q.Entitlements = nil }, domain.ErrForbidden},
		{"empty entitlements", func(q *domain.KnowledgeQuery) {
			q.Entitlements = []byte("[]")
			q.EntitlementsDigest = EntitlementsDigest(q.Entitlements)
		}, domain.ErrForbidden},
		{"malformed entitlements", func(q *domain.KnowledgeQuery) {
			q.Entitlements = []byte(`{"a":1}`)
			q.EntitlementsDigest = EntitlementsDigest(q.Entitlements)
		}, domain.ErrForbidden},
		{"stale digest", func(q *domain.KnowledgeQuery) { q.EntitlementsDigest = strings.Repeat("0", 64) }, domain.ErrForbidden},
		{"missing digest", func(q *domain.KnowledgeQuery) { q.EntitlementsDigest = "" }, domain.ErrForbidden},
		{"no tenant", func(q *domain.KnowledgeQuery) { q.TenantContext.TenantID = 0 }, domain.ErrValidation},
		{"no version", func(q *domain.KnowledgeQuery) { q.KnowledgeVersion = "" }, domain.ErrValidation},
		{"zero topk", func(q *domain.KnowledgeQuery) { q.TopK = 0 }, domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := &pgQueryFake{}
			index, _ := newPgIndex(query)
			q := entitledQuery("user:a")
			tc.edit(&q)
			for name, search := range map[string]func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error){
				"vector": (&PgVectorRetriever{Index: index}).Retrieve, "text": (&PgTextRetriever{Index: index}).Retrieve,
			} {
				if _, err := search(context.Background(), q); !errors.Is(err, tc.want) {
					t.Fatalf("%s: err = %v", name, err)
				}
			}
			if len(query.calls) != 0 {
				t.Fatal("unauthorized query reached the database")
			}
		})
	}
}

// Two principals differing by one label: only the one holding a granting label may see the chunk,
// both in the Go rule and in the exact entitlement JSON compiled into the query.
func TestPgVectorIndexAuthorizationLeak(t *testing.T) {
	chunkLabels := []string{"user:a"}
	holder := entitledQuery("user:a", "group:finance")
	other := entitledQuery("user:b", "group:finance")
	holderLabels, _ := ParseEntitlements(holder.Entitlements)
	otherLabels, _ := ParseEntitlements(other.Entitlements)
	if !Entitled(chunkLabels, holderLabels) || Entitled(chunkLabels, otherLabels) {
		t.Fatal("entitlement rule leaks or blocks the wrong principal")
	}
	query := &pgQueryFake{}
	index, _ := newPgIndex(query)
	for _, q := range []domain.KnowledgeQuery{holder, other} {
		if _, err := index.Search(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		if args := query.last(qPgVectorSearchVector); args[4] != string(q.Entitlements) {
			t.Fatalf("query entitlements %v not passed verbatim: %v", string(q.Entitlements), args[4])
		}
	}
	if query.calls[qPgVectorSearchVector][0][4] == query.calls[qPgVectorSearchVector][1][4] {
		t.Fatal("distinct principals compiled the same predicate")
	}
}

func TestPgVectorIndexTrimCapsPerDocument(t *testing.T) {
	query := &pgQueryFake{rows: map[string][][]any{qPgVectorSearchVector: {
		matchRow("doc-a", 0, 0.9), matchRow("doc-a", 1, 0.9), matchRow("doc-a", 2, 0.8), matchRow("doc-b", 0, 0.7), matchRow("doc-c", 0, 0.6),
	}}}
	index, _ := newPgIndex(query)
	index.Overfetch, index.MaxChunksPerDocument = 3, 2
	q := entitledQuery("user:a")
	q.TopK = 3
	result, err := index.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, match := range result.Matches {
		got = append(got, match.DocumentID+"#"+string(rune('0'+match.ChunkNo)))
	}
	if strings.Join(got, ",") != "doc-a#0,doc-a#1,doc-b#0" {
		t.Fatalf("trimmed = %v", got)
	}
}

func TestPgVectorIndexEnsureIndexes(t *testing.T) {
	query := &pgQueryFake{}
	index, db := newPgIndex(query)
	if err := index.EnsureIndexes(context.Background(), 1536); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{qPgVectorEnsureHNSWIndex, qPgVectorEnsureTextIndex, qPgVectorEnsureEntitlementIndex} {
		if len(query.calls[name]) != 1 {
			t.Fatalf("%s ran %d times", name, len(query.calls[name]))
		}
	}
	sql := db.catalogs[0][qPgVectorEnsureHNSWIndex]
	if !strings.Contains(sql, "knowledge_chunk_vector_hnsw_1536_ix") || !strings.Contains(sql, "vector(1536)") || !strings.Contains(sql, "WHERE dimensions = 1536") {
		t.Fatalf("hnsw sql = %s", sql)
	}
	for _, dims := range []int{0, -1, pgVectorMaxDimensions + 1} {
		if err := index.EnsureIndexes(context.Background(), dims); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("dims %d = %v", dims, err)
		}
	}
	if err := (&PgVectorIndex{}).EnsureIndexes(context.Background(), 3); err == nil {
		t.Fatal("missing database accepted")
	}
}

func TestEntitlementHelpers(t *testing.T) {
	raw, err := EncodeEntitlements([]string{"b", "a", "b"})
	if err != nil || string(raw) != `["a","b"]` {
		t.Fatalf("encode = %s, %v", raw, err)
	}
	if _, err := EncodeEntitlements([]string{"a", " "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank label = %v", err)
	}
	if _, err := EncodeEntitlements(nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty labels = %v", err)
	}
	labels, err := ParseEntitlements(raw)
	if err != nil || len(labels) != 2 {
		t.Fatalf("parse = %v, %v", labels, err)
	}
	if len(EntitlementsDigest(raw)) != 64 {
		t.Fatal("digest is not sha-256 hex")
	}
	if Entitled(nil, labels) || Entitled(labels, nil) {
		t.Fatal("empty side must fail closed")
	}
}
