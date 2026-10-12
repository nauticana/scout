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

// loopTableFake keeps step_loop_entry rows and the fence of each owned turn, and answers the
// fenced insert the way its guarded touch does: no row change while the fence does not match.
type loopTableFake struct {
	rows   map[string][]any
	fences map[string]int64
}

func (table *loopTableFake) owns(tenantID, requestID, fence any) bool {
	current, owned := table.fences[fmt.Sprint(tenantID, "/", requestID)]
	return owned && current == fence
}

func (table *loopTableFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	switch name {
	case qLoopEntryInsert:
		if !table.owns(args[0], args[1], args[2]) {
			return &keelmodel.QueryResult{Rows: [][]any{{int64(0), int64(0)}}}, nil
		}
		key := fmt.Sprint(args[3:7]...)
		if _, taken := table.rows[key]; taken {
			return &keelmodel.QueryResult{Rows: [][]any{{int64(1), int64(0)}}}, nil
		}
		table.rows[key] = []any{args[6], args[7], args[8]}
		return &keelmodel.QueryResult{Rows: [][]any{{int64(1), int64(1)}}}, nil
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
	owner := domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 2}
	table := &loopTableFake{rows: map[string][]any{}, fences: map[string]int64{"7/request-1": 2}}
	journal := &TableLoopJournal{DB: loopTableDB{table: table}, Objects: newObjectStateStore(&fake.ObjectStorage{})}
	ctx := context.Background()
	key := domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11}
	first := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Iteration: 1, Model: &domain.ModelResult{Output: []byte("first")}}
	if stored, err := journal.Append(ctx, owner, key, first); err != nil || string(stored.Model.Output) != "first" {
		t.Fatalf("Append: %+v, %v", stored, err)
	}
	rival := first
	rival.Model = &domain.ModelResult{Output: []byte("rival")}
	stored, err := journal.Append(ctx, owner, key, rival)
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
	if _, err := journal.Append(ctx, owner, key, domain.LoopEntry{Kind: domain.LoopEntryModel}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation for an unnumbered entry, got %v", err)
	}
}

// A lost fence is told apart from a key conflict: the stale owner is refused even for an entry
// number the current owner already wrote, and never reads that entry back as its own.
func TestTableLoopJournalRefusesAFencedOutOwner(t *testing.T) {
	table := &loopTableFake{rows: map[string][]any{}, fences: map[string]int64{"7/request-1": 3}}
	journal := &TableLoopJournal{DB: loopTableDB{table: table}, Objects: newObjectStateStore(&fake.ObjectStorage{})}
	ctx := context.Background()
	key := domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11}
	current, stale := domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 3}, domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 2}
	entry := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Iteration: 1, Model: &domain.ModelResult{Output: []byte("current")}}
	if _, err := journal.Append(ctx, stale, key, entry); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("stale append = %v, want ErrFenceLost", err)
	}
	if _, err := journal.Append(ctx, current, key, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, stale, key, entry); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("stale append of a taken entry = %v, want ErrFenceLost", err)
	}
	forged := current
	forged.RequestID = "request-2"
	if _, err := journal.Append(ctx, forged, key, entry); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("owner of another turn = %v, want ErrFenceLost", err)
	}
}
