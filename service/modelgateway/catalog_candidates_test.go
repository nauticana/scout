package modelgateway

import (
	"context"
	"errors"
	"reflect"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type catalogQueryFake struct {
	rows map[string][][]any
	args map[string][]any
	err  error
}

func (query *catalogQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.args == nil {
		query.args = make(map[string][]any)
	}
	query.args[name] = append([]any(nil), args...)
	if query.err != nil {
		return nil, query.err
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*catalogQueryFake) GenID() int64 { return 0 }

type catalogDBFake struct {
	keelport.DatabaseRepository
	query *catalogQueryFake
}

func (db catalogDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func TestTableCandidateCatalogDerivesRoutes(t *testing.T) {
	query := &catalogQueryFake{rows: map[string][][]any{
		qCandidateModels: {
			{"anthropic", "sonnet", int64(200_000), int64(8_000)},
			{"openai", "gpt", int64(128_000), int64(16_000)},
		},
		qCandidateCapabilities: {
			{"anthropic", "sonnet", "text"},
			{"anthropic", "sonnet", "vision"},
			{"openai", "gpt", "text"},
		},
	}}
	catalog := &TableCandidateCatalog{DB: catalogDBFake{query: query}, Region: "eu-central"}

	set, err := catalog.CandidatesFor(context.Background(), domain.TenantContext{TenantID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(query.args[qCandidateModels], []any{int64(42)}) || !reflect.DeepEqual(query.args[qCandidateCapabilities], []any{int64(42)}) {
		t.Fatalf("args = %v / %v", query.args[qCandidateModels], query.args[qCandidateCapabilities])
	}
	want := []domain.ModelCandidate{
		{Provider: "anthropic", Model: "sonnet", Region: "eu-central", RouteID: "anthropic/sonnet",
			Capabilities: []string{"text", "vision"}, MaxContextTokens: 200_000, MaxOutputTokens: 8_000},
		{Provider: "openai", Model: "gpt", Region: "eu-central", RouteID: "openai/gpt",
			Capabilities: []string{"text"}, MaxContextTokens: 128_000, MaxOutputTokens: 16_000},
	}
	if !reflect.DeepEqual(set.Candidates, want) {
		t.Fatalf("candidates = %+v", set.Candidates)
	}
	if set.Generation <= 0 {
		t.Fatalf("generation = %d", set.Generation)
	}

	// The generation is a content hash: same rows, same generation; changed rows, different generation.
	repeat, err := catalog.CandidatesFor(context.Background(), domain.TenantContext{TenantID: 42})
	if err != nil || repeat.Generation != set.Generation {
		t.Fatalf("generation = %d, want %d (%v)", repeat.Generation, set.Generation, err)
	}
	query.rows[qCandidateModels][0][2] = int64(150_000)
	changed, err := catalog.CandidatesFor(context.Background(), domain.TenantContext{TenantID: 42})
	if err != nil || changed.Generation == set.Generation {
		t.Fatalf("generation must follow content: %d (%v)", changed.Generation, err)
	}
}

func TestTableCandidateCatalogValidates(t *testing.T) {
	catalog := &TableCandidateCatalog{DB: catalogDBFake{query: &catalogQueryFake{}}}
	if _, err := catalog.CandidatesFor(context.Background(), domain.TenantContext{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
	if _, err := (&TableCandidateCatalog{}).CandidatesFor(context.Background(), domain.TenantContext{TenantID: 1}); err == nil {
		t.Fatal("expected database error")
	}
	failing := &TableCandidateCatalog{DB: catalogDBFake{query: &catalogQueryFake{err: errors.New("db down")}}}
	if _, err := failing.CandidatesFor(context.Background(), domain.TenantContext{TenantID: 1}); err == nil {
		t.Fatal("expected query error")
	}
}
