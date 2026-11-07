package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/workingset"
)

type bindingsQuery struct {
	keelport.QueryService
	rows [][]any
}

func (q *bindingsQuery) Query(context.Context, string, ...any) (*keelmodel.QueryResult, error) {
	return &keelmodel.QueryResult{Rows: q.rows}, nil
}

type bindingsDB struct {
	keelport.DatabaseRepository
	qs keelport.QueryService
}

func (db bindingsDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.qs
}

type labelsFake struct {
	labels []byte
	err    error
}

func (f labelsFake) Entitlements(context.Context, domain.Principal) ([]byte, string, error) {
	return f.labels, "digest", f.err
}

type retrieverFake struct {
	queries []domain.KnowledgeQuery
	matches []domain.KnowledgeMatch
	err     error
}

func (f *retrieverFake) Retrieve(_ context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	f.queries = append(f.queries, query)
	return domain.KnowledgeResult{Matches: f.matches}, f.err
}

type pinnedFake struct {
	documents []domain.PinnedDocument
}

func (f pinnedFake) PinnedKnowledge(context.Context, domain.PinnedKnowledgeRequest) (domain.PinnedKnowledge, error) {
	return domain.PinnedKnowledge{Documents: f.documents}, nil
}

func candidateRequest() domain.WorkingSetRequest {
	return domain.WorkingSetRequest{TenantContext: domain.TenantContext{TenantID: 7}, RequestID: "r",
		Principal: domain.Principal{TenantID: 7, ID: "writer-a", Release: "3"}, Task: "refund policy"}
}

func match(document, content string) domain.KnowledgeMatch {
	return domain.KnowledgeMatch{DocumentID: document, Content: []byte(content), SourceURI: "s3://" + document, SourceVersion: "v1"}
}

func releaseSource(retriever *retrieverFake, labels []byte, rows ...[]any) *ReleaseCandidates {
	return &ReleaseCandidates{DB: bindingsDB{qs: &bindingsQuery{rows: rows}}, Retriever: retriever, Entitlements: labelsFake{labels: labels}}
}

func TestReleaseCandidatesFeedTheWorkingSetAndKeepWhatTheBudgetExcludes(t *testing.T) {
	retriever := &retrieverFake{matches: []domain.KnowledgeMatch{match("best", strings.Repeat("a", 20)), match("next", strings.Repeat("b", 400))}}
	source := releaseSource(retriever, []byte(`["hr"]`), []any{"kb", "4"})
	source.TopK = 2
	compiler := &workingset.Compiler{Source: source, Limits: domain.WorkingSetLimits{MaxTokens: 10}}
	set, err := compiler.Compile(context.Background(), candidateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Selected) != 1 || set.Selected[0].ID != "knowledge:kb@4:best#0" || len(set.Excluded) != 1 || set.Excluded[0] != "knowledge:kb@4:next#0" {
		t.Fatalf("set = %+v", set)
	}
	if !strings.Contains(set.Selected[0].Provenance, "kb@4") || !strings.Contains(set.Selected[0].Provenance, "s3://best@v1") {
		t.Fatalf("provenance = %q", set.Selected[0].Provenance)
	}
	query := retriever.queries[0]
	if query.KnowledgeBaseID != "kb" || query.KnowledgeVersion != "4" || query.TopK != 2 || string(query.Query) != "refund policy" ||
		string(query.Entitlements) != `["hr"]` || query.EntitlementsDigest != "digest" {
		t.Fatalf("query = %+v", query)
	}
}

func TestReleaseCandidatesRetrieveNothingWithoutEntitlementsOrBindings(t *testing.T) {
	ctx := context.Background()
	retriever := &retrieverFake{matches: []domain.KnowledgeMatch{match("secret", "x")}}
	denied := releaseSource(retriever, nil, []any{"kb", "4"})
	denied.Entitlements = labelsFake{err: domain.ErrForbidden}
	if offered, err := denied.Candidates(ctx, candidateRequest()); err != nil || len(offered.Candidates) != 0 || len(retriever.queries) != 0 {
		t.Fatalf("no entitlements = %+v, %v, %d queries", offered, err, len(retriever.queries))
	}
	unbound := releaseSource(retriever, []byte(`["hr"]`))
	if offered, err := unbound.Candidates(ctx, candidateRequest()); err != nil || len(offered.Candidates) != 0 {
		t.Fatalf("no binding = %+v, %v", offered, err)
	}
}

func TestReleaseCandidatesFailClosed(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("resolver down")
	broken := releaseSource(&retrieverFake{}, nil)
	broken.Entitlements = labelsFake{err: cause}
	if _, err := broken.Candidates(ctx, candidateRequest()); !errors.Is(err, cause) {
		t.Fatalf("entitlement failure = %v", err)
	}
	unretrievable := releaseSource(nil, []byte(`["hr"]`), []any{"kb", "4"})
	unretrievable.Retriever = nil
	if _, err := unretrievable.Candidates(ctx, candidateRequest()); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no retriever = %v", err)
	}
	failing := releaseSource(&retrieverFake{err: cause}, []byte(`["hr"]`), []any{"kb", "4"})
	if _, err := failing.Candidates(ctx, candidateRequest()); !errors.Is(err, cause) {
		t.Fatalf("retrieval failure = %v", err)
	}
	if _, err := (&ReleaseCandidates{}).Candidates(ctx, candidateRequest()); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("zero value = %v", err)
	}
}

func TestPinnedDocumentsAreRequiredCandidates(t *testing.T) {
	source := releaseSource(&retrieverFake{}, []byte(`["hr"]`))
	source.Pinned = pinnedFake{documents: []domain.PinnedDocument{{KnowledgeBaseID: "rules", KnowledgeVersion: "1", DocumentID: "d", Content: []byte("always"), TokenCount: 50}}}
	compiler := &workingset.Compiler{Source: source, Limits: domain.WorkingSetLimits{MaxTokens: 10}}
	if _, err := compiler.Compile(context.Background(), candidateRequest()); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("a document over the budget is never truncated: %v", err)
	}
	compiler.Limits.MaxTokens = 100
	set, err := compiler.Compile(context.Background(), candidateRequest())
	if err != nil || set.Assurance != domain.AssuranceVerified || len(set.Selected) != 1 {
		t.Fatalf("set = %+v, %v", set, err)
	}
}

func TestCandidateIDFitsItsColumn(t *testing.T) {
	if id := candidateID(strings.Repeat("b", 80), strings.Repeat("v", 80), strings.Repeat("d", 200)); len(id) > domain.MaxWorkingSetCandidateIDBytes {
		t.Fatalf("id is %d bytes", len(id))
	}
}
