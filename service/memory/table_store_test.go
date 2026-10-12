package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type queryFake struct {
	keelport.QueryService
	rows  map[string][][]any
	calls map[string][]any
	err   error
}

func (q *queryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	q.calls[name] = args
	return &keelmodel.QueryResult{Rows: q.rows[name]}, q.err
}

type memoryDB struct {
	keelport.DatabaseRepository
	qs *queryFake
}

func (db memoryDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.qs
}

func newStore(rows map[string][][]any) (*TableStore, *queryFake) {
	qs := &queryFake{rows: rows, calls: map[string][]any{}}
	return &TableStore{DB: memoryDB{qs: qs}}, qs
}

func TestAppendWritesOneLineWithProvenanceUnderTheBound(t *testing.T) {
	store, qs := newStore(map[string][][]any{qMemoryAppend: {{"DONE"}}})
	err := store.Append(context.Background(), domain.MemoryFact{TenantID: 7, SectionID: "DONE",
		Fact: "Shipped the\nnew titles", Source: domain.MemoryVerified, Actor: "search_console"})
	if err != nil {
		t.Fatal(err)
	}
	args := qs.calls[qMemoryAppend]
	if args[0] != int64(7) || args[1] != "Shipped the new titles (verified by search_console)" || args[2] != "search_console" ||
		args[3] != "DONE" || args[4] != false || args[5] != sectionChars || args[7] != sectionChars {
		t.Fatalf("args = %v", args)
	}
}

func TestAppendRefusesUnprovenOversizedAndUnwritable(t *testing.T) {
	store, _ := newStore(map[string][][]any{})
	store.MaxSectionChars = 60
	valid := domain.MemoryFact{TenantID: 7, SectionID: "RULES", Fact: "never discount", Source: domain.MemoryStated, Actor: "owner@example.com"}
	cases := map[string]func(*domain.MemoryFact){
		"no source":  func(f *domain.MemoryFact) { f.Source = "" },
		"no tenant":  func(f *domain.MemoryFact) { f.TenantID = 0 },
		"blank fact": func(f *domain.MemoryFact) { f.Fact = " \n " },
		"no actor":   func(f *domain.MemoryFact) { f.Actor = "" },
		"too long":   func(f *domain.MemoryFact) { f.Fact = strings.Repeat("x", 40) },
	}
	for name, mutate := range cases {
		fact := valid
		mutate(&fact)
		if err := store.Append(context.Background(), fact); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := store.Append(context.Background(), domain.MemoryFact{TenantID: 7, SectionID: "X", Fact: "f"}); !errors.Is(err, domain.ErrUnprovenFact) {
		t.Errorf("unproven err = %v", err)
	}
	// No row came back: the section is unknown, or a tool wrote an owner-only section.
	if err := store.Append(context.Background(), valid); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("unwritable err = %v", err)
	}
	if err := (&TableStore{}).Append(context.Background(), valid); err == nil {
		t.Error("a store without a database must fail")
	}
	if err := (&TableStore{DB: store.DB, MaxSectionChars: -1}).Append(context.Background(), valid); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("negative bound err = %v", err)
	}
}

func TestSectionsAreTenantScopedAndRenderFenced(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	store, qs := newStore(map[string][][]any{qMemorySections: {{"DONE", "Done", "- 2026-10-01 — shipped\n", at}}})
	sections, err := store.Sections(context.Background(), 7)
	if err != nil || len(sections) != 1 || sections[0].Caption != "Done" || !sections[0].UpdatedAt.Equal(at) {
		t.Fatalf("sections = %+v, %v", sections, err)
	}
	if args := qs.calls[qMemorySections]; len(args) != 1 || args[0] != int64(7) {
		t.Fatalf("args = %v", args)
	}
	if _, err := store.Sections(context.Background(), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no tenant err = %v", err)
	}

	rendered, err := Context(sections, 0)
	if err != nil || !strings.Contains(rendered, "Done:\n<untrusted_content>") || !strings.Contains(rendered, "shipped") {
		t.Fatalf("rendered = %q, %v", rendered, err)
	}
	if empty, err := Context([]domain.MemorySection{{SectionID: "OPEN", Body: "  "}}, 0); err != nil || empty != "" {
		t.Fatalf("empty memory = %q, %v", empty, err)
	}
	if _, err := Context(sections, 10); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("over budget err = %v", err)
	}
}
