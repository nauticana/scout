package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qAgentEvalLockSet    = "scout_agent_evaluation_lock_set"
	qAgentEvalOpenRuns   = "scout_agent_evaluation_open_runs"
	qAgentEvalVersion    = "scout_agent_evaluation_version"
	qAgentEvalRelease    = "scout_agent_evaluation_release"
	qAgentEvalInsertRun  = "scout_agent_evaluation_insert_run"
	qAgentEvalInsertSubj = "scout_agent_evaluation_insert_subject"
	qAgentEvalRun        = "scout_agent_evaluation_run"
	qAgentEvalResults    = "scout_agent_evaluation_results"
	qAgentEvalDraft      = "scout_agent_evaluation_draft"
	qAgentEvalRecorded   = "scout_agent_evaluation_recorded"
	qAgentEvalRenew      = "scout_agent_evaluation_renew"
	qAgentEvalPutResult  = "scout_agent_evaluation_put_result"
	qAgentEvalAddTotals  = "scout_agent_evaluation_add_totals"
	qAgentEvalFinish     = "scout_agent_evaluation_finish"
)

// The run and results reads take the reader's scope: a dev reader never sees a
// gate example's result, only how many were withheld.
var agentEvaluationQueries = map[string]string{
	qAgentEvalLockSet: `
SELECT golden_set_id FROM agent_golden_set WHERE partner_id = ? AND agent_id = ? FOR UPDATE`,
	qAgentEvalOpenRuns: `
SELECT COUNT(*) FROM evaluation_run WHERE partner_id = ? AND agent_id = ? AND status_code IN ('queued', 'running')`,
	qAgentEvalVersion: `
SELECT COUNT(e.example_id)
  FROM golden_set_version v
  LEFT JOIN golden_example e ON e.partner_id = v.partner_id AND e.golden_set_id = v.golden_set_id AND e.set_version = v.set_version
                            AND (e.scope_code = ? OR ? = 'gate')
 WHERE v.partner_id = ? AND v.golden_set_id = ? AND v.set_version = ?
 GROUP BY v.set_version`,
	qAgentEvalRelease: `
SELECT definition_digest FROM agent_version WHERE partner_id = ? AND agent_id = ? AND agent_version = ?`,
	qAgentEvalInsertRun: `
INSERT INTO evaluation_run (id, partner_id, agent_id, golden_set_id, set_version, scope_code, status_code, started_at,
                            requested_by_kind, requested_by_id)
VALUES (?, ?, ?, ?, ?, ?, 'queued', NULL, ?, ?)
RETURNING queued_at`,
	qAgentEvalInsertSubj: `
INSERT INTO evaluation_run_subject (run_id, partner_id, role_code, subject_code, agent_id, agent_version, definition_digest, definition_json)
VALUES (?, ?, 'candidate', ?, ?, ?, ?, ?)`,
	qAgentEvalRun: `
SELECT r.id, r.agent_id, r.golden_set_id, r.set_version, r.scope_code, r.status_code, r.queued_at, r.started_at, r.completed_at,
       r.requested_by_kind, r.requested_by_id, r.sample_count, r.input_tokens, r.output_tokens, r.cost_minor_units, r.currency_code,
       COALESCE(c.exponent, 0), r.reason, s.subject_code, s.agent_version, s.definition_digest, r.lease_token,
       (SELECT COUNT(*)
          FROM evaluation_result x
          JOIN golden_example e ON e.partner_id = x.partner_id AND e.golden_set_id = x.golden_set_id
                               AND e.set_version = x.set_version AND e.example_id = x.example_id
         WHERE x.partner_id = r.partner_id AND x.run_id = r.id AND e.scope_code = 'gate' AND ? <> 'gate')
  FROM evaluation_run r
  JOIN evaluation_run_subject s ON s.partner_id = r.partner_id AND s.run_id = r.id AND s.role_code = 'candidate'
  LEFT JOIN currency c ON c.code = r.currency_code
 WHERE r.partner_id = ? AND r.id = ? AND r.agent_id IS NOT NULL`,
	qAgentEvalResults: `
SELECT x.example_id, x.scores, x.latency_ms, x.input_tokens, x.output_tokens, x.cost_minor_units, x.currency_code,
       COALESCE(c.exponent, 0), x.needs_human_review, x.reason
  FROM evaluation_result x
  JOIN golden_example e ON e.partner_id = x.partner_id AND e.golden_set_id = x.golden_set_id
                       AND e.set_version = x.set_version AND e.example_id = x.example_id
  LEFT JOIN currency c ON c.code = x.currency_code
 WHERE x.partner_id = ? AND x.run_id = ? AND x.role_code = 'candidate'
   AND (e.scope_code = ? OR ? = 'gate')
   AND (? = '' OR x.example_id < ?)
 ORDER BY x.example_id DESC
 LIMIT ?`,
	qAgentEvalDraft: `
SELECT definition_json FROM evaluation_run_subject WHERE partner_id = ? AND run_id = ? AND role_code = 'candidate'`,
	qAgentEvalRecorded: `
SELECT example_id, input_tokens, output_tokens, cost_minor_units, currency_code
  FROM evaluation_result
 WHERE partner_id = ? AND run_id = ? AND role_code = 'candidate'`,
	qAgentEvalRenew: `
UPDATE evaluation_run
   SET lease_until = CURRENT_TIMESTAMP + (? * INTERVAL '1 second')
 WHERE partner_id = ? AND id = ? AND lease_token = ? AND status_code = 'running'
RETURNING golden_set_id, set_version`,
	qAgentEvalPutResult: `
INSERT INTO evaluation_result (run_id, partner_id, golden_set_id, set_version, example_id, role_code, scores, latency_ms,
                               input_tokens, output_tokens, cost_minor_units, currency_code, needs_human_review, reason)
VALUES (?, ?, ?, ?, ?, 'candidate', ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, example_id, role_code) DO NOTHING
RETURNING example_id`,
	qAgentEvalAddTotals: `
UPDATE evaluation_run
   SET sample_count = sample_count + 1, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
       cost_minor_units = cost_minor_units + ?, currency_code = COALESCE(currency_code, ?)
 WHERE partner_id = ? AND id = ? AND (CAST(? AS CHAR(3)) IS NULL OR currency_code IS NULL OR currency_code = ?)
RETURNING id`,
	qAgentEvalFinish: `
UPDATE evaluation_run
   SET status_code = ?, completed_at = CURRENT_TIMESTAMP, reason = ?, lease_token = NULL, lease_until = NULL
 WHERE partner_id = ? AND id = ? AND lease_token = ? AND status_code = 'running'
RETURNING id`,
}

// AgentEvaluationQueueQueries builds the keel leased-queue SQL that claims queued agent
// runs. A claim takes a lease on the store clock; a run whose lease expired is queued
// again, or failed once it has used maxAttempts.
func AgentEvaluationQueueQueries(lease time.Duration, batch, maxAttempts int) (queries map[string]string, pending, claim, reclaim string) {
	leaseSeconds := max(int(lease/time.Second), 1)
	batch, maxAttempts = max(batch, 1), max(maxAttempts, 1)
	pending, claim, reclaim = "scout_agent_evaluation_pending", "scout_agent_evaluation_claim", "scout_agent_evaluation_reclaim"
	queries = map[string]string{
		pending: fmt.Sprintf(`
SELECT id FROM evaluation_run
 WHERE status_code = 'queued' AND agent_id IS NOT NULL
 ORDER BY id
 LIMIT %d`, batch),
		claim: fmt.Sprintf(`
UPDATE evaluation_run
   SET status_code = 'running', lease_token = ?, lease_until = CURRENT_TIMESTAMP + INTERVAL '%d seconds',
       attempt = attempt + 1, started_at = COALESCE(started_at, CURRENT_TIMESTAMP)
 WHERE id = ? AND status_code = 'queued' AND agent_id IS NOT NULL
RETURNING id, partner_id, lease_token`, leaseSeconds),
		reclaim: fmt.Sprintf(`
UPDATE evaluation_run
   SET status_code = CASE WHEN attempt >= %[1]d THEN 'failed' ELSE 'queued' END,
       completed_at = CASE WHEN attempt >= %[1]d THEN CURRENT_TIMESTAMP ELSE NULL END,
       reason = CASE WHEN attempt >= %[1]d THEN 'lease expired after last attempt' ELSE reason END,
       lease_token = NULL, lease_until = NULL
 WHERE status_code = 'running' AND lease_token IS NOT NULL AND lease_until < CURRENT_TIMESTAMP
RETURNING id`, maxAttempts),
	}
	return queries, pending, claim, reclaim
}

// AgentEvaluationStore queues single-subject agent evaluations in evaluation_run and
// evaluation_run_subject and serves their results; it is also the worker's queue.
type AgentEvaluationStore struct {
	keelStore
}

var (
	_ contract.AgentEvaluationStore = (*AgentEvaluationStore)(nil)
	_ contract.AgentEvaluationQueue = (*AgentEvaluationStore)(nil)
)

func validEnqueue(request domain.AgentEvaluationEnqueue) error {
	if request.TenantID <= 0 || strings.TrimSpace(request.AgentID) == "" || request.SetVersion <= 0 {
		return fmt.Errorf("%w: tenant, agent, and a positive set version are required", domain.ErrValidation)
	}
	if err := validScope(request.Scope); err != nil {
		return err
	}
	if request.Subject.AgentID != request.AgentID {
		return fmt.Errorf("%w: subject agent %q is not agent %q", domain.ErrValidation, request.Subject.AgentID, request.AgentID)
	}
	switch request.Subject.Kind {
	case domain.SubjectDraft:
		if request.Draft == nil || request.Subject.Version != "" || !isSHA256Hex(request.Subject.DefinitionDigest) ||
			request.Draft.DefinitionDigest != request.Subject.DefinitionDigest || request.Draft.AgentID != request.AgentID {
			return fmt.Errorf("%w: a draft subject carries its frozen definition and that definition's digest", domain.ErrValidation)
		}
	case domain.SubjectRelease:
		if request.Draft != nil || strings.TrimSpace(request.Subject.Version) == "" {
			return fmt.Errorf("%w: a release subject names a version and carries no draft", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unknown evaluation subject %q", domain.ErrValidation, request.Subject.Kind)
	}
	return nil
}

// Enqueue pins the set version and subject and queues the run, refusing an agent at its open-run cap.
func (store *AgentEvaluationStore) Enqueue(ctx context.Context, request domain.AgentEvaluationEnqueue) (domain.AgentEvaluationRun, error) {
	if err := validEnqueue(request); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	var definition any
	if request.Draft != nil {
		encoded, err := json.Marshal(request.Draft)
		if err != nil {
			return domain.AgentEvaluationRun{}, fmt.Errorf("encode frozen draft: %w", err)
		}
		definition = string(encoded)
	}
	tx, err := store.begin(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	set, err := tx.Query(ctx, qAgentEvalLockSet, request.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("lock agent golden set: %w", err)
	}
	if len(set.Rows) == 0 {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: agent %q has no golden set", domain.ErrNotFound, request.AgentID)
	}
	run := domain.AgentEvaluationRun{
		TenantID: request.TenantID, AgentID: request.AgentID, GoldenSetID: common.AsString(set.Rows[0][0]), SetVersion: request.SetVersion,
		Scope: request.Scope, Subject: request.Subject, Status: domain.EvaluationQueued, RequestedBy: request.RequestedBy,
	}
	open, err := tx.Query(ctx, qAgentEvalOpenRuns, request.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("count open evaluations: %w", err)
	}
	if len(open.Rows) > 0 && common.AsInt64(open.Rows[0][0]) >= int64(request.MaxOpenRuns) {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: agent %q already has %d open evaluations", domain.ErrRateLimited, request.AgentID, request.MaxOpenRuns)
	}
	examples, err := tx.Query(ctx, qAgentEvalVersion, string(request.Scope), string(request.Scope), request.TenantID, run.GoldenSetID, request.SetVersion)
	if err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("load golden set version: %w", err)
	}
	if len(examples.Rows) == 0 {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: golden set %q version %d", domain.ErrNotFound, run.GoldenSetID, request.SetVersion)
	}
	if common.AsInt64(examples.Rows[0][0]) == 0 {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: golden set %q version %d has no examples in the %s scope", domain.ErrValidation, run.GoldenSetID, request.SetVersion, request.Scope)
	}
	if request.Subject.Kind == domain.SubjectRelease {
		release, err := tx.Query(ctx, qAgentEvalRelease, request.TenantID, request.AgentID, request.Subject.Version)
		if err != nil {
			return domain.AgentEvaluationRun{}, fmt.Errorf("load release: %w", err)
		}
		if len(release.Rows) == 0 {
			return domain.AgentEvaluationRun{}, fmt.Errorf("%w: release %q of agent %q", domain.ErrNotFound, request.Subject.Version, request.AgentID)
		}
		run.Subject.DefinitionDigest = strings.TrimSpace(common.AsString(release.Rows[0][0]))
	}
	run.RunID = tx.GenID()
	kind, id := principalColumns(request.RequestedBy)
	inserted, err := tx.Query(ctx, qAgentEvalInsertRun, run.RunID, run.TenantID, run.AgentID, run.GoldenSetID, run.SetVersion, string(run.Scope), kind, id)
	if err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("queue evaluation run: %w", err)
	}
	if len(inserted.Rows) > 0 {
		run.QueuedAt = common.AsTime(inserted.Rows[0][0])
	}
	if _, err = tx.Query(ctx, qAgentEvalInsertSubj, run.RunID, run.TenantID, string(run.Subject.Kind), run.AgentID,
		nullable(run.Subject.Version), run.Subject.DefinitionDigest, definition); err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("pin evaluation subject: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("commit evaluation run: %w", err)
	}
	committed = true
	return run, nil
}

// Run returns one agent run of the tenant with the count of results withheld from scope.
func (store *AgentEvaluationStore) Run(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64) (domain.AgentEvaluationRun, error) {
	run, _, err := store.run(ctx, scope, tenantID, runID)
	return run, err
}

func (store *AgentEvaluationStore) run(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64) (domain.AgentEvaluationRun, int64, error) {
	if err := validScope(scope); err != nil {
		return domain.AgentEvaluationRun{}, 0, err
	}
	if tenantID <= 0 || runID <= 0 {
		return domain.AgentEvaluationRun{}, 0, fmt.Errorf("%w: tenant and run id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return domain.AgentEvaluationRun{}, 0, err
	}
	result, err := qs.Query(ctx, qAgentEvalRun, string(scope), tenantID, runID)
	if err != nil {
		return domain.AgentEvaluationRun{}, 0, fmt.Errorf("load evaluation run: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentEvaluationRun{}, 0, fmt.Errorf("%w: evaluation run %d", domain.ErrNotFound, runID)
	}
	row := result.Rows[0]
	if len(row) < 23 {
		return domain.AgentEvaluationRun{}, 0, fmt.Errorf("evaluation run row: expected 23 columns, got %d", len(row))
	}
	run := domain.AgentEvaluationRun{
		RunID: common.AsInt64(row[0]), TenantID: tenantID, AgentID: common.AsString(row[1]), GoldenSetID: common.AsString(row[2]),
		SetVersion: common.AsInt64(row[3]), Scope: domain.GoldenScope(common.AsString(row[4])), Status: common.AsString(row[5]),
		QueuedAt: common.AsTime(row[6]), StartedAt: optionalTime(row[7]), CompletedAt: optionalTime(row[8]),
		RequestedBy: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[9])), ID: common.AsString(row[10])},
		Samples:     int(common.AsInt64(row[11])),
		Usage: domain.Usage{InputTokens: common.AsInt64(row[12]), OutputTokens: common.AsInt64(row[13]),
			CostMinorUnits: common.AsInt64(row[14]), Currency: strings.TrimSpace(common.AsString(row[15]))},
		Exponent: int(common.AsInt64(row[16])), Reason: common.AsString(row[17]),
		Subject: domain.AgentEvaluationSubject{Kind: domain.EvaluationSubjectKind(common.AsString(row[18])), AgentID: common.AsString(row[1]),
			Version: common.AsString(row[19]), DefinitionDigest: strings.TrimSpace(common.AsString(row[20]))},
		HiddenResults: int(common.AsInt64(row[22])),
	}
	return run, common.AsInt64(row[21]), nil
}

func optionalTime(value any) *time.Time {
	if value == nil {
		return nil
	}
	at := common.AsTime(value)
	if at.IsZero() {
		return nil
	}
	return &at
}

// Results pages the run's results visible in scope, in descending example id.
func (store *AgentEvaluationStore) Results(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64, before string, limit int) ([]domain.AgentCaseResult, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	if tenantID <= 0 || runID <= 0 || limit <= 0 {
		return nil, fmt.Errorf("%w: tenant, run id, and a positive limit are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return nil, err
	}
	rows, err := qs.Query(ctx, qAgentEvalResults, tenantID, runID, string(scope), string(scope), before, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list evaluation results: %w", err)
	}
	results := make([]domain.AgentCaseResult, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		if len(row) < 10 {
			return nil, fmt.Errorf("evaluation result row: expected 10 columns, got %d", len(row))
		}
		result := domain.AgentCaseResult{
			ExampleID: common.AsString(row[0]), Latency: time.Duration(common.AsInt64(row[2])) * time.Millisecond,
			Usage: domain.Usage{InputTokens: common.AsInt64(row[3]), OutputTokens: common.AsInt64(row[4]),
				CostMinorUnits: common.AsInt64(row[5]), Currency: strings.TrimSpace(common.AsString(row[6]))},
			Exponent: int(common.AsInt64(row[7])), NeedsHumanReview: common.AsBool(row[8]), Reason: common.AsString(row[9]),
		}
		if raw := common.AsString(row[1]); raw != "" && raw != "null" {
			if err := json.Unmarshal([]byte(raw), &result.Scores); err != nil {
				return nil, fmt.Errorf("decode scores for %q: %w", result.ExampleID, err)
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func validLease(lease domain.AgentEvaluationLease) error {
	if lease.TenantID <= 0 || lease.RunID <= 0 || lease.Token == 0 || lease.TTL < time.Second {
		return fmt.Errorf("%w: lease needs tenant, run, token, and a TTL of at least a second", domain.ErrValidation)
	}
	return nil
}

// Load returns the claimed run, its pinned draft, and the examples an earlier attempt settled.
func (store *AgentEvaluationStore) Load(ctx context.Context, lease domain.AgentEvaluationLease) (domain.AgentEvaluationClaim, error) {
	if err := validLease(lease); err != nil {
		return domain.AgentEvaluationClaim{}, err
	}
	run, token, err := store.run(ctx, domain.GoldenScopeGate, lease.TenantID, lease.RunID)
	if err != nil {
		return domain.AgentEvaluationClaim{}, err
	}
	if run.Status != domain.EvaluationRunning || token != lease.Token {
		return domain.AgentEvaluationClaim{}, fmt.Errorf("%w: run %d is not held by this lease", domain.ErrConflict, lease.RunID)
	}
	claim := domain.AgentEvaluationClaim{Lease: lease, Run: run, Recorded: map[string]domain.Usage{}}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return domain.AgentEvaluationClaim{}, err
	}
	if run.Subject.Kind == domain.SubjectDraft {
		draft, err := qs.Query(ctx, qAgentEvalDraft, lease.TenantID, lease.RunID)
		if err != nil {
			return domain.AgentEvaluationClaim{}, fmt.Errorf("load pinned draft: %w", err)
		}
		if len(draft.Rows) == 0 {
			return domain.AgentEvaluationClaim{}, fmt.Errorf("%w: pinned draft of run %d", domain.ErrNotFound, lease.RunID)
		}
		var definition domain.AgentDefinition
		if err := json.Unmarshal([]byte(common.AsString(draft.Rows[0][0])), &definition); err != nil {
			return domain.AgentEvaluationClaim{}, fmt.Errorf("decode pinned draft of run %d: %w", lease.RunID, err)
		}
		if definition.DefinitionDigest != run.Subject.DefinitionDigest || definition.AgentID != run.AgentID {
			return domain.AgentEvaluationClaim{}, fmt.Errorf("%w: pinned draft of run %d does not match its digest", domain.ErrConflict, lease.RunID)
		}
		claim.Draft = &definition
	}
	recorded, err := qs.Query(ctx, qAgentEvalRecorded, lease.TenantID, lease.RunID)
	if err != nil {
		return domain.AgentEvaluationClaim{}, fmt.Errorf("load recorded results: %w", err)
	}
	for _, row := range recorded.Rows {
		claim.Recorded[common.AsString(row[0])] = domain.Usage{InputTokens: common.AsInt64(row[1]), OutputTokens: common.AsInt64(row[2]),
			CostMinorUnits: common.AsInt64(row[3]), Currency: strings.TrimSpace(common.AsString(row[4]))}
	}
	return claim, nil
}

// Record renews the lease, stores one result, and adds it to the run's totals in one
// transaction; a result priced in another currency than the run is domain.ErrValidation.
func (store *AgentEvaluationStore) Record(ctx context.Context, lease domain.AgentEvaluationLease, result domain.EvaluationResult) error {
	if err := validLease(lease); err != nil {
		return err
	}
	if strings.TrimSpace(result.ExampleID) == "" || result.Usage.CostMinorUnits < 0 || result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 || result.Latency < 0 {
		return fmt.Errorf("%w: a result needs an example id and non-negative usage", domain.ErrValidation)
	}
	if result.Usage.CostMinorUnits > 0 && len(result.Usage.Currency) != 3 {
		return fmt.Errorf("%w: a priced result needs an ISO currency", domain.ErrValidation)
	}
	scores, err := json.Marshal(result.Scores)
	if err != nil {
		return fmt.Errorf("encode scores: %w", err)
	}
	tx, err := store.begin(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	renewed, err := tx.Query(ctx, qAgentEvalRenew, int64(lease.TTL/time.Second), lease.TenantID, lease.RunID, lease.Token)
	if err != nil {
		return fmt.Errorf("renew evaluation lease: %w", err)
	}
	if len(renewed.Rows) == 0 {
		return fmt.Errorf("%w: run %d is no longer held by this lease", domain.ErrConflict, lease.RunID)
	}
	goldenSetID, setVersion := common.AsString(renewed.Rows[0][0]), common.AsInt64(renewed.Rows[0][1])
	// A free result carries no currency, so it can never fix the run's currency.
	var currency any
	if result.Usage.CostMinorUnits > 0 {
		currency = result.Usage.Currency
	}
	inserted, err := tx.Query(ctx, qAgentEvalPutResult, lease.RunID, lease.TenantID, goldenSetID, setVersion, result.ExampleID, string(scores),
		result.Latency.Milliseconds(), result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CostMinorUnits, currency,
		result.NeedsHumanReview, nullable(truncate(result.Reason, 400)))
	if err != nil {
		return fmt.Errorf("insert evaluation result %q: %w", result.ExampleID, err)
	}
	if len(inserted.Rows) > 0 {
		totals, err := tx.Query(ctx, qAgentEvalAddTotals, result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CostMinorUnits,
			currency, lease.TenantID, lease.RunID, currency, currency)
		if err != nil {
			return fmt.Errorf("add evaluation totals: %w", err)
		}
		if len(totals.Rows) == 0 {
			return fmt.Errorf("%w: result %q is priced in %s, unlike the run", domain.ErrValidation, result.ExampleID, result.Usage.Currency)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit evaluation result: %w", err)
	}
	committed = true
	return nil
}

// Finish records the terminal status and releases the lease; totals were settled per result.
func (store *AgentEvaluationStore) Finish(ctx context.Context, lease domain.AgentEvaluationLease, status, reason string) error {
	if err := validLease(lease); err != nil {
		return err
	}
	switch status {
	case domain.EvaluationCompleted, domain.EvaluationStoppedEarly, domain.EvaluationFailed:
	default:
		return fmt.Errorf("%w: %q is not a terminal run status", domain.ErrValidation, status)
	}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return err
	}
	finished, err := qs.Query(ctx, qAgentEvalFinish, status, nullable(truncate(reason, 400)), lease.TenantID, lease.RunID, lease.Token)
	if err != nil {
		return fmt.Errorf("finish evaluation run: %w", err)
	}
	if len(finished.Rows) == 0 {
		return fmt.Errorf("%w: run %d is no longer held by this lease", domain.ErrConflict, lease.RunID)
	}
	return nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := 0
	for index := range value {
		if index > limit {
			break
		}
		cut = index
	}
	return value[:cut]
}
