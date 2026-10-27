package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// approvalTable keeps turn_approval_request rows by key and the outbox events queued with them.
type approvalTable struct {
	rows        map[string][]any
	decisions   map[int64][]any
	events      map[int64]string
	nextID      int64
	commits     int
	failQuery   string
	fences      [][]any
	escalations [][]any
	vacantArgs  []any
	settled     map[int64]bool
}

type approvalTx struct {
	table   *approvalTable
	pending map[int64]string
	linked  map[int64]int64
}

// requestKey keys a row by request id and execution step, the store's natural key within a tenant.
func requestKey(requestID, step any) string { return fmt.Sprintf("%v/%v", requestID, step) }

func open(row []any) bool {
	return row[18] == string(domain.ApprovalStatusPending) || row[18] == string(domain.ApprovalStatusEscalated)
}

// selectRows returns copies of the open rows keep accepts, in id order.
func (table *approvalTable) selectRows(keep func([]any) bool) *keelmodel.QueryResult {
	result := &keelmodel.QueryResult{}
	for _, row := range table.rows {
		if open(row) && keep(row) {
			result.Rows = append(result.Rows, slices.Clone(row))
		}
	}
	slices.SortFunc(result.Rows, func(a, b []any) int { return int(a[0].(int64) - b[0].(int64)) })
	return result
}

// assignedAs is the fence: route step and approver as the caller read them; nil matches only nil.
func assignedAs(row []any, step, kind, id any) bool {
	return fmt.Sprint(row[31]) == fmt.Sprint(step) && row[6] == kind && row[7] == id
}

func (table *approvalTable) query(name string, args []any, tx *approvalTx) (*keelmodel.QueryResult, error) {
	if name == table.failQuery {
		return nil, errors.New("store down")
	}
	switch name {
	case qApprovalOpen:
		if _, exists := table.rows[requestKey(args[1], args[3])]; exists {
			return &keelmodel.QueryResult{}, nil
		}
		table.nextID++
		row := make([]any, 34)
		copy(row[1:18], args[1:18])
		row[0], row[18], row[19], row[23] = table.nextID, string(domain.ApprovalStatusPending), args[18], args[19]
		copy(row[24:34], args[20:30])
		table.rows[requestKey(args[1], args[3])] = row
		return &keelmodel.QueryResult{Rows: [][]any{{table.nextID}}}, nil
	case qApprovalGet:
		if row, ok := table.rows[requestKey(args[1], args[2])]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{slices.Clone(row)}}, nil
		}
	case qApprovalResolve:
		table.fences = append(table.fences, args[6:9])
		row, ok := table.rows[requestKey(args[3], args[4])]
		if ok && open(row) && row[17] == args[5] && assignedAs(row, args[6], args[7], args[8]) {
			row[18], row[21] = args[0], args[1]
			return &keelmodel.QueryResult{Rows: [][]any{{row[0]}}}, nil
		}
	case qApprovalEscalate:
		table.escalations = append(table.escalations, args)
		for _, row := range table.rows {
			if row[0] == args[9] && open(row) && assignedAs(row, args[10], args[11], args[12]) {
				row[18], row[6], row[7], row[19], row[31], row[29], row[30], row[32] = args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7]
				return &keelmodel.QueryResult{Rows: [][]any{{row[0]}}}, nil
			}
		}
	case qApprovalVacant:
		table.vacantArgs = args
		return table.selectRows(func(row []any) bool {
			deadline, timed := row[19].(time.Time)
			return row[28] != nil && row[7] == nil && (!timed || deadline.After(args[1].(time.Time)))
		}), nil
	case qApprovalUnsettled:
		result := &keelmodel.QueryResult{}
		for _, row := range table.rows {
			if row[18] == string(domain.ApprovalStatusExpired) && !table.settled[row[0].(int64)] {
				result.Rows = append(result.Rows, slices.Clone(row))
			}
		}
		return result, nil
	case qApprovalTurnSettled:
		table.settled[args[2].(int64)] = true
	case qApprovalDue:
		return table.selectRows(func(row []any) bool {
			deadline, timed := row[19].(time.Time)
			return timed && !deadline.After(args[1].(time.Time))
		}), nil
	case qApprovalDecision:
		table.decisions[args[0].(int64)] = args
	case qApprovalDecisionGet, qApprovalRevision:
		row, ok := table.rows[requestKey(args[1], args[2])]
		if !ok {
			break
		}
		d, ok := table.decisions[row[0].(int64)]
		if !ok {
			break
		}
		if name == qApprovalDecisionGet {
			return &keelmodel.QueryResult{Rows: [][]any{{d[2], d[3], d[4], d[5], d[6], d[7], d[8], d[9], d[12]}}}, nil
		}
		if d[2] == string(domain.ApprovalStatusEdited) && d[11] != nil {
			return &keelmodel.QueryResult{Rows: [][]any{{d[3], d[4], d[5], d[6], d[7], d[9], d[11], d[12]}}}, nil
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
	maps.Copy(tx.table.events, tx.pending)
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
	request := domain.ApprovalRequest{
		TenantID: 7, RequestID: requestID, ExecutionStepID: 3, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"},
		Source: domain.ApprovalSourceAutonomy, RuleID: "autonomy", Action: "tool:refund", Resource: "refund",
		Class: domain.OutputApprovalRequest, RiskTier: domain.RiskHigh, Summary: "refund", ProposedDigest: strings.Repeat("a", 64),
		DeadlineAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
	}
	if approver != "" {
		request.Approver = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: approver}
	}
	return request
}

// routedOwed is a request routed under the payments route at step, assigned to approver or vacant.
func routedOwed(requestID, approver string, step int) domain.ApprovalRequest {
	request := owed(requestID, approver)
	request.ReleaseVersion, request.Source, request.RuleID = "3", domain.ApprovalSourcePolicy, "policy"
	request.Requester = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "5"}
	request.Route = routedAuthority(step)
	request.Route.Resolution = "directory:holder/77"
	return request
}

func newApprovalStore(notify bool) (*approvalTable, *TableStore) {
	table := &approvalTable{rows: map[string][]any{}, decisions: map[int64][]any{}, events: map[int64]string{}, settled: map[int64]bool{}}
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

func TestOpenAcceptsOnlySeededOutputClassCodes(t *testing.T) {
	if err := validateOpen(owed("req-class", "")); err != nil {
		t.Fatalf("a seeded class must open: %v", err)
	}
	for _, class := range []domain.OutputClass{"", "approval_request", "advice", "r"} {
		request := owed("req-class", "")
		request.Class = class
		if err := validateOpen(request); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("class %q: want ErrValidation, got %v", class, err)
		}
	}
}

func TestOpenKeepsAnAuthenticatedOnlyRoute(t *testing.T) {
	_, store := newApprovalStore(false)
	ctx := context.Background()
	request := routedOwed("turn-1", "77", 0)
	request.Route.AuthenticatedOnly = true
	opened, err := store.Open(ctx, request)
	if err != nil || !opened.Route.AuthenticatedOnly {
		t.Fatalf("Open = %+v, %v", opened.Route, err)
	}
	unrouted := owed("turn-2", "alex")
	unrouted.Route.AuthenticatedOnly = true
	if _, err := store.Open(ctx, unrouted); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unrouted request cannot be authenticated-only: %v", err)
	}
}
