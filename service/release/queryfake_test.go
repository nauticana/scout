package release

import (
	"context"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"
)

type queryFake struct {
	rows map[string][][]any
	args map[string][]any
	err  map[string]error
}

func newQueryFake(rows map[string][][]any) *queryFake {
	return &queryFake{rows: rows, args: map[string][]any{}, err: map[string]error{}}
}

func (query *queryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	query.args[name] = append([]any(nil), args...)
	if err := query.err[name]; err != nil {
		return nil, err
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*queryFake) GenID() int64                   { return 7 }
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

func requireArgs(t *testing.T, query *queryFake, name string, want ...any) {
	t.Helper()
	got := query.args[name]
	if len(got) != len(want) {
		t.Fatalf("%s args = %v, want %v", name, got, want)
	}
	for index, value := range want {
		if got[index] != value {
			t.Fatalf("%s arg %d = %v, want %v", name, index, got[index], value)
		}
	}
}
