package evaluation

import (
	"context"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type queryFake struct {
	rows map[string][][]any
	args map[string][]any
	err  map[string]error
	ids  int64
}

func newQueryFake(rows map[string][][]any) *queryFake {
	if rows == nil {
		rows = make(map[string][][]any)
	}
	return &queryFake{rows: rows, args: make(map[string][]any), err: make(map[string]error)}
}

func (query *queryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	query.args[name] = append([]any(nil), args...)
	if err := query.err[name]; err != nil {
		return nil, err
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (query *queryFake) GenID() int64 { query.ids++; return query.ids }

func (*queryFake) Commit(context.Context) error   { return nil }
func (*queryFake) Rollback(context.Context) error { return nil }

type dbFake struct {
	keelport.DatabaseRepository
	query *queryFake
}

func (db dbFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db dbFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

var testClock = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testExample(id string) domain.GoldenExample {
	return domain.GoldenExample{
		TenantID: 7, ExampleID: id, GoldenSetID: "core", SetVersion: 3, Provenance: "curated",
		ConsentClass: "internal", RetentionClass: "short", RiskTier: "low", Domain: "billing", Language: "en-US",
		Payload: domain.ObjectRef{URI: "object://bucket/" + id, Digest: sha256Hex([]byte(id))},
	}
}

func testManifest(t *testing.T, examples ...domain.GoldenExample) domain.EvaluationManifest {
	t.Helper()
	builder := &ManifestBuilder{Now: fixedClock(testClock)}
	manifest, err := builder.Build(domain.EvaluationManifest{
		TenantID:            7,
		Candidate:           domain.EvaluationSubject{AgentID: "agent", AgentVersion: "v2", Versions: domain.ComponentVersions{Model: "m2", Knowledge: "k1", Index: "i1"}},
		Baseline:            domain.EvaluationSubject{AgentID: "agent", AgentVersion: "v1", Versions: domain.ComponentVersions{Model: "m1", Knowledge: "k1", Index: "i1"}},
		GoldenSetID:         "core",
		GoldenSetVersion:    3,
		DatasetRevision:     DatasetRevision(examples),
		Evaluators:          []domain.EvaluatorVersion{{Kind: "heuristic", Version: "h1"}},
		SafetyPolicyVersion: "safety-1",
	})
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return manifest
}
