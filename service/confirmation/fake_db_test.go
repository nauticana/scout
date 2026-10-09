package confirmation

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"
)

// confirmationRow is one mcp_confirmation row as the fake keeps it.
type confirmationRow struct {
	id, tenant                                   int64
	makerKind, makerID, client, tool             string
	payload, digest, preview, requirements       string
	channel, status                              string
	approvalRequired                             bool
	approvalID, eventID, fence, attempts         int64
	expires, created, decided, leaseUntil        time.Time
	completed                                    time.Time
	deciderKind, deciderID, note, result, reason string
	reconcilerKind, reconcilerID, reconcileNote  string
}

func (row *confirmationRow) open() bool {
	switch row.status {
	case "pending", "approved", "executing", "unknown":
		return true
	}
	return false
}

func (row *confirmationRow) values(now time.Time) []any {
	text := func(value string) any { return value }
	return []any{row.id, row.tenant, row.makerKind, row.makerID, text(row.client), row.tool, text(row.payload),
		row.digest, text(row.preview), row.requirements, row.channel, row.status, row.approvalRequired, row.approvalID,
		row.expires, row.created, timeOrNil(row.decided), row.deciderKind, row.deciderID, row.note, row.fence,
		row.attempts, row.result, row.reason, timeOrNil(row.completed), row.reconcilerKind, row.reconcilerID,
		row.reconcileNote, row.status == "pending" && !row.expires.After(now)}
}

func timeOrNil(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

// confirmationDB interprets the store's named queries over rows; now is the store clock.
type confirmationDB struct {
	keelport.DatabaseRepository
	mu         sync.Mutex
	rows       map[int64]*confirmationRow
	next       int64
	now        time.Time
	events     []string
	catalogNil bool
	// renewErr makes every renewal fail, as when another executor took the claim.
	renewErr bool
}

func newConfirmationDB() *confirmationDB {
	return &confirmationDB{rows: map[int64]*confirmationRow{}, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
}

func (db *confirmationDB) advance(by time.Duration) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.now = db.now.Add(by)
}

func (db *confirmationDB) row(id int64) confirmationRow {
	db.mu.Lock()
	defer db.mu.Unlock()
	return *db.rows[id]
}

func (db *confirmationDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db
}

func (db *confirmationDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db, nil
}

func (db *confirmationDB) QueryService(string, map[string]string) keelport.QueryService {
	if db.catalogNil {
		return nil
	}
	return db
}
func (db *confirmationDB) GenID() int64                   { return int64(len(db.events) + 1000) }
func (db *confirmationDB) Commit(context.Context) error   { return nil }
func (db *confirmationDB) Rollback(context.Context) error { return nil }

func rowsOf(values ...[]any) *keelmodel.QueryResult { return &keelmodel.QueryResult{Rows: values} }

func (db *confirmationDB) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	str := func(i int) string {
		if args[i] == nil {
			return ""
		}
		return args[i].(string)
	}
	id := func(i int) int64 { return args[i].(int64) }
	find := func(tenant, rowID int64) *confirmationRow {
		if row, ok := db.rows[rowID]; ok && row.tenant == tenant {
			return row
		}
		return nil
	}
	seconds := func(i int) time.Duration { return time.Duration(args[i].(int64)) * time.Second }
	switch name {
	case qConfirmInsert:
		for _, row := range db.rows {
			if row.open() && row.tenant == id(0) && row.tool == str(4) && row.digest == str(6) {
				return rowsOf(), nil
			}
		}
		db.next++
		db.rows[db.next] = &confirmationRow{id: db.next, tenant: id(0), makerKind: str(1), makerID: str(2), client: str(3),
			tool: str(4), payload: str(5), digest: str(6), preview: str(7), requirements: str(8), channel: str(9),
			approvalRequired: args[10].(bool), status: "pending", created: db.now, expires: db.now.Add(seconds(11))}
		return rowsOf([]any{db.next}), nil
	case qConfirmOpen:
		for _, row := range db.rows {
			if row.open() && row.tenant == id(0) && row.tool == str(1) && row.digest == str(2) {
				return rowsOf(row.values(db.now)), nil
			}
		}
	case qConfirmGet:
		if row := find(id(0), id(1)); row != nil {
			return rowsOf(row.values(db.now)), nil
		}
	case "keel_outbox_insert":
		db.events = append(db.events, fmt.Sprint(args[2], "/", args[4]))
	case qConfirmNotified:
		if row := find(id(1), id(2)); row != nil && row.eventID == 0 {
			row.eventID = id(0)
		}
	case qConfirmAttach:
		if row := find(id(1), id(2)); row != nil && row.status == "pending" && row.approvalRequired && row.approvalID == 0 && row.expires.After(db.now) {
			row.approvalID = id(0)
			return rowsOf([]any{row.id}), nil
		}
	case qConfirmAbandon:
		if row := find(id(1), id(2)); row != nil && row.status == "pending" {
			row.status, row.reason, row.completed = "failed", str(0), db.now
			return rowsOf([]any{row.id}), nil
		}
	case qConfirmDecide:
		if row := find(id(4), id(5)); row != nil && row.status == "pending" && !row.approvalRequired && row.approvalID == 0 && row.expires.After(db.now) {
			row.status, row.deciderKind, row.deciderID, row.note, row.decided = str(0), str(1), str(2), str(3), db.now
			return rowsOf([]any{row.id}), nil
		}
	case qConfirmDecideApproval:
		if row := find(id(4), id(5)); row != nil && row.status == "pending" && row.approvalID == id(6) && row.expires.After(db.now) {
			row.status, row.deciderKind, row.deciderID, row.note, row.decided = str(0), str(1), str(2), str(3), db.now
			return rowsOf([]any{row.id}), nil
		}
	case qConfirmWithdraw:
		if row := find(id(0), id(1)); row != nil && row.status == "pending" && row.makerKind == str(2) && row.makerID == str(3) {
			row.status, row.deciderKind, row.deciderID, row.decided = "withdrawn", row.makerKind, row.makerID, db.now
			return rowsOf([]any{row.approvalID}), nil
		}
	case qConfirmApproved:
		var out [][]any
		for rowID := int64(1); rowID <= db.next; rowID++ {
			if row := db.rows[rowID]; row.status == "approved" {
				out = append(out, []any{row.tenant, row.id})
			}
		}
		return rowsOf(out...), nil
	case qConfirmClaim:
		if row := find(id(1), id(2)); row != nil && row.status == "approved" {
			row.status, row.leaseUntil = "executing", db.now.Add(seconds(0))
			row.fence++
			row.attempts++
			return rowsOf([]any{row.fence}), nil
		}
	case qConfirmRenew:
		if row := find(id(1), id(2)); row != nil && !db.renewErr && row.status == "executing" && row.fence == id(3) {
			row.leaseUntil = db.now.Add(seconds(0))
			return rowsOf([]any{row.id}), nil
		}
	case qConfirmComplete:
		if row := find(id(4), id(5)); row != nil && row.status == "executing" && row.fence == id(6) {
			row.status, row.result, row.reason, row.leaseUntil = str(0), str(1), str(2), time.Time{}
			if row.status != "unknown" {
				row.completed = db.now
			}
			return rowsOf([]any{row.id}), nil
		}
	case qConfirmLapse:
		var candidates []*confirmationRow
		for _, row := range db.rows {
			if row.status == "executing" && !row.leaseUntil.After(db.now) {
				candidates = append(candidates, row)
			}
		}
		slices.SortFunc(candidates, func(a, b *confirmationRow) int {
			return cmp.Or(a.leaseUntil.Compare(b.leaseUntil), cmp.Compare(a.id, b.id))
		})
		var out [][]any
		for _, row := range candidates[:min(args[0].(int), len(candidates))] {
			row.status, row.leaseUntil = "unknown", time.Time{}
			out = append(out, []any{row.id})
		}
		return rowsOf(out...), nil
	case qConfirmExpire:
		var out [][]any
		for _, row := range db.rows {
			if row.status == "pending" && !row.expires.After(db.now) {
				row.status, row.decided = "expired", db.now
				out = append(out, []any{row.id, row.tenant, row.makerKind, row.makerID, row.tool, row.approvalID})
			}
		}
		return rowsOf(out...), nil
	case qConfirmReconcile:
		if row := find(id(4), id(5)); row != nil && row.status == "unknown" {
			row.status, row.reconcilerKind, row.reconcilerID, row.reconcileNote, row.completed = str(0), str(1), str(2), str(3), db.now
			return rowsOf([]any{row.id}), nil
		}
	default:
		return nil, fmt.Errorf("unexpected query %s", name)
	}
	return rowsOf(), nil
}
