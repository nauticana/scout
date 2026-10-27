package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qRunTraceRun       = "scout_run_trace_run"
	qRunTraceTurn      = "scout_run_trace_turn"
	qRunTraceJournal   = "scout_run_trace_journal"
	qRunTraceSafety    = "scout_run_trace_safety"
	qRunTraceApprovals = "scout_run_trace_approvals"
	qRunTraceDecisions = "scout_run_trace_decisions"
	qRunTraceOutcomes  = "scout_run_trace_outcomes"

	// DefaultRunTrace* apply to zero limits; MaxRunTrace* are hard ceilings.
	DefaultRunTraceEvents         = 500
	DefaultRunTraceJournalEntries = 1000
	DefaultRunTraceJournalBytes   = 4 << 20
	MaxRunTraceEvents             = 5000
	MaxRunTraceJournalEntries     = 10000
	MaxRunTraceJournalBytes       = 64 << 20
)

// Each source query is tenant-scoped and reads one row past its bound to detect truncation.
var runTraceQueries = map[string]string{
	qRunTraceRun: `
SELECT id, agent_id, agent_version, task_kind, request_id, status_code, completed_at
  FROM agent_run
 WHERE partner_id = ? AND id = ? AND agent_id = ?`,
	qRunTraceTurn: `
SELECT conversation_id, turn_no, status_code, queued_at, started_at, first_response_at, completed_at
  FROM conversation_turn
 WHERE partner_id = ? AND request_id = ?`,
	qRunTraceJournal: `
SELECT execution_step_id, entry_no, entry_uri, entry_digest, created_at
  FROM step_loop_entry
 WHERE partner_id = ? AND request_id = ?
 ORDER BY created_at, execution_step_id, entry_no
 LIMIT ?`,
	qRunTraceSafety: `
SELECT id, stage_code, layer_code, action_code, severity_code, rule_ids, release_version, policy_version, duration_ms, occurred_at
  FROM safety_event
 WHERE partner_id = ? AND request_id = ?
 ORDER BY occurred_at, id
 LIMIT ?`,
	qRunTraceApprovals: `
SELECT r.id, r.execution_step_id, r.requested_action, r.resource_ref, r.rule_source_code, r.rule_id, r.risk_tier_code,
       r.status_code, COALESCE(d.proposed_digest, r.proposed_digest), r.revises_step_id,
       r.created_at, r.resolved_at, d.decider_kind, d.decided_at
  FROM turn_approval_request r
  LEFT JOIN turn_approval_decision d ON d.turn_approval_request_id = r.id AND d.partner_id = r.partner_id
 WHERE r.partner_id = ? AND r.request_id = ?
 ORDER BY r.created_at, r.id
 LIMIT ?`,
	qRunTraceDecisions: `
SELECT id, category, performed_action, resource_ref, outcome_code, policy_id, policy_version, release_version,
       principal_kind, principal_id, grant_id, grantor_kind, grantor_id, obligations, payload_digest, occurred_at
  FROM audit_event
 WHERE partner_id = ? AND request_id = ?
 ORDER BY occurred_at, id
 LIMIT ?`,
	qRunTraceOutcomes: `
SELECT execution_step_id, call_id, status_code, COALESCE(resolution_code, ''), COALESCE(resolver_kind, ''),
       COALESCE(resolver_id, ''), resolved_at, COALESCE(evidence_digest, '')
  FROM turn_call_outcome
 WHERE partner_id = ? AND request_id = ?
 ORDER BY execution_step_id, call_id
 LIMIT ?`,
}

// TableRunTrace assembles a payload-free trace from durable turn records.
type TableRunTrace struct {
	DB      port.DatabaseRepository
	Objects contract.EvidenceArchive
	// Charter is optional; with it, each governed action Charter recorded joins the trace as a decision.
	Charter contract.CharterActionSource
	Limits  domain.RunTraceLimits

	once sync.Once
	qs   port.QueryService
}

var _ contract.RunTraceQuery = (*TableRunTrace)(nil)

func (store *TableRunTrace) init(ctx context.Context) error {
	if store.DB == nil || store.Objects == nil {
		return fmt.Errorf("run trace: database and object codec are required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, runTraceQueries) })
	if store.qs == nil {
		return fmt.Errorf("run trace: query service is required")
	}
	return nil
}

func (store *TableRunTrace) limits() (domain.RunTraceLimits, error) {
	limits := store.Limits
	if limits.MaxEvents < 0 || limits.MaxEvents > MaxRunTraceEvents ||
		limits.MaxJournalEntries < 0 || limits.MaxJournalEntries > MaxRunTraceJournalEntries ||
		limits.MaxJournalBytes < 0 || limits.MaxJournalBytes > MaxRunTraceJournalBytes {
		return limits, fmt.Errorf("run trace: limits exceed their supported range")
	}
	limits.MaxEvents = positiveOr(limits.MaxEvents, DefaultRunTraceEvents)
	limits.MaxJournalEntries = positiveOr(limits.MaxJournalEntries, DefaultRunTraceJournalEntries)
	limits.MaxJournalBytes = positiveOr(limits.MaxJournalBytes, DefaultRunTraceJournalBytes)
	return limits, nil
}

// Trace returns one scoped run and its ordered events; a run outside a turn has none.
func (store *TableRunTrace) Trace(ctx context.Context, tenantID int64, agentID string, runID int64) (domain.RunTrace, error) {
	limits, err := store.limits()
	if err != nil {
		return domain.RunTrace{}, err
	}
	agentID = strings.TrimSpace(agentID)
	if tenantID <= 0 || agentID == "" || runID <= 0 {
		return domain.RunTrace{}, fmt.Errorf("%w: tenant, agent, and run are required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return domain.RunTrace{}, err
	}
	run, err := store.run(ctx, tenantID, agentID, runID)
	if err != nil {
		return domain.RunTrace{}, err
	}
	trace := domain.RunTrace{Run: run, Events: []domain.RunTraceEvent{}}
	if run.RequestID == "" {
		return trace, nil
	}
	if trace.Turn, err = store.turn(ctx, tenantID, run.RequestID); err != nil {
		return domain.RunTrace{}, err
	}
	collector := &traceCollector{maxEvents: limits.MaxEvents}
	for _, read := range []func(context.Context, int64, string, domain.RunTraceLimits, *traceCollector) error{
		store.journal, store.guardrails, store.approvals, store.decisions, store.charterActions,
	} {
		if err := read(ctx, tenantID, run.RequestID, limits, collector); err != nil {
			return domain.RunTrace{}, err
		}
	}
	if err := store.settlements(ctx, tenantID, run.RequestID, collector); err != nil {
		return domain.RunTrace{}, err
	}
	trace.Events, trace.Truncated = collector.ordered()
	return trace, nil
}

func (store *TableRunTrace) run(ctx context.Context, tenantID int64, agentID string, runID int64) (domain.AgentRun, error) {
	result, err := store.qs.Query(ctx, qRunTraceRun, tenantID, runID, agentID)
	if err != nil {
		return domain.AgentRun{}, fmt.Errorf("read agent run %d: %w", runID, err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentRun{}, fmt.Errorf("%w: run %d", domain.ErrNotFound, runID)
	}
	row := result.Rows[0]
	if len(row) < 7 {
		return domain.AgentRun{}, malformedTraceRow("agent run")
	}
	completedAt, ok := common.AsTimeOK(row[6])
	if !ok {
		return domain.AgentRun{}, malformedTraceRow("agent run")
	}
	return domain.AgentRun{
		ID:          common.AsInt64(row[0]),
		Release:     domain.AgentReleaseReference{AgentID: common.AsString(row[1]), Version: common.AsString(row[2])},
		TaskKind:    common.AsString(row[3]),
		RequestID:   common.AsString(row[4]),
		Status:      domain.RunStatus(common.AsString(row[5])),
		CompletedAt: completedAt,
	}, nil
}

func (store *TableRunTrace) turn(ctx context.Context, tenantID int64, requestID string) (*domain.RunTraceTurn, error) {
	result, err := store.qs.Query(ctx, qRunTraceTurn, tenantID, requestID)
	if err != nil {
		return nil, fmt.Errorf("read the run's turn: %w", err)
	}
	if len(result.Rows) == 0 {
		return nil, nil
	}
	row := result.Rows[0]
	if len(row) < 7 {
		return nil, malformedTraceRow("conversation turn")
	}
	queuedAt, ok := common.AsTimeOK(row[3])
	if !ok {
		return nil, malformedTraceRow("conversation turn")
	}
	return &domain.RunTraceTurn{
		ConversationID: common.AsString(row[0]), TurnNo: common.AsInt64(row[1]), Status: domain.TurnStatus(common.AsString(row[2])),
		QueuedAt: queuedAt, StartedAt: common.AsTime(row[4]), FirstResponseAt: common.AsTime(row[5]), CompletedAt: common.AsTime(row[6]),
	}, nil
}

// journal cuts the trace before the first entry past its entry or cumulative byte bound.
func (store *TableRunTrace) journal(ctx context.Context, tenantID int64, requestID string, limits domain.RunTraceLimits, collector *traceCollector) error {
	result, err := store.qs.Query(ctx, qRunTraceJournal, tenantID, requestID, limits.MaxJournalEntries+1)
	if err != nil {
		return fmt.Errorf("list the run's loop journal: %w", err)
	}
	var bytesRead int64
	events := 0
	for index, row := range result.Rows {
		if len(row) < 5 {
			return malformedTraceRow("loop journal entry")
		}
		createdAt, ok := common.AsTimeOK(row[4])
		if !ok {
			return malformedTraceRow("loop journal entry")
		}
		key := traceKey{at: createdAt, source: traceSourceJournal, seq: int64(index)}
		if index >= limits.MaxJournalEntries || events > limits.MaxEvents {
			collector.stopAt(key)
			return nil
		}
		stepID, entryNo := common.AsInt64(row[0]), int(common.AsInt64(row[1]))
		object := domain.ObjectRef{URI: common.AsString(row[2]), Digest: strings.TrimSpace(common.AsString(row[3]))}
		payload, err := store.Objects.Hydrate(ctx, object)
		if err != nil {
			return traceSourceFailure(fmt.Sprintf("loop journal entry %d of step %d", entryNo, stepID), err)
		}
		if DigestBytes(payload) != object.Digest {
			return fmt.Errorf("%w: loop journal entry %d of step %d", ErrDigestMismatch, entryNo, stepID)
		}
		if bytesRead += int64(len(payload)); bytesRead > limits.MaxJournalBytes {
			collector.stopAt(key)
			return nil
		}
		var entry domain.LoopEntry
		if err := json.Unmarshal(payload, &entry); err != nil || entry.EntryNo != entryNo {
			return malformedTraceRow(fmt.Sprintf("loop journal entry %d of step %d", entryNo, stepID))
		}
		event, err := journalEvent(entry, domain.RunTraceJournalRef{ExecutionStepID: stepID, EntryNo: entryNo, Digest: object.Digest})
		if err != nil {
			return malformedTraceRow(fmt.Sprintf("loop journal entry %d of step %d", entryNo, stepID))
		}
		if event.Kind != "" {
			collector.add(key, event)
			events++
		}
	}
	return nil
}

func (store *TableRunTrace) guardrails(ctx context.Context, tenantID int64, requestID string, limits domain.RunTraceLimits, collector *traceCollector) error {
	result, err := store.qs.Query(ctx, qRunTraceSafety, tenantID, requestID, limits.MaxEvents+1)
	if err != nil {
		return fmt.Errorf("read the run's safety events: %w", err)
	}
	for _, row := range result.Rows {
		if len(row) < 10 {
			return malformedTraceRow("safety event")
		}
		occurredAt, ok := common.AsTimeOK(row[9])
		var ruleIDs []string
		durationMS := common.AsInt64(row[8])
		if !ok || durationMS < 0 || durationMS > math.MaxInt64/int64(time.Millisecond) ||
			json.Unmarshal([]byte(common.AsString(row[5])), &ruleIDs) != nil {
			return malformedTraceRow("safety event")
		}
		collector.add(traceKey{at: occurredAt, source: traceSourceGuardrail, seq: common.AsInt64(row[0])}, domain.RunTraceEvent{
			Kind: domain.RunTraceEventGuardrail, Guardrail: &domain.RunTraceGuardrailHit{
				Stage: domain.GuardrailStage(common.AsString(row[1])), Layer: domain.GuardrailLayer(common.AsString(row[2])),
				Action: domain.GuardrailAction(common.AsString(row[3])), Severity: domain.GuardrailSeverity(common.AsString(row[4])),
				RuleIDs: ruleIDs, ReleaseVersion: common.AsString(row[6]), PolicyVersion: common.AsString(row[7]),
				Duration: time.Duration(durationMS) * time.Millisecond,
			},
		})
	}
	return nil
}

func (store *TableRunTrace) approvals(ctx context.Context, tenantID int64, requestID string, limits domain.RunTraceLimits, collector *traceCollector) error {
	result, err := store.qs.Query(ctx, qRunTraceApprovals, tenantID, requestID, limits.MaxEvents+1)
	if err != nil {
		return fmt.Errorf("read the run's approval requests: %w", err)
	}
	for _, row := range result.Rows {
		if len(row) < 14 {
			return malformedTraceRow("approval request")
		}
		createdAt, ok := common.AsTimeOK(row[10])
		if !ok {
			return malformedTraceRow("approval request")
		}
		collector.add(traceKey{at: createdAt, source: traceSourceApproval, seq: common.AsInt64(row[0])}, domain.RunTraceEvent{
			Kind: domain.RunTraceEventApproval, Approval: &domain.RunTraceApproval{
				ID: common.AsInt64(row[0]), ExecutionStepID: common.AsInt64(row[1]), RequestedAction: common.AsString(row[2]),
				Resource: common.AsString(row[3]), RuleSource: common.AsString(row[4]), RuleID: common.AsString(row[5]),
				RiskTier: domain.RiskTier(common.AsString(row[6])), Status: domain.ApprovalStatus(common.AsString(row[7])),
				ProposedDigest: strings.TrimSpace(common.AsString(row[8])), RevisesStepID: common.AsInt64(row[9]),
				ResolvedAt: common.AsTime(row[11]), DeciderKind: domain.PrincipalKind(common.AsString(row[12])), DecidedAt: common.AsTime(row[13]),
			},
		})
	}
	return nil
}

func (store *TableRunTrace) decisions(ctx context.Context, tenantID int64, requestID string, limits domain.RunTraceLimits, collector *traceCollector) error {
	result, err := store.qs.Query(ctx, qRunTraceDecisions, tenantID, requestID, limits.MaxEvents+1)
	if err != nil {
		return fmt.Errorf("read the run's governed decisions: %w", err)
	}
	for _, row := range result.Rows {
		if len(row) < 16 {
			return malformedTraceRow("audit decision")
		}
		occurredAt, ok := common.AsTimeOK(row[15])
		if !ok {
			return malformedTraceRow("audit decision")
		}
		decision := &domain.RunTraceDecisionEvent{
			Source: domain.RunTraceDecisionAudit, Category: common.AsString(row[1]), Action: common.AsString(row[2]),
			Resource: common.AsString(row[3]), Outcome: domain.DecisionOutcome(common.AsString(row[4])),
			PolicyID: common.AsString(row[5]), PolicyVersion: common.AsString(row[6]), ReleaseVersion: common.AsString(row[7]),
			Authority: domain.AuthorityRef{
				Subject: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[8])), ID: common.AsString(row[9])},
				GrantID: common.AsString(row[10]),
				Grantor: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[11])), ID: common.AsString(row[12])},
			},
			EvidenceDigest: strings.TrimSpace(common.AsString(row[14])),
		}
		for obligation := range strings.SplitSeq(common.AsString(row[13]), ",") {
			if obligation != "" {
				decision.Obligations = append(decision.Obligations, domain.ObligationKind(obligation))
			}
		}
		collector.add(traceKey{at: occurredAt, source: traceSourceAudit, seq: common.AsInt64(row[0])},
			domain.RunTraceEvent{Kind: domain.RunTraceEventDecision, Decision: decision})
	}
	return nil
}

func (store *TableRunTrace) charterActions(ctx context.Context, tenantID int64, requestID string, limits domain.RunTraceLimits, collector *traceCollector) error {
	if store.Charter == nil {
		return nil
	}
	actions, err := store.Charter.RunActions(ctx, tenantID, requestID, limits.MaxEvents+1)
	if err != nil {
		return traceSourceFailure("charter actions", err)
	}
	// Charter returns a run's latest actions, so a full window may omit its first ones: no
	// consistent prefix of the trace can be built from it.
	if len(actions) > limits.MaxEvents {
		return fmt.Errorf("%w: run %q has more than %d Charter actions; a trace cannot show their beginning", domain.ErrExecutionLimit, requestID, limits.MaxEvents)
	}
	for index := range actions {
		action := &actions[index]
		collector.add(traceKey{at: action.At, source: traceSourceCharter, seq: int64(index)}, domain.RunTraceEvent{
			Kind: domain.RunTraceEventDecision, Decision: &domain.RunTraceDecisionEvent{
				Source: domain.RunTraceDecisionCharter, Action: action.Capability.ID, Charter: action,
			},
		})
	}
	return nil
}

type traceCallKey struct {
	step   int64
	callID string
}

// settlements attaches each journaled call's outcome record, matched by execution step and call id.
func (store *TableRunTrace) settlements(ctx context.Context, tenantID int64, requestID string, collector *traceCollector) error {
	calls := map[traceCallKey][]*domain.RunTraceToolCall{}
	for _, keyed := range collector.events {
		if call := keyed.event.Tool; call != nil && call.CallID != "" {
			key := traceCallKey{step: call.Journal.ExecutionStepID, callID: call.CallID}
			calls[key] = append(calls[key], call)
		}
	}
	if len(calls) == 0 {
		return nil
	}
	result, err := store.qs.Query(ctx, qRunTraceOutcomes, tenantID, requestID, maxTurnOutcomes+1)
	if err != nil {
		return fmt.Errorf("read the run's call outcomes: %w", err)
	}
	if len(result.Rows) > maxTurnOutcomes {
		return fmt.Errorf("%w: turn %q has more than %d call outcomes", domain.ErrExecutionLimit, requestID, maxTurnOutcomes)
	}
	for _, row := range result.Rows {
		if len(row) < 8 {
			return malformedTraceRow("call outcome")
		}
		settlement := &domain.RunTraceCallSettlement{
			Status: domain.CallOutcomeStatus(common.AsString(row[2])), Resolution: domain.CallResolution(common.AsString(row[3])),
			Resolver:   domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[4])), ID: common.AsString(row[5])},
			ResolvedAt: common.AsTime(row[6]), EvidenceDigest: strings.TrimSpace(common.AsString(row[7])),
		}
		if !settlement.Status.Valid() || settlement.Resolution != "" && !settlement.Resolution.Valid() {
			return malformedTraceRow("call outcome")
		}
		for _, call := range calls[traceCallKey{step: common.AsInt64(row[0]), callID: common.AsString(row[1])}] {
			call.Settlement = settlement
		}
	}
	return nil
}

func malformedTraceRow(what string) error {
	return fmt.Errorf("run trace: malformed %s", what)
}

type traceSourceUnavailable struct {
	what string
	err  error
}

func (err traceSourceUnavailable) Error() string { return "run trace " + err.what + " unavailable" }
func (err traceSourceUnavailable) Unwrap() error { return err.err }

// traceSourceFailure masks record errors while preserving operational retry and cancellation signals.
func traceSourceFailure(what string, err error) error {
	if errors.Is(err, ErrDigestMismatch) {
		return fmt.Errorf("run trace %s: %w", what, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("run trace %s: %w", what, err)
	}
	if errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrCircuitOpen) || errors.Is(err, domain.ErrNotReady) {
		return traceSourceUnavailable{what: what, err: err}
	}
	return fmt.Errorf("run trace %s: %v", what, err)
}
