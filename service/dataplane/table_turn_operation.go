package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

const (
	qOperatorTakeover    = "scout_turn_operator_takeover"
	qOperatorGuard       = "scout_turn_operator_guard"
	qOperatorState       = "scout_turn_operator_state"
	qOperatorPosition    = "scout_turn_operator_position"
	qOperatorIntend      = "scout_turn_operator_intend"
	qOperatorEntryInsert = "scout_turn_operator_entry_insert"
	qOperatorSettleCall  = "scout_turn_operator_settle_call"
	qOperatorUnknownCall = "scout_turn_operator_unknown_call"
	qOperatorHoldUnknown = "scout_turn_operator_hold_unknown"
	qOperatorHandback    = "scout_turn_operator_handback"
	qOperatorFinish      = "scout_turn_operator_finish"
	qOperatorEntries     = "scout_turn_operator_entries"
	qOperatorSteps       = "scout_turn_operator_steps"
	qOperatorLapse       = "scout_turn_operator_lapse"
	qOperatorCallKnown   = "scout_turn_operator_call_known"

	// DefaultTakeoverLease, DefaultOperatorEntryMaxBytes, DefaultOperatorMaxEntries,
	// DefaultHoldLapseBatch, and DefaultHoldLapsePeriod are what a zero OperatorSettings takes.
	DefaultTakeoverLease         = 900 * time.Second
	DefaultOperatorEntryMaxBytes = 16 << 10
	DefaultOperatorMaxEntries    = 64
	DefaultHoldLapseBatch        = 100
	DefaultHoldLapsePeriod       = 30 * time.Second
	// MaxTakeoverLease, MaxOperatorEntryBytes, MaxOperatorEntries, MaxHoldLapseBatch, and
	// MaxHoldLapsePeriod bound the operator settings.
	MaxTakeoverLease      = 24 * time.Hour
	MaxOperatorEntryBytes = 1 << 20
	MaxOperatorEntries    = 4096
	MaxHoldLapseBatch     = 10000
	MaxHoldLapsePeriod    = time.Hour
)

// inConversationOrder gates a transition of the turn aliased turn on no earlier turn of its
// conversation being live, so a person never acts ahead of the conversation's order.
const inConversationOrder = `
   AND NOT EXISTS (SELECT 1 FROM conversation_turn earlier
                    WHERE earlier.partner_id = turn.partner_id AND earlier.conversation_id = turn.conversation_id
                      AND earlier.turn_no < turn.turn_no AND earlier.status_code IN (` + sqlTurnLive + `))`

// operatorHolds matches the turn aliased turn while the lease named by the next five placeholders
// (tenant, request, fence, holder kind, holder id) is live by the store clock.
const operatorHolds = `turn.partner_id = ? AND turn.request_id = ? AND turn.fence = ? AND turn.status_code = ` + sqlTurnOperated + `
   AND turn.holder_kind = ? AND turn.holder_id = ? AND turn.hold_until > CURRENT_TIMESTAMP`

const operatorEntryColumns = `
SELECT e.fence, e.entry_no, e.kind_code, e.actor_kind, e.actor_id, e.step_no, COALESCE(e.execution_step_id, 0), e.after_entry_no,
       e.entry_uri, e.entry_digest, e.input_tokens, e.output_tokens, e.tool_calls, e.cost_minor_units,
       e.search_queries, COALESCE(e.currency_code, ''), COALESCE(c.exponent, 0), e.created_at
  FROM turn_operator_entry e
  LEFT JOIN currency c ON c.code = e.currency_code`

var operatorQueries = map[string]string{
	qOperatorTakeover: `
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnOperated + `, fence = turn.fence + 1, holder_kind = ?, holder_id = ?,
       hold_until = CURRENT_TIMESTAMP + make_interval(secs => ?)
 WHERE turn.partner_id = ? AND turn.request_id = ? AND turn.status_code = ` + sqlTurnHeld + ` AND turn.fence = ?` + noOpenOutcome + inConversationOrder + `
RETURNING turn.fence, turn.hold_until`,
	// The holder's guarded touch renews the lease; it is the first statement of every operator
	// transaction, so a lapse or another transition linearizes before or after the whole act.
	qOperatorGuard: `
UPDATE conversation_turn turn SET hold_until = CURRENT_TIMESTAMP + make_interval(secs => ?)
 WHERE ` + operatorHolds + `
RETURNING turn.turn_no, turn.conversation_id, turn.hold_until`,
	qOperatorState: `
SELECT status_code, fence, COALESCE(holder_kind, ''), COALESCE(holder_id, ''), COALESCE(hold_until <= CURRENT_TIMESTAMP, FALSE),
       turn_no, conversation_id
  FROM conversation_turn
 WHERE partner_id = ? AND request_id = ?`,
	// Where the turn stands for its operator: committed steps, the step in progress and its
	// journal length, the latest step a call can be filed under, the frames published, the replies
	// and entries made, and the operator calls still open.
	qOperatorPosition: `
SELECT (SELECT COUNT(*) FROM step_idempotency s
         WHERE s.partner_id = t.partner_id AND s.request_id = t.request_id AND s.status_code = ` + sqlStepCommitted + `),
       COALESCE(open_step.execution_step_id, 0),
       (SELECT COUNT(*) FROM step_loop_entry e
         WHERE e.partner_id = t.partner_id AND e.request_id = t.request_id AND e.execution_step_id = open_step.execution_step_id),
       COALESCE((SELECT MAX(s.execution_step_id) FROM step_idempotency s WHERE s.partner_id = t.partner_id AND s.request_id = t.request_id), 0),
       CASE WHEN snap.latest_turn_no = t.turn_no THEN snap.latest_step_no ELSE 0 END,
       (SELECT COUNT(*) FROM turn_operator_entry y
         WHERE y.partner_id = t.partner_id AND y.request_id = t.request_id AND y.kind_code = ` + sqlOperatorReply + `),
       COALESCE((SELECT MAX(x.entry_no) FROM turn_operator_entry x WHERE x.partner_id = t.partner_id AND x.request_id = t.request_id), 0),
       (SELECT COUNT(*) FROM turn_call_outcome o
         WHERE o.partner_id = t.partner_id AND o.request_id = t.request_id AND o.origin_code = ` + sqlCallFromOperator + `
           AND o.status_code IN (` + sqlCallOpen + `))
  FROM conversation_turn t
  LEFT JOIN LATERAL (SELECT s.execution_step_id FROM step_idempotency s
                      WHERE s.partner_id = t.partner_id AND s.request_id = t.request_id
                        AND s.status_code IN (` + sqlStepClaimed + `, ` + sqlStepAbandoned + `)
                      ORDER BY s.execution_step_id DESC
                      LIMIT 1) open_step ON TRUE
  LEFT JOIN session_snapshot snap ON snap.partner_id = t.partner_id AND snap.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND t.request_id = ?`,
	qOperatorIntend: `
INSERT INTO turn_call_outcome (partner_id, request_id, execution_step_id, call_id, idempotency_key, tool_id, tool_version,
                               origin_code, status_code, dispatch_fence, dispatched_at, deadline_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ` + sqlCallFromOperator + `, ` + sqlCallInFlight + `, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP + make_interval(secs => ?))
ON CONFLICT DO NOTHING
RETURNING call_id`,
	qOperatorEntryInsert: `
INSERT INTO turn_operator_entry (partner_id, request_id, fence, entry_no, kind_code, actor_kind, actor_id, step_no,
                                 execution_step_id, after_entry_no, entry_uri, entry_digest,
                                 input_tokens, output_tokens, tool_calls, cost_minor_units, search_queries, currency_code)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING
RETURNING created_at, COALESCE((SELECT c.exponent FROM currency c WHERE c.code = turn_operator_entry.currency_code), 0)`,
	qOperatorSettleCall: `
UPDATE turn_call_outcome
   SET status_code = ?, resolution_code = ` + sqlResolvedByGateway + `, result_uri = ?, result_digest = ?,
       resolved_fence = dispatch_fence, resolved_at = CURRENT_TIMESTAMP
 WHERE ` + outcomeKey + ` AND origin_code = ` + sqlCallFromOperator + ` AND dispatch_fence = ? AND status_code = ` + sqlCallInFlight + `
RETURNING call_id`,
	qOperatorUnknownCall: `
UPDATE turn_call_outcome SET status_code = ` + sqlCallUnknown + `
 WHERE ` + outcomeKey + ` AND origin_code = ` + sqlCallFromOperator + ` AND dispatch_fence = ? AND status_code = ` + sqlCallInFlight + `
RETURNING call_id`,
	// A call nobody can establish holds the turn again, as the agent's own does.
	qOperatorHoldUnknown: `
UPDATE conversation_turn
   SET status_code = ` + sqlTurnHeld + `, fence = fence + 1, hold_code = ` + sqlHoldUnknownOutcome + `, held_at = CURRENT_TIMESTAMP,
       holder_kind = NULL, holder_id = NULL, hold_until = NULL
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code = ` + sqlTurnOperated + `
RETURNING fence`,
	qOperatorHandback: `
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnQueued + `, fence = turn.fence + 1, ` + unheld + `
 WHERE ` + operatorHolds + noOpenOutcome + `
RETURNING turn.fence`,
	// The holder stays on the completed turn: it says a person, not the agent, authored the response.
	qOperatorFinish: `
WITH finished AS (
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnCompleted + `, response_uri = ?, response_digest = ?,
       started_at = COALESCE(turn.started_at, CURRENT_TIMESTAMP), completed_at = CURRENT_TIMESTAMP,
       hold_code = NULL, held_at = NULL, hold_requested_at = NULL, hold_reason = NULL, hold_until = NULL
 WHERE ` + operatorHolds + noOpenOutcome + inConversationOrder + `
RETURNING turn.partner_id, turn.conversation_id, turn.turn_no),
mirrored AS (
UPDATE conversation_turn_detail detail
   SET result_kind = ?, result_payload = ?, error_text = NULL
  FROM finished
 WHERE detail.partner_id = finished.partner_id AND detail.conversation_id = finished.conversation_id AND detail.turn_no = finished.turn_no
RETURNING detail.turn_no)
SELECT turn_no FROM finished`,
	qOperatorEntries: operatorEntryColumns + `
 WHERE e.partner_id = ? AND e.request_id = ?
 ORDER BY e.entry_no
 LIMIT ?`,
	qOperatorCallKnown: `
SELECT 1 FROM turn_call_outcome WHERE ` + outcomeKey,
	qOperatorSteps: `
SELECT execution_step_id, status_code, COALESCE(result_uri, ''), COALESCE(result_digest, '')
  FROM step_idempotency
 WHERE partner_id = ? AND request_id = ?
 ORDER BY execution_step_id`,
	// Lapsed leases are held again in bounded batches by the store clock; rows another sweeper
	// already locked are skipped, as the MCP confirmation lapse does.
	qOperatorLapse: `
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnHeld + `, fence = turn.fence + 1, holder_kind = NULL, holder_id = NULL, hold_until = NULL
  FROM (SELECT partner_id, conversation_id, turn_no, holder_kind, holder_id
          FROM conversation_turn
         WHERE status_code = ` + sqlTurnOperated + ` AND hold_until <= CURRENT_TIMESTAMP
         ORDER BY hold_until, partner_id, request_id
         LIMIT ?
           FOR UPDATE SKIP LOCKED) lapsed
 WHERE turn.partner_id = lapsed.partner_id AND turn.conversation_id = lapsed.conversation_id AND turn.turn_no = lapsed.turn_no
   AND turn.status_code = ` + sqlTurnOperated + ` AND turn.hold_until <= CURRENT_TIMESTAMP
RETURNING turn.partner_id, turn.request_id, turn.fence, lapsed.holder_kind, lapsed.holder_id,
          (SELECT c.agent_id FROM agent_conversation c WHERE c.partner_id = turn.partner_id AND c.conversation_id = turn.conversation_id)`,
}

// operationQueries are every statement an operator transaction may run: the turn control's own,
// the history and usage statements a finish writes, and the operator usage it aggregates.
var operationQueries = common.MergeMaps(turnControlQueries, operatorQueries, TurnLedgerQueries(), turnSettlementQueries)

// operatorPosition is where the operated turn stands, read under the holder's guarded touch.
type operatorPosition struct {
	committed      int
	openStep       int64
	journal        int
	latestStep     int64
	published      int64
	replies        int64
	lastEntry      int
	openCalls      int64
	turnNo         int64
	conversationID string
}

// entryPosition is where an entry made now reaches the agent: the step in progress at its journal
// length, else the next step to start.
func (position operatorPosition) entryPosition() domain.OperatorPosition {
	return domain.OperatorPosition{StepNo: position.committed + 1, ExecutionStepID: position.openStep, AfterEntryNo: position.journal}
}

// replySequence is the reply frame sequence a reply made now takes: one frame per step published,
// one per reply made before it.
func (position operatorPosition) replySequence() int64 { return position.published + position.replies }

// operation resolves the operator settings, refusing a negative or out-of-range one.
func (control *TableTurnControl) operation() (domain.OperatorSettings, error) {
	settings := control.Operation
	if settings.Lease < 0 || settings.Lease > MaxTakeoverLease || settings.EntryMaxBytes < 0 || settings.EntryMaxBytes > MaxOperatorEntryBytes ||
		settings.MaxEntries < 0 || settings.MaxEntries > MaxOperatorEntries || settings.LapseBatch < 0 || settings.LapseBatch > MaxHoldLapseBatch ||
		settings.LapsePeriod < 0 || settings.LapsePeriod > MaxHoldLapsePeriod {
		return domain.OperatorSettings{}, fmt.Errorf("%w: operator settings are outside their ranges", domain.ErrValidation)
	}
	settings.Lease = positiveOr(settings.Lease, DefaultTakeoverLease)
	settings.EntryMaxBytes = positiveOr(settings.EntryMaxBytes, DefaultOperatorEntryMaxBytes)
	settings.MaxEntries = positiveOr(settings.MaxEntries, DefaultOperatorMaxEntries)
	settings.LapseBatch = positiveOr(settings.LapseBatch, DefaultHoldLapseBatch)
	settings.LapsePeriod = positiveOr(settings.LapsePeriod, DefaultHoldLapsePeriod)
	return settings, nil
}

// operating checks what every operator write needs and returns the resolved settings.
func (control *TableTurnControl) operating(ctx context.Context) (domain.OperatorSettings, error) {
	if _, err := control.queries(ctx); err != nil {
		return domain.OperatorSettings{}, err
	}
	if control.Objects == nil {
		return domain.OperatorSettings{}, fmt.Errorf("%w: turn operation needs an object codec", domain.ErrNotReady)
	}
	return control.operation()
}

func leaseSeconds(lease time.Duration) int64 { return int64((lease-1)/time.Second + 1) }

// TakeOver moves the turn held at the takeover's fence to operated by its person.
func (control *TableTurnControl) TakeOver(ctx context.Context, takeover domain.TurnTakeover) (domain.OperatorHold, error) {
	lease := domain.OperatorLease{TenantID: takeover.TenantID, RequestID: takeover.RequestID, Fence: takeover.Fence, Holder: takeover.Holder}
	if err := lease.Check(); err != nil {
		return domain.OperatorHold{}, fmt.Errorf("%w: a takeover names the turn, its held fence, and its person", domain.ErrValidation)
	}
	settings, err := control.operating(ctx)
	if err != nil {
		return domain.OperatorHold{}, err
	}
	ctx = context.WithoutCancel(ctx)
	taken, err := control.qs.Query(ctx, qOperatorTakeover, string(takeover.Holder.Kind), takeover.Holder.ID, leaseSeconds(settings.Lease),
		takeover.TenantID, takeover.RequestID, takeover.Fence)
	if err != nil {
		return domain.OperatorHold{}, fmt.Errorf("take over turn: %w", err)
	}
	if len(taken.Rows) == 0 {
		return domain.OperatorHold{}, fmt.Errorf("%w: turn %q is not held at fence %d, a call of it has an open outcome, or an earlier turn of its conversation is live",
			domain.ErrConflict, takeover.RequestID, takeover.Fence)
	}
	lease.Fence = common.AsInt64(taken.Rows[0][0])
	until, _ := common.AsTimeOK(taken.Rows[0][1])
	return domain.OperatorHold{Lease: lease, HoldUntil: until}, nil
}

// operatorTx opens a transaction whose first statement is the holder's guarded touch, renewing the
// lease, and reads where the turn stands; a lease that lapsed or a fence that moved is ErrFenceLost.
func (control *TableTurnControl) operatorTx(ctx context.Context, lease domain.OperatorLease, settings domain.OperatorSettings) (keelport.TxQueryService, operatorPosition, error) {
	tx, err := control.DB.BeginTx(ctx, operationQueries)
	if err != nil {
		return nil, operatorPosition{}, fmt.Errorf("begin turn operation: %w", err)
	}
	guarded, err := tx.Query(ctx, qOperatorGuard, leaseSeconds(settings.Lease),
		lease.TenantID, lease.RequestID, lease.Fence, string(lease.Holder.Kind), lease.Holder.ID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, operatorPosition{}, fmt.Errorf("guard operated turn: %w", err)
	}
	if len(guarded.Rows) == 0 {
		_ = tx.Rollback(ctx)
		return nil, operatorPosition{}, control.refusedLease(ctx, lease)
	}
	position, err := readPosition(ctx, tx, lease)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, operatorPosition{}, err
	}
	position.turnNo, position.conversationID = common.AsInt64(guarded.Rows[0][0]), common.AsString(guarded.Rows[0][1])
	return tx, position, nil
}

// refusedLease says why a lease no longer holds its turn.
func (control *TableTurnControl) refusedLease(ctx context.Context, lease domain.OperatorLease) error {
	state, err := control.qs.Query(ctx, qOperatorState, lease.TenantID, lease.RequestID)
	if err != nil {
		return fmt.Errorf("read operated turn: %w", err)
	}
	if len(state.Rows) == 0 {
		return fmt.Errorf("%w: turn request %q", domain.ErrNotFound, lease.RequestID)
	}
	row := state.Rows[0]
	if domain.TurnStatus(common.AsString(row[0])) == domain.TurnOperated && common.AsInt64(row[1]) == lease.Fence && common.AsBool(row[4]) {
		return fmt.Errorf("%w: the lease of turn %q at fence %d lapsed", domain.ErrFenceLost, lease.RequestID, lease.Fence)
	}
	return fmt.Errorf("%w: %s holds no lease on turn %q at fence %d; it is %s at fence %d",
		domain.ErrFenceLost, lease.Holder.ID, lease.RequestID, lease.Fence, common.AsString(row[0]), common.AsInt64(row[1]))
}

func readPosition(ctx context.Context, tx keelport.QueryService, lease domain.OperatorLease) (operatorPosition, error) {
	result, err := tx.Query(ctx, qOperatorPosition, lease.TenantID, lease.RequestID)
	if err != nil {
		return operatorPosition{}, fmt.Errorf("read operated turn position: %w", err)
	}
	if len(result.Rows) == 0 || len(result.Rows[0]) < 8 {
		return operatorPosition{}, fmt.Errorf("%w: operated turn %q has no position", domain.ErrNotFound, lease.RequestID)
	}
	row := result.Rows[0]
	return operatorPosition{
		committed: int(common.AsInt64(row[0])), openStep: common.AsInt64(row[1]), journal: int(common.AsInt64(row[2])),
		latestStep: common.AsInt64(row[3]), published: common.AsInt64(row[4]), replies: common.AsInt64(row[5]),
		lastEntry: int(common.AsInt64(row[6])), openCalls: common.AsInt64(row[7]),
	}, nil
}

// IntendCall records the holder's tool call in flight under the turn's next entry number. The
// call is filed under the step in progress, else the turn's latest step; a turn that ran no step
// has none to file it under.
func (control *TableTurnControl) IntendCall(ctx context.Context, lease domain.OperatorLease, tool domain.ToolReference, window time.Duration) (domain.OperatorCallIntent, error) {
	if err := lease.Check(); err != nil {
		return domain.OperatorCallIntent{}, err
	}
	if strings.TrimSpace(tool.ToolID) == "" || strings.TrimSpace(tool.Version) == "" {
		return domain.OperatorCallIntent{}, fmt.Errorf("%w: an operator call names a tool version", domain.ErrValidation)
	}
	seconds, err := windowSeconds(window)
	if err != nil {
		return domain.OperatorCallIntent{}, err
	}
	settings, err := control.operating(ctx)
	if err != nil {
		return domain.OperatorCallIntent{}, err
	}
	ctx = context.WithoutCancel(ctx)
	tx, position, err := control.operatorTx(ctx, lease, settings)
	if err != nil {
		return domain.OperatorCallIntent{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = position.admits(lease, settings); err != nil {
		return domain.OperatorCallIntent{}, err
	}
	step := position.openStep
	if step == 0 {
		step = position.latestStep
	}
	if step == 0 {
		return domain.OperatorCallIntent{}, fmt.Errorf("%w: turn %q ran no step a call can be filed under", domain.ErrConflict, lease.RequestID)
	}
	entryNo := position.lastEntry + 1
	intent := domain.OperatorCallIntent{
		Key:            domain.CallOutcomeKey{TenantID: lease.TenantID, RequestID: lease.RequestID, ExecutionStepID: step, CallID: fmt.Sprintf("op:%d:%d", lease.Fence, entryNo)},
		IdempotencyKey: operatorIdempotencyKey(lease, entryNo), EntryNo: entryNo, Tool: tool, Window: window,
	}
	inserted, err := tx.Query(ctx, qOperatorIntend, append(keyArgs(intent.Key), intent.IdempotencyKey, tool.ToolID, tool.Version, lease.Fence, seconds)...)
	if err != nil {
		return domain.OperatorCallIntent{}, fmt.Errorf("record operator call intent: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return domain.OperatorCallIntent{}, fmt.Errorf("%w: operator call %q of turn %q is already recorded", domain.ErrConflict, intent.Key.CallID, lease.RequestID)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.OperatorCallIntent{}, fmt.Errorf("commit operator call intent: %w", err)
	}
	committed = true
	return intent, nil
}

// admits refuses an act while another operator call is in flight or once the turn holds the most entries it may.
func (position operatorPosition) admits(lease domain.OperatorLease, settings domain.OperatorSettings) error {
	if position.openCalls > 0 {
		return fmt.Errorf("%w: an operator call of turn %q is still in flight", domain.ErrConflict, lease.RequestID)
	}
	if position.lastEntry >= settings.MaxEntries {
		return fmt.Errorf("%w: turn %q holds %d operator entries, the most it may", domain.ErrExecutionLimit, lease.RequestID, position.lastEntry)
	}
	return nil
}

// operatorIdempotencyKey is "op:" and the SHA-256 of the call's tenant, request, fence, and entry.
func operatorIdempotencyKey(lease domain.OperatorLease, entryNo int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x1f%s\x1f%d\x1f%d", lease.TenantID, lease.RequestID, lease.Fence, entryNo))
	return "op:" + hex.EncodeToString(sum[:])
}

// Act appends one entry under the holder's lease.
func (control *TableTurnControl) Act(ctx context.Context, act domain.OperatorAct) (domain.OperatorActed, error) {
	if err := act.Lease.Check(); err != nil {
		return domain.OperatorActed{}, err
	}
	if err := validAct(act); err != nil {
		return domain.OperatorActed{}, err
	}
	settings, err := control.operating(ctx)
	if err != nil {
		return domain.OperatorActed{}, err
	}
	ctx = context.WithoutCancel(ctx)
	position, err := control.position(ctx, act.Lease)
	if err != nil {
		return domain.OperatorActed{}, err
	}
	if act.Kind == domain.OperatorWithdraw {
		if err = control.withdrawable(ctx, act.Lease, position, act.Body.CallID); err != nil {
			return domain.OperatorActed{}, err
		}
	}
	entry := control.entryOf(act.Lease, act.Kind, act.Body, position)
	if act.Call != nil {
		entry.EntryNo = act.Call.EntryNo
	}
	if entry.Body.Frame != nil {
		entry.Body.Frame.Sequence, entry.Body.Frame.Fence, entry.Body.Frame.Final = position.replySequence(), act.Lease.Fence, false
	}
	entry.Usage = act.Usage
	if err = control.storeEntry(ctx, &entry, settings); err != nil {
		return domain.OperatorActed{}, err
	}
	var record domain.ObjectRef
	if act.Settlement != nil {
		if record, err = dehydrateCallRecord(ctx, control.Objects, act.Settlement.Key, act.Settlement.Record); err != nil {
			return domain.OperatorActed{}, err
		}
	}
	tx, current, err := control.operatorTx(ctx, act.Lease, settings)
	if err != nil {
		return domain.OperatorActed{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	acted := domain.OperatorActed{}
	if err = current.sameAs(position, entry); err != nil {
		return domain.OperatorActed{}, err
	}
	if act.Call != nil {
		if control.Settlement.Budget == nil {
			return domain.OperatorActed{}, fmt.Errorf("%w: operator tool calls need a budget settler", domain.ErrNotReady)
		}
		if err = control.Settlement.Budget.SettleTurnTx(ctx, tx, act.Reservation, act.Usage, false); err != nil {
			return domain.OperatorActed{}, fmt.Errorf("settle operator tool call spend: %w", err)
		}
		if err = settleOperatorCall(ctx, tx, act, record); err != nil {
			return domain.OperatorActed{}, err
		}
		current.openCalls = 0
	} else {
		if err = current.admits(act.Lease, settings); err != nil {
			return domain.OperatorActed{}, err
		}
	}
	if err = insertEntry(ctx, tx, &entry); err != nil {
		return domain.OperatorActed{}, err
	}
	if act.Call != nil && act.Settlement == nil {
		held, err := tx.Query(ctx, qOperatorHoldUnknown, act.Lease.TenantID, act.Lease.RequestID, act.Lease.Fence)
		if err != nil {
			return domain.OperatorActed{}, fmt.Errorf("hold turn on an unknown operator call: %w", err)
		}
		if len(held.Rows) == 0 {
			return domain.OperatorActed{}, fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, act.Lease.RequestID, act.Lease.Fence)
		}
		acted.HeldFence = common.AsInt64(held.Rows[0][0])
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.OperatorActed{}, fmt.Errorf("commit operator entry: %w", err)
	}
	committed = true
	acted.Entry = entry
	return acted, nil
}

// validAct refuses an entry its kind does not describe.
func validAct(act domain.OperatorAct) error {
	body := act.Body
	switch {
	case act.Kind == domain.OperatorNote && strings.TrimSpace(body.Text) != "" && body.Tool == nil && body.Frame == nil && body.CallID == "":
	case act.Kind == domain.OperatorReply && body.Frame != nil && len(body.Frame.Payload) > 0 && body.Tool == nil && body.CallID == "":
	case act.Kind == domain.OperatorWithdraw && strings.TrimSpace(body.CallID) != "" && body.Tool == nil && body.Frame == nil:
	case act.Kind == domain.OperatorTool && body.Tool != nil && act.Call != nil && body.Frame == nil && body.CallID == "":
		if body.Tool.CallID != act.Call.Key.CallID || body.Tool.ToolID != act.Call.Tool.ToolID || body.Tool.Version != act.Call.Tool.Version {
			return fmt.Errorf("%w: an operator tool entry must describe its intent", domain.ErrValidation)
		}
		if act.Settlement != nil && (act.Settlement.Key != act.Call.Key || act.Settlement.Validate() != nil || act.Settlement.Resolution != domain.ResolvedByGateway) {
			return fmt.Errorf("%w: an operator call settles its own intent by the gateway's answer", domain.ErrValidation)
		}
		if act.Settlement == nil && body.Tool.Status != domain.CallUnknown || act.Settlement != nil && body.Tool.Status != act.Settlement.Status {
			return fmt.Errorf("%w: an operator tool entry must carry its call outcome", domain.ErrValidation)
		}
		if len(act.Usage.Currency) != 3 || act.Usage.InputTokens < 0 || act.Usage.OutputTokens < 0 || act.Usage.ToolCalls < 1 ||
			act.Usage.SearchQueries < 0 || act.Usage.CostMinorUnits < 0 || len(act.Usage.Models) != 0 {
			return fmt.Errorf("%w: an operator call's entry carries its non-negative usage in the tenant currency", domain.ErrValidation)
		}
		if act.Reservation.TenantID != act.Lease.TenantID || act.Reservation.RequestID != act.Lease.RequestID ||
			strings.TrimSpace(act.Reservation.ReservationID) == "" || act.Reservation.Currency != act.Usage.Currency {
			return fmt.Errorf("%w: an operator call settles its action reservation", domain.ErrValidation)
		}
		return nil
	default:
		return fmt.Errorf("%w: an operator entry of kind %q does not carry what its kind needs", domain.ErrValidation, act.Kind)
	}
	if act.Call != nil || act.Settlement != nil || act.Reservation.ReservationID != "" || act.Usage.InputTokens != 0 || act.Usage.OutputTokens != 0 || act.Usage.ToolCalls != 0 ||
		act.Usage.SearchQueries != 0 || act.Usage.CostMinorUnits != 0 || len(act.Usage.Models) != 0 || act.Usage.Currency != "" {
		return fmt.Errorf("%w: only an operator tool call carries a call and usage", domain.ErrValidation)
	}
	return nil
}

// withdrawable refuses a withdrawal of anything but a call the step in progress proposed and
// never sent: proposed in its journal, unobserved there, without an outcome record, and not
// withdrawn already. An operated turn is frozen, so the check holds until the act commits.
func (control *TableTurnControl) withdrawable(ctx context.Context, lease domain.OperatorLease, position operatorPosition, callID string) error {
	if position.openStep == 0 {
		return fmt.Errorf("%w: turn %q has no step in progress, so no proposed call to withdraw", domain.ErrConflict, lease.RequestID)
	}
	key := domain.LoopKey{TenantID: lease.TenantID, RequestID: lease.RequestID, ExecutionStepID: position.openStep}
	entries, err := (&TableLoopJournal{DB: control.DB, Objects: control.Objects}).Load(ctx, key)
	if err != nil {
		return fmt.Errorf("load the step's journal: %w", err)
	}
	proposed := false
	for _, entry := range entries {
		switch {
		case entry.Kind == domain.LoopEntryModel && entry.Model != nil:
			for _, call := range entry.Model.ToolCalls {
				proposed = proposed || call.CallID == callID
			}
		case entry.Kind == domain.LoopEntryObservation && entry.Observation != nil && entry.Observation.CallID == callID:
			return fmt.Errorf("%w: call %q of turn %q was already observed", domain.ErrConflict, callID, lease.RequestID)
		}
	}
	if !proposed {
		return fmt.Errorf("%w: the step in progress of turn %q proposed no call %q", domain.ErrNotFound, lease.RequestID, callID)
	}
	recorded, err := control.qs.Query(ctx, qOperatorCallKnown, lease.TenantID, lease.RequestID, position.openStep, callID)
	if err != nil {
		return fmt.Errorf("read call outcome: %w", err)
	}
	if len(recorded.Rows) > 0 {
		return fmt.Errorf("%w: call %q of turn %q was sent; reconcile it instead", domain.ErrConflict, callID, lease.RequestID)
	}
	withdrawn, err := control.OperatorEntries(ctx, lease.TenantID, lease.RequestID)
	if err != nil {
		return err
	}
	for _, entry := range withdrawn {
		if entry.Kind == domain.OperatorWithdraw && entry.Position.ExecutionStepID == position.openStep && entry.Body.CallID == callID {
			return fmt.Errorf("%w: call %q of turn %q is withdrawn already", domain.ErrConflict, callID, lease.RequestID)
		}
	}
	return nil
}

// position reads where the operated turn stands before an entry's object is stored; the act's own
// transaction checks the turn still stands there.
func (control *TableTurnControl) position(ctx context.Context, lease domain.OperatorLease) (operatorPosition, error) {
	position, err := readPosition(ctx, control.qs, lease)
	if err != nil {
		return operatorPosition{}, err
	}
	return position, nil
}

// sameAs refuses an act whose entry was built against a turn that moved on meanwhile.
func (position operatorPosition) sameAs(built operatorPosition, entry domain.OperatorEntry) error {
	if position.lastEntry+1 != entry.EntryNo || position.replies != built.replies || position.published != built.published ||
		position.committed != built.committed || position.openStep != built.openStep || position.journal != built.journal {
		return fmt.Errorf("%w: another act of turn %q came first; retry", domain.ErrConflict, entry.RequestID)
	}
	return nil
}

func (control *TableTurnControl) entryOf(lease domain.OperatorLease, kind domain.OperatorEntryKind, body domain.OperatorEntryBody, position operatorPosition) domain.OperatorEntry {
	if body.Frame != nil {
		frame := *body.Frame
		body.Frame = &frame
	}
	return domain.OperatorEntry{
		TenantID: lease.TenantID, RequestID: lease.RequestID, Fence: lease.Fence, EntryNo: position.lastEntry + 1, Kind: kind,
		Actor: lease.Holder, Position: position.entryPosition(), Body: body,
	}
}

// storeEntry bounds and stores the entry's object, content-addressed under the turn and fence.
func (control *TableTurnControl) storeEntry(ctx context.Context, entry *domain.OperatorEntry, settings domain.OperatorSettings) error {
	payload, err := json.Marshal(entry.Body)
	if err != nil {
		return fmt.Errorf("encode operator entry: %w", err)
	}
	if len(payload) > settings.EntryMaxBytes {
		return fmt.Errorf("%w: an operator entry is %d bytes, more than %d", domain.ErrValidation, len(payload), settings.EntryMaxBytes)
	}
	name := fmt.Sprintf("operator-entry/%d/%s/%d", entry.TenantID, url.PathEscape(entry.RequestID), entry.Fence)
	if entry.Object, err = control.Objects.Dehydrate(ctx, name, payload); err != nil {
		return fmt.Errorf("dehydrate operator entry: %w", err)
	}
	return nil
}

func insertEntry(ctx context.Context, tx keelport.QueryService, entry *domain.OperatorEntry) error {
	var step, currency any
	if entry.Position.ExecutionStepID > 0 {
		step = entry.Position.ExecutionStepID
	}
	if entry.Kind == domain.OperatorTool {
		currency = entry.Usage.Currency
	}
	usage := entry.Usage
	inserted, err := tx.Query(ctx, qOperatorEntryInsert,
		entry.TenantID, entry.RequestID, entry.Fence, entry.EntryNo, string(entry.Kind), string(entry.Actor.Kind), entry.Actor.ID,
		entry.Position.StepNo, step, entry.Position.AfterEntryNo, entry.Object.URI, entry.Object.Digest,
		usage.InputTokens, usage.OutputTokens, usage.ToolCalls, usage.CostMinorUnits, usage.SearchQueries, currency)
	if err != nil {
		return fmt.Errorf("insert operator entry: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return fmt.Errorf("%w: operator entry %d of turn %q is already recorded", domain.ErrConflict, entry.EntryNo, entry.RequestID)
	}
	entry.CreatedAt, _ = common.AsTimeOK(inserted.Rows[0][0])
	if len(inserted.Rows[0]) > 1 {
		entry.Exponent = int(common.AsInt64(inserted.Rows[0][1]))
	}
	return nil
}

// settleOperatorCall settles the act's call by its gateway answer, or marks it unknown.
func settleOperatorCall(ctx context.Context, tx keelport.QueryService, act domain.OperatorAct, record domain.ObjectRef) error {
	key := act.Call.Key
	var settled []any
	query := qOperatorUnknownCall
	if act.Settlement != nil {
		query, settled = qOperatorSettleCall, []any{string(act.Settlement.Status), record.URI, record.Digest}
	}
	result, err := tx.Query(ctx, query, append(append(settled, keyArgs(key)...), act.Lease.Fence)...)
	if err != nil {
		return fmt.Errorf("settle operator call: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: operator call %q is not in flight under fence %d", domain.ErrConflict, key.CallID, act.Lease.Fence)
	}
	return nil
}

// HandBack appends the instruction, when there is one, and returns the operated turn to the queue.
func (control *TableTurnControl) HandBack(ctx context.Context, lease domain.OperatorLease, instruction string) (domain.OperatorHandedBack, error) {
	if err := lease.Check(); err != nil {
		return domain.OperatorHandedBack{}, err
	}
	settings, err := control.operating(ctx)
	if err != nil {
		return domain.OperatorHandedBack{}, err
	}
	ctx = context.WithoutCancel(ctx)
	position, err := control.position(ctx, lease)
	if err != nil {
		return domain.OperatorHandedBack{}, err
	}
	var entry *domain.OperatorEntry
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		built := control.entryOf(lease, domain.OperatorHandback, domain.OperatorEntryBody{Text: instruction}, position)
		if err = control.storeEntry(ctx, &built, settings); err != nil {
			return domain.OperatorHandedBack{}, err
		}
		entry = &built
	}
	tx, current, err := control.operatorTx(ctx, lease, settings)
	if err != nil {
		return domain.OperatorHandedBack{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if entry != nil {
		if err = current.sameAs(position, *entry); err != nil {
			return domain.OperatorHandedBack{}, err
		}
		if err = current.admits(lease, settings); err != nil {
			return domain.OperatorHandedBack{}, err
		}
		if err = insertEntry(ctx, tx, entry); err != nil {
			return domain.OperatorHandedBack{}, err
		}
	}
	handed, err := tx.Query(ctx, qOperatorHandback, lease.TenantID, lease.RequestID, lease.Fence, string(lease.Holder.Kind), lease.Holder.ID)
	if err != nil {
		return domain.OperatorHandedBack{}, fmt.Errorf("hand back turn: %w", err)
	}
	if len(handed.Rows) == 0 {
		return domain.OperatorHandedBack{}, fmt.Errorf("%w: a call of turn %q has an open outcome", domain.ErrConflict, lease.RequestID)
	}
	if _, err = tx.Query(ctx, qQueueRequeue, control.now().UTC(), lease.TenantID, lease.RequestID); err != nil {
		return domain.OperatorHandedBack{}, fmt.Errorf("requeue handed back turn: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.OperatorHandedBack{}, fmt.Errorf("commit turn handback: %w", err)
	}
	committed = true
	return domain.OperatorHandedBack{Fence: common.AsInt64(handed.Rows[0][0]), Instruction: entry}, nil
}

// Finish completes the operated turn with the holder's reply: the reply entry, the agent's and the
// operators' settlement, and the completion commit together under the holder's guarded touch.
func (control *TableTurnControl) Finish(ctx context.Context, finish domain.OperatorFinish) (domain.OperatorEntry, error) {
	lease := finish.Lease
	if err := lease.Check(); err != nil {
		return domain.OperatorEntry{}, err
	}
	if len(finish.Frame.Payload) == 0 || len(finish.Currency) != 3 {
		return domain.OperatorEntry{}, fmt.Errorf("%w: a finish needs its reply and the budget currency", domain.ErrValidation)
	}
	settings, err := control.operating(ctx)
	if err != nil {
		return domain.OperatorEntry{}, err
	}
	if err = control.Settlement.validate(); err != nil {
		return domain.OperatorEntry{}, err
	}
	ctx = context.WithoutCancel(ctx)
	position, err := control.position(ctx, lease)
	if err != nil {
		return domain.OperatorEntry{}, err
	}
	frame := finish.Frame
	entry := control.entryOf(lease, domain.OperatorReply, domain.OperatorEntryBody{Frame: &frame}, position)
	entry.Body.Frame.Sequence, entry.Body.Frame.Fence, entry.Body.Frame.Final = position.replySequence(), lease.Fence, false
	if err = control.storeEntry(ctx, &entry, settings); err != nil {
		return domain.OperatorEntry{}, err
	}
	turn, err := control.qs.Query(ctx, qOperatorState, lease.TenantID, lease.RequestID)
	if err != nil {
		return domain.OperatorEntry{}, fmt.Errorf("read operated turn: %w", err)
	}
	if len(turn.Rows) == 0 {
		return domain.OperatorEntry{}, fmt.Errorf("%w: turn request %q", domain.ErrNotFound, lease.RequestID)
	}
	turnNo, conversationID := common.AsInt64(turn.Rows[0][5]), common.AsString(turn.Rows[0][6])
	response, err := control.Objects.Dehydrate(ctx, turnResponseName(lease.TenantID, conversationID, turnNo), frame.Payload)
	if err != nil {
		return domain.OperatorEntry{}, fmt.Errorf("dehydrate operator response: %w", err)
	}
	usage, err := control.agentUsage(ctx, lease, finish.Currency)
	if err != nil {
		return domain.OperatorEntry{}, err
	}
	tx, current, err := control.operatorTx(ctx, lease, settings)
	if err != nil {
		return domain.OperatorEntry{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = current.sameAs(position, entry); err != nil {
		return domain.OperatorEntry{}, err
	}
	if err = current.admits(lease, settings); err != nil {
		return domain.OperatorEntry{}, err
	}
	if err = insertEntry(ctx, tx, &entry); err != nil {
		return domain.OperatorEntry{}, err
	}
	settlement := domain.TurnSettlement{Reservation: finish.Reservation, Usage: usage, SubjectRef: finish.SubjectRef, Attribution: finish.Attribution}
	unheld, err := control.Settlement.write(ctx, tx, lease.TenantID, lease.RequestID, current.conversationID, current.turnNo, settlement)
	if err != nil {
		return domain.OperatorEntry{}, err
	}
	if unheld != nil {
		return domain.OperatorEntry{}, unheld
	}
	kind, card := resultCard(frame.Payload, response, positiveOr(control.MaxResultCardBytes, defaultResultCardBytes))
	finished, err := tx.Query(ctx, qOperatorFinish, response.URI, response.Digest,
		lease.TenantID, lease.RequestID, lease.Fence, string(lease.Holder.Kind), lease.Holder.ID, kind, card)
	if err != nil {
		return domain.OperatorEntry{}, fmt.Errorf("finish operated turn: %w", err)
	}
	if len(finished.Rows) == 0 {
		return domain.OperatorEntry{}, fmt.Errorf("%w: a call of turn %q has an open outcome, or an earlier turn of its conversation is live", domain.ErrConflict, lease.RequestID)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.OperatorEntry{}, fmt.Errorf("commit operator finish: %w", err)
	}
	committed = true
	return entry, nil
}

// agentUsage is what the turn's agent spent before the hold, accounted as a replay would: each
// committed step's result, and the journal of the step in progress. An operated turn is frozen, so
// reading it before the finish's transaction reads what that transaction settles.
func (control *TableTurnControl) agentUsage(ctx context.Context, lease domain.OperatorLease, currency string) (domain.Usage, error) {
	steps, err := control.qs.Query(ctx, qOperatorSteps, lease.TenantID, lease.RequestID)
	if err != nil {
		return domain.Usage{}, fmt.Errorf("read the turn's steps: %w", err)
	}
	journal := &TableLoopJournal{DB: control.DB, Objects: control.Objects}
	usage := domain.Usage{}
	for _, row := range steps.Rows {
		stepID, status := common.AsInt64(row[0]), domain.StepIdempotencyStatus(common.AsString(row[1]))
		if status == domain.StepCommitted {
			payload, err := control.Objects.Hydrate(ctx, domain.ObjectRef{URI: common.AsString(row[2]), Digest: common.AsString(row[3])})
			if err != nil {
				return domain.Usage{}, fmt.Errorf("hydrate step %d result: %w", stepID, err)
			}
			var result domain.StepResult
			if err = json.Unmarshal(payload, &result); err != nil {
				return domain.Usage{}, fmt.Errorf("decode step %d result: %w", stepID, err)
			}
			if err = addChunkUsage(&usage, result.Usage); err != nil {
				return domain.Usage{}, err
			}
			continue
		}
		entries, err := journal.Load(ctx, domain.LoopKey{TenantID: lease.TenantID, RequestID: lease.RequestID, ExecutionStepID: stepID})
		if err != nil {
			return domain.Usage{}, fmt.Errorf("load step %d journal: %w", stepID, err)
		}
		for _, entry := range entries {
			if err = addChunkUsage(&usage, entry.Usage); err != nil {
				return domain.Usage{}, err
			}
		}
	}
	if usage.Currency == "" {
		usage.Currency = currency
	}
	return usage, nil
}

// OperatorEntries reads a turn's operator entries in order, each object hydrated and verified.
func (control *TableTurnControl) OperatorEntries(ctx context.Context, tenantID int64, requestID string) ([]domain.OperatorEntry, error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	qs, err := control.queries(ctx)
	if err != nil {
		return nil, err
	}
	if control.Objects == nil {
		return nil, fmt.Errorf("%w: operator entries need an object codec", domain.ErrNotReady)
	}
	listed, err := qs.Query(ctx, qOperatorEntries, tenantID, requestID, MaxOperatorEntries+1)
	if err != nil {
		return nil, fmt.Errorf("list operator entries: %w", err)
	}
	if len(listed.Rows) > MaxOperatorEntries {
		return nil, fmt.Errorf("%w: turn %q has more than %d operator entries", domain.ErrExecutionLimit, requestID, MaxOperatorEntries)
	}
	entries := make([]domain.OperatorEntry, 0, len(listed.Rows))
	for _, row := range listed.Rows {
		entry, err := decodeOperatorEntry(tenantID, requestID, row)
		if err != nil {
			return nil, err
		}
		payload, err := control.Objects.Hydrate(ctx, entry.Object)
		if err != nil {
			return nil, fmt.Errorf("hydrate operator entry %d: %w", entry.EntryNo, err)
		}
		if err = json.Unmarshal(payload, &entry.Body); err != nil {
			return nil, fmt.Errorf("decode operator entry %d: %w", entry.EntryNo, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func decodeOperatorEntry(tenantID int64, requestID string, row []any) (domain.OperatorEntry, error) {
	if len(row) < 18 {
		return domain.OperatorEntry{}, fmt.Errorf("decode operator entry: expected 18 columns, got %d", len(row))
	}
	entry := domain.OperatorEntry{
		TenantID: tenantID, RequestID: requestID, Fence: common.AsInt64(row[0]), EntryNo: int(common.AsInt64(row[1])),
		Kind:  domain.OperatorEntryKind(common.AsString(row[2])),
		Actor: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[3])), ID: common.AsString(row[4])},
		Position: domain.OperatorPosition{
			StepNo: int(common.AsInt64(row[5])), ExecutionStepID: common.AsInt64(row[6]), AfterEntryNo: int(common.AsInt64(row[7])),
		},
		Object: domain.ObjectRef{URI: common.AsString(row[8]), Digest: common.AsString(row[9])},
		Usage: domain.Usage{
			InputTokens: common.AsInt64(row[10]), OutputTokens: common.AsInt64(row[11]), ToolCalls: int(common.AsInt64(row[12])),
			CostMinorUnits: common.AsInt64(row[13]), SearchQueries: common.AsInt64(row[14]), Currency: common.AsString(row[15]),
		},
		Exponent: int(common.AsInt64(row[16])),
	}
	entry.CreatedAt, _ = common.AsTimeOK(row[17])
	if !entry.Kind.Valid() || entry.Position.StepNo <= 0 {
		return domain.OperatorEntry{}, fmt.Errorf("%w: operator entry %d does not decode", domain.ErrConflict, entry.EntryNo)
	}
	return entry, nil
}

// LapseHolds holds again, by the store clock, at most batch operated turns whose lease lapsed.
func (control *TableTurnControl) LapseHolds(ctx context.Context, batch int) ([]domain.LapsedHold, error) {
	if batch <= 0 || batch > MaxHoldLapseBatch {
		return nil, fmt.Errorf("%w: a lapse batch lies within 1..%d", domain.ErrValidation, MaxHoldLapseBatch)
	}
	qs, err := control.queries(ctx)
	if err != nil {
		return nil, err
	}
	lapsed, err := qs.Query(context.WithoutCancel(ctx), qOperatorLapse, batch)
	if err != nil {
		return nil, fmt.Errorf("lapse operator holds: %w", err)
	}
	holds := make([]domain.LapsedHold, 0, len(lapsed.Rows))
	for _, row := range lapsed.Rows {
		if len(row) < 6 {
			return holds, errors.New("decode lapsed hold: expected 6 columns")
		}
		holds = append(holds, domain.LapsedHold{
			TenantID: common.AsInt64(row[0]), RequestID: common.AsString(row[1]), Fence: common.AsInt64(row[2]),
			Holder:  domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[3])), ID: common.AsString(row[4])},
			AgentID: common.AsString(row[5]),
		})
	}
	return holds, nil
}
