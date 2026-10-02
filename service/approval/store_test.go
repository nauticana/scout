package approval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// approvalTable keeps approval_request rows by key and the outbox events queued with them.
type approvalTable struct {
	rows      map[string][]any
	events    map[int64]string
	nextID    int64
	commits   int
	failQuery string
}

type approvalTx struct {
	table   *approvalTable
	pending map[int64]string
	linked  map[int64]int64
}

func requestKey(args []any) string { return args[1].(string) }

func (table *approvalTable) query(name string, args []any, tx *approvalTx) (*keelmodel.QueryResult, error) {
	if name == table.failQuery {
		return nil, errors.New("store down")
	}
	switch name {
	case qApprovalOpen:
		if _, exists := table.rows[requestKey(args)]; exists {
			return &keelmodel.QueryResult{}, nil
		}
		table.nextID++
		row := make([]any, 23)
		row[0], row[1], row[3], row[7], row[17], row[18] = table.nextID, args[1], args[3], args[7], args[17], "pending"
		table.rows[requestKey(args)] = row
		return &keelmodel.QueryResult{Rows: [][]any{{table.nextID}}}, nil
	case qApprovalGet:
		if row, ok := table.rows[args[1].(string)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{row}}, nil
		}
	case qApprovalNotified:
		tx.linked[args[2].(int64)] = args[0].(int64)
	case "keel_outbox_insert":
		tx.pending[args[0].(int64)] = args[5].(string)
	}
	return &keelmodel.QueryResult{}, nil
}

func (tx *approvalTx) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	return tx.table.query(name, args, tx)
}
func (tx *approvalTx) GenID() int64 { return 900 + int64(len(tx.table.events)) }
func (tx *approvalTx) Commit(context.Context) error {
	tx.table.commits++
	for id, payload := range tx.pending {
		tx.table.events[id] = payload
	}
	for _, row := range tx.table.rows {
		if eventID, ok := tx.linked[row[0].(int64)]; ok {
			row[22] = eventID
		}
	}
	return nil
}

// Rollback drops the queued events; the fake keeps inserted rows, so a test asserts on events and links.
func (*approvalTx) Rollback(context.Context) error { return nil }

type autocommit struct{ table *approvalTable }

func (q autocommit) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	return q.table.query(name, args, nil)
}
func (autocommit) GenID() int64 { return 0 }

type approvalDB struct {
	keelport.DatabaseRepository
	table *approvalTable
}

func (db approvalDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return autocommit{table: db.table}
}

func (db approvalDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return &approvalTx{table: db.table, pending: map[int64]string{}, linked: map[int64]int64{}}, nil
}

func owed(requestID string, approver string) domain.ApprovalRequest {
	return domain.ApprovalRequest{
		TenantID: 7, RequestID: requestID, ExecutionStepID: 3, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"},
		Approver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: approver}, Action: "tool:refund", Resource: "refund",
		Class: domain.OutputApprovalRequest, RiskTier: domain.RiskHigh, Summary: "refund", ProposedDigest: strings.Repeat("a", 64),
		DeadlineAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
	}
}

func newApprovalStore(notify bool) (*approvalTable, *TableStore) {
	table := &approvalTable{rows: map[string][]any{}, events: map[int64]string{}}
	return table, &TableStore{DB: approvalDB{table: table}, NotifyThroughOutbox: notify}
}

func TestOpenQueuesTheApproversNoticeOnce(t *testing.T) {
	table, store := newApprovalStore(true)
	ctx := context.Background()
	opened, err := store.Open(ctx, owed("turn-1", "alex"))
	if err != nil || len(table.events) != 1 || opened.NotificationEventID == 0 {
		t.Fatalf("Open = %+v, %v, events %v", opened, err, table.events)
	}
	var notice approvalNotice
	if err = json.Unmarshal([]byte(table.events[opened.NotificationEventID]), &notice); err != nil ||
		notice.RecipientID != "alex" || notice.RequestID != "turn-1" || notice.ExecutionStepID != 3 || notice.RiskTier != "high" || notice.DueAt.IsZero() {
		t.Fatalf("notice = %+v, %v", notice, err)
	}
	replayed, err := store.Open(ctx, owed("turn-1", "alex"))
	if err != nil || len(table.events) != 1 || replayed.NotificationEventID != opened.NotificationEventID {
		t.Fatalf("a replayed turn must not notify again: %+v, %v, events %d", replayed, err, len(table.events))
	}
	byScope, err := store.Open(ctx, owed("turn-2", ""))
	if err != nil || len(table.events) != 1 || byScope.NotificationEventID != 0 {
		t.Fatalf("a request routed by scope has no recipient to notify: %+v, %v", byScope, err)
	}
}

func TestOpenNotifiesNothingWithoutTheOutbox(t *testing.T) {
	table, store := newApprovalStore(false)
	opened, err := store.Open(context.Background(), owed("turn-1", "alex"))
	if err != nil || len(table.events) != 0 || opened.NotificationEventID != 0 || table.commits != 0 {
		t.Fatalf("Open = %+v, %v, events %d, commits %d", opened, err, len(table.events), table.commits)
	}
}

func TestOpenQueuesNothingWhenTheLinkFails(t *testing.T) {
	table, store := newApprovalStore(true)
	table.failQuery = qApprovalNotified
	if _, err := store.Open(context.Background(), owed("turn-1", "alex")); err == nil {
		t.Fatal("a notice that cannot be linked must fail the open")
	}
	if len(table.events) != 0 || table.commits != 0 {
		t.Fatalf("nothing may commit: events %d, commits %d", len(table.events), table.commits)
	}
}

func TestOpenRefusesAChangedProposal(t *testing.T) {
	_, store := newApprovalStore(true)
	ctx := context.Background()
	if _, err := store.Open(ctx, owed("turn-1", "alex")); err != nil {
		t.Fatal(err)
	}
	changed := owed("turn-1", "alex")
	changed.ProposedDigest = strings.Repeat("b", 64)
	if _, err := store.Open(ctx, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a different proposal for the same step: want ErrConflict, got %v", err)
	}
}
