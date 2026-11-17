package dataplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qSweepDueCalls = "scout_sweep_due_calls"

// turn_call_outcome_due_ix serves the order; the cursor is the last call of the previous sweep.
var sweepQueries = map[string]string{
	qOperateDispatch: operateQueries[qOperateDispatch],
	qOperateRelease:  operateQueries[qOperateRelease],
	qSweepDueCalls: `
SELECT o.partner_id, o.request_id, o.execution_step_id, o.call_id, turn.fence, conv.agent_id, o.deadline_at
  FROM turn_call_outcome o
  JOIN conversation_turn turn ON turn.partner_id = o.partner_id AND turn.request_id = o.request_id
  JOIN agent_conversation conv ON conv.partner_id = turn.partner_id AND conv.conversation_id = turn.conversation_id
 WHERE o.status_code IN (` + sqlCallOpen + `) AND o.deadline_at <= CURRENT_TIMESTAMP AND turn.status_code IN (` + sqlTurnUnworked + `)
   AND (o.deadline_at, o.partner_id, o.request_id, o.execution_step_id, o.call_id) > (?, ?, ?, ?, ?)
 ORDER BY o.deadline_at, o.partner_id, o.request_id, o.execution_step_id, o.call_id
 LIMIT ?`,
}

// CallOutcomeSweeper settles, across tenants, the open calls of held or ended turns whose deadline
// passed by the store clock: it asks the reconciler about each as an operator's lookup does, and
// resolves a definite answer as domain.PlatformPrincipal, audited call_outcome_resolved. Anything
// else stays open for a person. Each sweep continues after the last call the previous one asked
// about and starts over once none is left, so a call that stays unknown cannot starve the rest.
type CallOutcomeSweeper struct {
	DB         port.DatabaseRepository
	Outcomes   contract.CallOutcomeStore
	Journal    contract.LoopJournal
	Reconciler contract.ToolOutcomeReconciler
	// Control and Principals rebuild a call a person made from their operator entry.
	Control    contract.TurnControl
	Principals contract.PrincipalResolver
	// Audit records each resolution; nil records none.
	Audit contract.AuditSink
	Now   func() time.Time

	once  sync.Once
	qs    port.QueryService
	mu    sync.Mutex
	after domain.OpenOutcome
}

// Sweep asks about at most limit due calls and returns how many it settled. A call a person or
// another sweep settled first, or whose turn moved on, is skipped; any other failure is collected
// and the sweep continues.
func (s *CallOutcomeSweeper) Sweep(ctx context.Context, limit int) (int, error) {
	if s.DB == nil || s.Outcomes == nil || s.Journal == nil || s.Reconciler == nil || s.Control == nil || s.Principals == nil {
		return 0, fmt.Errorf("%w: call outcome sweeper needs a database, call outcomes, journal, reconciler, turn control, and principal resolver", domain.ErrNotReady)
	}
	if limit < 1 {
		return 0, fmt.Errorf("%w: call outcome sweep limit must be positive, got %d", domain.ErrValidation, limit)
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, sweepQueries) })
	s.mu.Lock()
	defer s.mu.Unlock()
	after := s.after
	if after.DeadlineAt.IsZero() {
		after.DeadlineAt = time.Unix(0, 0).UTC()
	}
	due, err := s.qs.Query(ctx, qSweepDueCalls, after.DeadlineAt, after.Key.TenantID, after.Key.RequestID, after.Key.ExecutionStepID, after.Key.CallID, limit)
	if err != nil {
		return 0, fmt.Errorf("list due call outcomes: %w", err)
	}
	s.after = domain.OpenOutcome{}
	if len(due.Rows) == limit {
		last := due.Rows[len(due.Rows)-1]
		s.after = domain.OpenOutcome{DeadlineAt: common.AsTime(last[6]), Key: domain.CallOutcomeKey{TenantID: common.AsInt64(last[0]),
			RequestID: common.AsString(last[1]), ExecutionStepID: common.AsInt64(last[2]), CallID: common.AsString(last[3])}}
	}
	calls := dispatchedCalls{qs: s.qs, Outcomes: s.Outcomes, Journal: s.Journal, Control: s.Control, Principals: s.Principals,
		Reconciler: s.Reconciler, Audit: s.Audit, Now: s.Now}
	settled := 0
	var errs []error
	for _, row := range due.Rows {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		key := domain.CallOutcomeKey{TenantID: common.AsInt64(row[0]), RequestID: common.AsString(row[1]),
			ExecutionStepID: common.AsInt64(row[2]), CallID: common.AsString(row[3])}
		outcome, found, err := s.Outcomes.Find(ctx, key)
		if err == nil && found && outcome.Status.Open() {
			var done bool
			_, done, err = calls.settle(ctx, outcome, common.AsString(row[5]), common.AsInt64(row[4]), domain.PlatformPrincipal)
			if done {
				settled++
			}
		}
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			errs = append(errs, fmt.Errorf("call %q of turn %q: %w", key.CallID, key.RequestID, err))
		}
	}
	return settled, errors.Join(errs...)
}
