package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

var historyPerson = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "41"}

func historyRow(requestID string, queuedAt time.Time, currencies int64) []any {
	return []any{requestID, "conversation-1", "writer", "3", "C", "case-9", "draft", "Write the note", "",
		`{"ok":true}`, queuedAt, queuedAt.Add(time.Second), int64(1250), "JPY", int64(0), currencies}
}

func TestTurnHistoryPagesThePersonsTurnsNewestFirst(t *testing.T) {
	newest := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	query := &queueQueryFake{rows: map[string][][]any{qTurnHistory: {
		historyRow("request-3", newest, 1), historyRow("request-2", newest.Add(-time.Minute), 1), historyRow("request-1", newest.Add(-2*time.Minute), 1),
	}}}
	history := &TurnHistory{DB: queueDBFake{query: query}}
	page, err := history.Turns(context.Background(), domain.TenantContext{TenantID: 7}, historyPerson, domain.TurnHistoryFilter{WorkRef: "case-9", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	args := query.firstArgs(qTurnHistory)
	if args[0] != int64(7) || args[1] != "41" || args[2] != "case-9" || args[4] != "" || args[len(args)-1] != 3 {
		t.Fatalf("query args = %v", args)
	}
	if !strings.Contains(turnHistoryQueries[qTurnHistory], "c.end_user_ref = ?") {
		t.Fatal("the history must be bound to the person's conversations")
	}
	if len(page.Turns) != 2 || page.NextBefore != encodeTurnCursor(newest.Add(-time.Minute), "request-2") {
		t.Fatalf("page = %+v", page)
	}
	turn := page.Turns[0]
	if turn.RequestID != "request-3" || turn.AgentID != "writer" || turn.AgentVersion != "3" || turn.Status != domain.TurnCompleted ||
		turn.WorkRef != "case-9" || turn.TaskKind != "draft" || string(turn.Result) != `{"ok":true}` ||
		turn.CostMinorUnits != 1250 || turn.Currency != "JPY" || turn.Exponent != 0 || !turn.CompletedAt.After(turn.QueuedAt) {
		t.Fatalf("turn = %+v", turn)
	}

	next, err := history.Turns(context.Background(), domain.TenantContext{TenantID: 7}, historyPerson, domain.TurnHistoryFilter{Before: page.NextBefore})
	if err != nil {
		t.Fatal(err)
	}
	args = query.args[qTurnHistory][1]
	if args[4] != page.NextBefore || !args[5].(time.Time).Equal(newest.Add(-time.Minute)) || args[7] != "request-2" || args[len(args)-1] != DefaultTurnHistoryPageSize+1 {
		t.Fatalf("cursor args = %v", args)
	}
	if len(next.Turns) != 3 || next.NextBefore != "" {
		t.Fatalf("last page = %+v", next)
	}
}

func TestTurnHistoryRefusesWhatItCannotAnswer(t *testing.T) {
	ctx, tenant := context.Background(), domain.TenantContext{TenantID: 7}
	for name, tc := range map[string]struct {
		history *TurnHistory
		tenant  domain.TenantContext
		person  domain.PrincipalRef
		filter  domain.TurnHistoryFilter
		want    error
	}{
		"no database":     {&TurnHistory{}, tenant, historyPerson, domain.TurnHistoryFilter{}, domain.ErrNotReady},
		"page over max":   {&TurnHistory{PageSize: 300}, tenant, historyPerson, domain.TurnHistoryFilter{}, domain.ErrNotReady},
		"no tenant":       {&TurnHistory{}, domain.TenantContext{}, historyPerson, domain.TurnHistoryFilter{}, domain.ErrForbidden},
		"a service":       {&TurnHistory{}, tenant, domain.PrincipalRef{Kind: domain.PrincipalService, ID: "x"}, domain.TurnHistoryFilter{}, domain.ErrForbidden},
		"negative limit":  {&TurnHistory{}, tenant, historyPerson, domain.TurnHistoryFilter{Limit: -1}, domain.ErrValidation},
		"limit too large": {&TurnHistory{}, tenant, historyPerson, domain.TurnHistoryFilter{Limit: 201}, domain.ErrValidation},
		"long work ref":   {&TurnHistory{}, tenant, historyPerson, domain.TurnHistoryFilter{WorkRef: strings.Repeat("w", 201)}, domain.ErrValidation},
		"bad cursor":      {&TurnHistory{}, tenant, historyPerson, domain.TurnHistoryFilter{Before: "yesterday"}, domain.ErrValidation},
	} {
		if tc.history.DB == nil && name != "no database" {
			tc.history.DB = queueDBFake{query: &queueQueryFake{}}
		}
		if _, err := tc.history.Turns(ctx, tc.tenant, tc.person, tc.filter); !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
	query := &queueQueryFake{rows: map[string][][]any{qTurnHistory: {historyRow("request-1", time.Now(), 2)}}}
	if _, err := (&TurnHistory{DB: queueDBFake{query: query}}).Turns(ctx, tenant, historyPerson, domain.TurnHistoryFilter{}); err == nil {
		t.Fatal("a turn settled in two currencies must not be summed")
	}
}

func TestTurnHistoryReturnsOnlyBoundedErrorCodes(t *testing.T) {
	row := historyRow("request-1", time.Now(), 1)
	row[8] = "database password is secret"
	query := &queueQueryFake{rows: map[string][][]any{qTurnHistory: {row}}}
	history := &TurnHistory{DB: queueDBFake{query: query}}
	page, err := history.Turns(context.Background(), domain.TenantContext{TenantID: 7}, historyPerson, domain.TurnHistoryFilter{})
	if err != nil || page.Turns[0].ErrorText != "internal" {
		t.Fatalf("error text = %q, error = %v", page.Turns[0].ErrorText, err)
	}
	for _, code := range []string{"rate_limited", string(domain.ApprovalStatusRejected), string(domain.ApprovalStatusExpired), turnDispatchFailed} {
		row[8] = code
		if page, err = history.Turns(context.Background(), domain.TenantContext{TenantID: 7}, historyPerson, domain.TurnHistoryFilter{}); err != nil || page.Turns[0].ErrorText != code {
			t.Fatalf("runtime code %q = %q, error = %v", code, page.Turns[0].ErrorText, err)
		}
	}
}

func TestTurnHistoryReadsTheEndUserRefTheLedgerStored(t *testing.T) {
	ctx, tenant := context.Background(), domain.TenantContext{TenantID: 7}
	query := &queueQueryFake{}
	mapperCalled := false
	history := &TurnHistory{DB: queueDBFake{query: query}, EndUserRef: func(person domain.PrincipalRef) string {
		mapperCalled = true
		if person.ID == "41" {
			return "workspace:41"
		}
		return ""
	}}
	if _, err := history.Turns(ctx, tenant, historyPerson, domain.TurnHistoryFilter{}); err != nil {
		t.Fatal(err)
	}
	if args := query.firstArgs(qTurnHistory); args[1] != "workspace:41" {
		t.Fatalf("end user ref = %v", args[1])
	}
	if _, err := history.Turns(ctx, tenant, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, domain.TurnHistoryFilter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an unmapped person: error = %v", err)
	}
	mapperCalled = false
	if _, err := history.Turns(ctx, tenant, domain.PrincipalRef{Kind: domain.PrincipalHuman}, domain.TurnHistoryFilter{}); !errors.Is(err, domain.ErrForbidden) || mapperCalled {
		t.Fatalf("an invalid person: error = %v, mapper called = %v", err, mapperCalled)
	}
}
