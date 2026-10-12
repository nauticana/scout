package dataplane

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// outcomeTable answers the call outcome store's statements by their SQL semantics over one turn:
// the guarded touch and the fenced writes match only the owner's fence on a running turn, every
// transition is a conditional write on its status, and a transaction's writes roll back together.
type outcomeTable struct {
	keelport.DatabaseRepository
	mu       sync.Mutex
	now      time.Time
	status   domain.TurnStatus
	fence    int64
	outcomes map[domain.CallOutcomeKey]outcomeRow
	attests  map[domain.CallOutcomeKey][]domain.CallAttestation
	journal  map[int][2]string
	links    map[domain.CallOutcomeKey]int64
	failOn   map[string]error
	calls    []string
	args     map[string][][]any
	commits  int
	rolled   int
	saved    *outcomeTable
}

type outcomeRow struct {
	outcome  domain.CallOutcome
	deadline time.Time
}

const outcomeTurnTenant, outcomeTurnRequest = int64(7), "request-1"

func newOutcomeTable(status domain.TurnStatus, fence int64) *outcomeTable {
	return &outcomeTable{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), status: status, fence: fence,
		outcomes: map[domain.CallOutcomeKey]outcomeRow{}, attests: map[domain.CallOutcomeKey][]domain.CallAttestation{},
		journal: map[int][2]string{}, links: map[domain.CallOutcomeKey]int64{}, failOn: map[string]error{}}
}

func (table *outcomeTable) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return table
}

func (table *outcomeTable) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	copied := &outcomeTable{outcomes: maps.Clone(table.outcomes), journal: maps.Clone(table.journal), links: maps.Clone(table.links),
		attests: map[domain.CallOutcomeKey][]domain.CallAttestation{}}
	for key, list := range table.attests {
		copied.attests[key] = slices.Clone(list)
	}
	table.saved = copied
	return table, nil
}

func (table *outcomeTable) QueryService(string, map[string]string) keelport.QueryService {
	return table
}
func (*outcomeTable) GenID() int64 { return 0 }

func (table *outcomeTable) Commit(context.Context) error {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.commits, table.saved = table.commits+1, nil
	return nil
}

func (table *outcomeTable) Rollback(context.Context) error {
	table.mu.Lock()
	defer table.mu.Unlock()
	if saved := table.saved; saved != nil {
		table.outcomes, table.attests, table.journal, table.links = saved.outcomes, saved.attests, saved.journal, saved.links
		table.saved = nil
	}
	table.rolled++
	return nil
}

func (table *outcomeTable) owned(tenantID, requestID, fence any) bool {
	return tenantID == outcomeTurnTenant && requestID == outcomeTurnRequest && fence == table.fence &&
		(table.status == domain.TurnRunning || table.status == domain.TurnStreaming)
}

func (table *outcomeTable) unworkedAt(fence any) bool {
	return fence == table.fence && (table.status == domain.TurnHeld || table.status.Terminal())
}

func outcomeKeyOf(args []any) domain.CallOutcomeKey {
	return domain.CallOutcomeKey{TenantID: args[0].(int64), RequestID: args[1].(string), ExecutionStepID: args[2].(int64), CallID: args[3].(string)}
}

func (table *outcomeTable) row(key domain.CallOutcomeKey) []any {
	row := table.outcomes[key]
	o := row.outcome
	var resolvedAt any
	if !o.ResolvedAt.IsZero() {
		resolvedAt = o.ResolvedAt
	}
	return []any{key.RequestID, key.ExecutionStepID, key.CallID, o.IdempotencyKey, o.Tool.ToolID, o.Tool.Version, string(o.Origin),
		string(o.Status), o.DispatchFence, o.DispatchedAt, row.deadline, !table.now.Before(row.deadline),
		o.Result.URI, o.Result.Digest, string(o.Resolution), o.ResolvedFence, string(o.Resolver.Kind), o.Resolver.ID, resolvedAt,
		o.Note, o.Evidence.URI, o.Evidence.Digest}
}

func (table *outcomeTable) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.calls = append(table.calls, name)
	if table.args == nil {
		table.args = map[string][][]any{}
	}
	table.args[name] = append(table.args[name], slices.Clone(args))
	if err := table.failOn[name]; err != nil {
		return nil, err
	}
	fenced := func(apply func([]any) bool) *keelmodel.QueryResult {
		if !table.owned(args[0], args[1], args[2]) {
			return rows([]any{int64(0), int64(0)})
		}
		if !apply(args[3:]) {
			return rows([]any{int64(1), int64(0)})
		}
		return rows([]any{int64(1), int64(1)})
	}
	settle := func(key domain.CallOutcomeKey, status, resolution string, result domain.ObjectRef, fence int64) bool {
		row, found := table.outcomes[key]
		if !found || !row.outcome.Status.Open() {
			return false
		}
		row.outcome.Status, row.outcome.Resolution, row.outcome.Result = domain.CallOutcomeStatus(status), domain.CallResolution(resolution), result
		row.outcome.ResolvedFence, row.outcome.ResolvedAt = fence, table.now
		table.outcomes[key] = row
		return true
	}
	ref := func(uri, digest any) domain.ObjectRef {
		return domain.ObjectRef{URI: fmt.Sprint(uri), Digest: fmt.Sprint(digest)}
	}
	switch name {
	case qOutcomeOwnerGuard:
		if table.owned(args[0], args[1], args[2]) {
			return rows([]any{int64(4)}), nil
		}
		return rows(), nil
	case qOutcomeIntend:
		return fenced(func(a []any) bool {
			key := outcomeKeyOf(a)
			if _, found := table.outcomes[key]; found {
				return false
			}
			for other, row := range table.outcomes {
				if other.TenantID == key.TenantID && row.outcome.IdempotencyKey == a[4] {
					return false
				}
			}
			table.outcomes[key] = outcomeRow{outcome: domain.CallOutcome{Key: key, IdempotencyKey: a[4].(string),
				Tool: domain.ToolReference{ToolID: a[5].(string), Version: a[6].(string)}, Origin: domain.CallFromAgent,
				Status: domain.CallInFlight, DispatchFence: a[7].(int64), DispatchedAt: table.now},
				deadline: table.now.Add(time.Duration(a[8].(int64)) * time.Second)}
			return true
		}), nil
	case qOutcomeRedispatch:
		return fenced(func(a []any) bool {
			key := outcomeKeyOf(a[2:])
			row, found := table.outcomes[key]
			if !found || !row.outcome.Status.Open() {
				return false
			}
			row.outcome.Status, row.outcome.DispatchFence, row.outcome.Result = domain.CallInFlight, a[0].(int64), domain.ObjectRef{}
			row.deadline = table.now.Add(time.Duration(a[1].(int64)) * time.Second)
			table.outcomes[key] = row
			return true
		}), nil
	case qOutcomeAccept:
		return fenced(func(a []any) bool {
			key := outcomeKeyOf(a[3:])
			row, found := table.outcomes[key]
			if !found || row.outcome.Status != domain.CallInFlight || row.outcome.DispatchFence != a[7] {
				return false
			}
			row.outcome.Status, row.outcome.Result = domain.CallAccepted, ref(a[0], a[1])
			if deadline := table.now.Add(time.Duration(a[2].(int64)) * time.Second); deadline.After(row.deadline) {
				row.deadline = deadline
			}
			table.outcomes[key] = row
			return true
		}), nil
	case qOutcomeMarkUnknown:
		return fenced(func(a []any) bool {
			key := outcomeKeyOf(a)
			row, found := table.outcomes[key]
			if !found || row.outcome.Status != domain.CallInFlight && row.outcome.Status != domain.CallAccepted {
				return false
			}
			row.outcome.Status = domain.CallUnknown
			table.outcomes[key] = row
			return true
		}), nil
	case qOutcomeLink:
		table.links[outcomeKeyOf(args)] = args[4].(int64)
		return rows(), nil
	case qOutcomeSettle:
		if settle(outcomeKeyOf(args[5:]), args[0].(string), args[1].(string), ref(args[2], args[3]), args[4].(int64)) {
			return rows([]any{args[8]}), nil
		}
		return rows(), nil
	case qOutcomeJournal:
		if _, found := table.journal[args[3].(int)]; found {
			return rows(), nil
		}
		table.journal[args[3].(int)] = [2]string{args[4].(string), args[5].(string)}
		return rows([]any{args[3]}), nil
	case qLoopEntryGet:
		if entry, found := table.journal[args[3].(int)]; found {
			return rows([]any{int64(args[3].(int)), entry[0], entry[1]}), nil
		}
		return rows(), nil
	case qOutcomeRetract:
		key := outcomeKeyOf(args)
		if row, found := table.outcomes[key]; found && row.outcome.Status == domain.CallInFlight && row.outcome.DispatchFence == args[4] && len(table.attests[key]) == 0 {
			delete(table.outcomes, key)
			return rows([]any{key.CallID}), nil
		}
		return rows(), nil
	case qOutcomeReport:
		key := outcomeKeyOf(args[3:])
		if row, found := table.outcomes[key]; found && row.outcome.DispatchFence == args[7] &&
			settle(key, args[0].(string), string(domain.ResolvedByWorker), ref(args[1], args[2]), row.outcome.DispatchFence) {
			return rows([]any{key.CallID}), nil
		}
		return rows(), nil
	case qOutcomeFind:
		if _, found := table.outcomes[outcomeKeyOf(args)]; found {
			return rows(table.row(outcomeKeyOf(args))), nil
		}
		return rows(), nil
	case qOutcomeList:
		var listed [][]any
		for key := range table.outcomes {
			if key.TenantID == args[0] && key.RequestID == args[1] {
				listed = append(listed, table.row(key))
			}
		}
		return &keelmodel.QueryResult{Rows: listed}, nil
	case qOutcomeResolve:
		key := outcomeKeyOf(args[7:])
		if !table.unworkedAt(args[11]) {
			return rows(), nil
		}
		if settle(key, args[0].(string), args[1].(string), ref(args[2], args[3]), args[4].(int64)) {
			row := table.outcomes[key]
			row.outcome.Resolver = domain.PrincipalRef{Kind: domain.PrincipalKind(args[5].(string)), ID: args[6].(string)}
			table.outcomes[key] = row
			return rows([]any{key.CallID}), nil
		}
		return rows(), nil
	case qOutcomeAttestTouch:
		key := outcomeKeyOf(args)
		if row, found := table.outcomes[key]; found && row.outcome.Status.Open() && !table.now.Before(row.deadline) && table.unworkedAt(args[4]) {
			return rows([]any{key.CallID}), nil
		}
		return rows(), nil
	case qOutcomeAttestState:
		key := outcomeKeyOf(args[1:])
		row, found := table.outcomes[key]
		if !found {
			return rows(), nil
		}
		o := row.outcome
		return rows([]any{string(o.Status), !table.now.Before(row.deadline), string(o.Resolution), string(o.Resolver.Kind), o.Resolver.ID, table.unworkedAt(args[0])}), nil
	case qOutcomeAttestations:
		var listed [][]any
		for _, stated := range table.attests[outcomeKeyOf(args)] {
			listed = append(listed, []any{string(stated.Attester.Kind), stated.Attester.ID, string(stated.Outcome), stated.Note, stated.Evidence.URI, stated.Evidence.Digest})
		}
		return &keelmodel.QueryResult{Rows: listed}, nil
	case qOutcomeAttestInsert:
		key := outcomeKeyOf(args)
		person := domain.PrincipalRef{Kind: domain.PrincipalKind(args[4].(string)), ID: args[5].(string)}
		for _, stated := range table.attests[key] {
			if stated.Attester == person {
				return rows(), nil
			}
		}
		uri, digest := "", ""
		if args[8] != nil {
			uri, digest = args[8].(string), args[9].(string)
		}
		table.attests[key] = append(table.attests[key], domain.CallAttestation{Key: key, Attester: person,
			Outcome: domain.CallOutcomeStatus(args[6].(string)), Note: args[7].(string), Evidence: domain.ObjectRef{URI: uri, Digest: digest}})
		return rows(), nil
	case qOutcomeAttestResolve:
		key := outcomeKeyOf(args[7:])
		if settle(key, args[0].(string), string(domain.ResolvedByAttestation), table.outcomes[key].outcome.Result, args[1].(int64)) {
			row := table.outcomes[key]
			row.outcome.Resolver = domain.PrincipalRef{Kind: domain.PrincipalKind(args[2].(string)), ID: args[3].(string)}
			row.outcome.Note = args[4].(string)
			if args[5] != nil {
				row.outcome.Evidence = domain.ObjectRef{URI: args[5].(string), Digest: args[6].(string)}
			}
			table.outcomes[key] = row
			return rows([]any{key.CallID}), nil
		}
		return rows(), nil
	case qOutcomeConfirmSettled:
		var settled [][]any
		for key, id := range table.links {
			if key.TenantID == args[3] && id == args[4] && settle(key, args[0].(string), string(domain.ResolvedByConfirmation), domain.ObjectRef{}, table.fence) {
				row := table.outcomes[key]
				row.outcome.Resolver = domain.PrincipalRef{Kind: domain.PrincipalKind(args[1].(string)), ID: args[2].(string)}
				table.outcomes[key] = row
				settled = append(settled, []any{key.CallID})
			}
		}
		return &keelmodel.QueryResult{Rows: settled}, nil
	}
	return rows(), nil
}

func (table *outcomeTable) statusOf(key domain.CallOutcomeKey) domain.CallOutcomeStatus {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.outcomes[key].outcome.Status
}
