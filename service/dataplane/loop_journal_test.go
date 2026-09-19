package dataplane

import (
	"context"
	"errors"
	"fmt"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type loopTableFake struct{ rows map[string][]any }

func (table *loopTableFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	switch name {
	case qLoopEntryInsert:
		key := fmt.Sprint(args[:4]...)
		if _, taken := table.rows[key]; taken {
			return &keelmodel.QueryResult{}, nil
		}
		table.rows[key] = []any{args[3], args[4], args[5]}
		return &keelmodel.QueryResult{Rows: [][]any{{args[3]}}}, nil
	case qLoopEntryGet:
		if row, ok := table.rows[fmt.Sprint(args[:4]...)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{row}}, nil
		}
	case qLoopEntryList:
		result := &keelmodel.QueryResult{}
		for entryNo := 1; ; entryNo++ {
			row, ok := table.rows[fmt.Sprint(args[0], args[1], args[2], entryNo)]
			if !ok {
				return result, nil
			}
			result.Rows = append(result.Rows, row)
		}
	}
	return &keelmodel.QueryResult{}, nil
}

func (*loopTableFake) GenID() int64 { return 0 }

type loopTableDB struct {
	keelport.DatabaseRepository
	table *loopTableFake
}

func (db loopTableDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.table
}

func TestTableLoopJournalKeepsTheFirstWriterAndIsolatesTenants(t *testing.T) {
	journal := &TableLoopJournal{DB: loopTableDB{table: &loopTableFake{rows: map[string][]any{}}}, Objects: newObjectStateStore(&fake.ObjectStorage{})}
	ctx := context.Background()
	key := domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11}
	first := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Iteration: 1, Model: &domain.ModelResult{Output: []byte("first")}}
	if stored, err := journal.Append(ctx, key, first); err != nil || string(stored.Model.Output) != "first" {
		t.Fatalf("Append: %+v, %v", stored, err)
	}
	rival := first
	rival.Model = &domain.ModelResult{Output: []byte("rival")}
	stored, err := journal.Append(ctx, key, rival)
	if err != nil || string(stored.Model.Output) != "first" {
		t.Fatalf("a second writer must get the first writer's entry, got %+v, %v", stored, err)
	}
	entries, err := journal.Load(ctx, key)
	if err != nil || len(entries) != 1 || entries[0].Kind != domain.LoopEntryModel {
		t.Fatalf("Load: %+v, %v", entries, err)
	}
	other := key
	other.TenantID = 8
	if entries, _ := journal.Load(ctx, other); len(entries) != 0 {
		t.Fatalf("another tenant must see an empty journal, got %+v", entries)
	}
	if _, err := journal.Append(ctx, key, domain.LoopEntry{Kind: domain.LoopEntryModel}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation for an unnumbered entry, got %v", err)
	}
}
