package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func workItem() domain.WorkItem {
	return domain.WorkItem{
		TenantID: 7, Assignee: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "a"},
		Requester: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "b"}, RequestID: "request:5:a",
		ParentRequestID: "request", Input: domain.ObjectRef{URI: "scout://in", Digest: strings.Repeat("a", 64)},
	}
}

func TestWorkItemStoreKeepsTheDelegatingTurn(t *testing.T) {
	ctx := context.Background()
	query := &queueQueryFake{rows: map[string][][]any{qWorkAssign: {{int64(5)}}}}
	store := &TableWorkItemStore{DB: queueDBFake{query: query}}
	if _, err := store.Assign(ctx, workItem()); err != nil {
		t.Fatal(err)
	}
	if args := query.firstArgs(qWorkAssign); args[len(args)-1] != "request" {
		t.Fatalf("assign args = %v, want the parent request last", args)
	}
	orphan := workItem()
	orphan.ParentRequestID = ""
	if _, err := store.Assign(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qWorkAssign][1]; args[len(args)-1] != nil {
		t.Fatalf("an item no turn delegated stores parent request %v, want NULL", args[len(args)-1])
	}
	long := workItem()
	long.ParentRequestID = strings.Repeat("r", 121)
	if _, err := store.Assign(ctx, long); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a parent request over 120 bytes: %v", err)
	}

	now := time.Now().UTC()
	row := []any{int64(5), "agent", "a", "agent", "b", "", nil, int64(1), "", "approve", "scout://in",
		strings.Repeat("a", 64), "request:5:a", "Q", nil, nil, now, nil, "request"}
	query.rows[qWorkGet], query.rows[qWorkAncestors] = [][]any{row}, [][]any{row}
	item, err := store.Get(ctx, 7, 5)
	if err != nil || item.ParentRequestID != "request" || item.RequestID != "request:5:a" || !item.CompletedAt.IsZero() {
		t.Fatalf("item = %+v, error = %v", item, err)
	}
	chain, err := store.Ancestors(ctx, 7, 5)
	if err != nil || len(chain) != 1 || chain[0] != item {
		t.Fatalf("ancestors = %+v, error = %v", chain, err)
	}
	// The recursive arm lists its columns by hand and must match the anchor's.
	if arm := strings.Count(workItemQueries[qWorkAncestors], "parent."); arm != strings.Count(workColumns, ",")+1+1 {
		t.Fatalf("ancestor arm selects %d parent columns, want %d", arm-1, strings.Count(workColumns, ",")+1)
	}
}
