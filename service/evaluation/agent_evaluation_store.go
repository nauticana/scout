package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
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

	qAgentEvalBaseline        = "scout_agent_evaluation_baseline"
	qAgentEvalBaselineOutputs = "scout_agent_evaluation_baseline_outputs"
	qAgentEvalCompared        = "scout_agent_evaluation_compared"
	qAgentEvalPutComparison   = "scout_agent_evaluation_put_comparison"
	qAgentEvalPutArm          = "scout_agent_evaluation_put_arm"
	qAgentEvalComparisons     = "scout_agent_evaluation_comparisons"
	qAgentEvalTally           = "scout_agent_evaluation_tally"
)

// The run and results reads take the reader's scope: a dev reader never sees a
// gate example's result, only how many were withheld.
var agentEvaluationQueries = map[string]string{
	qAgentEvalLockSet: `
SELECT golden_set_id FROM agent_golden_set WHERE partner_id = ? AND agent_id = ? FOR UPDATE`,
	qAgentEvalOpenRuns: `
SELECT COUNT(*) FROM evaluation_run WHERE partner_id = ? AND agent_id = ? AND status_code IN (` + sqlRunQueued + `, ` + sqlRunRunning + `)`,
	qAgentEvalVersion: `
SELECT COUNT(e.example_id)
  FROM golden_set_version v
  LEFT JOIN golden_example e ON e.partner_id = v.partner_id AND e.golden_set_id = v.golden_set_id AND e.set_version = v.set_version
                            AND (e.scope_code = ? OR ? = ` + sqlScopeGate + `)
 WHERE v.partner_id = ? AND v.golden_set_id = ? AND v.set_version = ?
 GROUP BY v.set_version`,
	qAgentEvalRelease: `
SELECT definition_digest FROM agent_version WHERE partner_id = ? AND agent_id = ? AND agent_version = ?`,
	qAgentEvalInsertRun: `
INSERT INTO evaluation_run (id, partner_id, agent_id, golden_set_id, set_version, scope_code, status_code, started_at,
                            requested_by_kind, requested_by_id)
VALUES (?, ?, ?, ?, ?, ?, ` + sqlRunQueued + `, NULL, ?, ?)
RETURNING queued_at`,
	qAgentEvalInsertSubj: `
INSERT INTO evaluation_run_subject (run_id, partner_id, subject_code, agent_id, agent_version, definition_digest, definition_json, settings_json,
                                    role_code, baseline_id, provider_id, model_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qAgentEvalRun: `
SELECT r.id, r.agent_id, r.golden_set_id, r.set_version, r.scope_code, r.status_code, r.queued_at, r.started_at, r.completed_at,
       r.requested_by_kind, r.requested_by_id, r.sample_count, r.input_tokens, r.output_tokens, r.cost_minor_units, r.currency_code,
       COALESCE(c.exponent, 0), r.reason, s.subject_code, s.agent_version, s.definition_digest, r.lease_token,
       (SELECT COUNT(DISTINCT x.example_id)
          FROM evaluation_result x
          JOIN golden_example e ON e.partner_id = x.partner_id AND e.golden_set_id = x.golden_set_id
                               AND e.set_version = x.set_version AND e.example_id = x.example_id
         WHERE x.partner_id = r.partner_id AND x.run_id = r.id AND e.scope_code = ` + sqlScopeGate + ` AND ? <> ` + sqlScopeGate + `),
       s.provider_id, s.model_id, b.subject_code, b.agent_version, b.definition_digest, b.baseline_id, b.provider_id, b.model_id
  FROM evaluation_run r
  JOIN evaluation_run_subject s ON s.partner_id = r.partner_id AND s.run_id = r.id AND s.role_code = ` + sqlRoleCandidate + `
  LEFT JOIN evaluation_run_subject b ON b.partner_id = r.partner_id AND b.run_id = r.id AND b.role_code = ` + sqlRoleBaseline + `
  LEFT JOIN currency c ON c.code = r.currency_code
 WHERE r.partner_id = ? AND r.id = ? AND r.agent_id IS NOT NULL`,
	qAgentEvalResults: `
SELECT x.example_id, x.scores, x.latency_ms, x.input_tokens, x.output_tokens, x.cost_minor_units, x.currency_code,
       COALESCE(c.exponent, 0), x.needs_human_review, x.reason,
       x.evaluator_input_tokens, x.evaluator_output_tokens, x.evaluator_cost_minor_units
  FROM evaluation_result x
  JOIN golden_example e ON e.partner_id = x.partner_id AND e.golden_set_id = x.golden_set_id
                       AND e.set_version = x.set_version AND e.example_id = x.example_id
  LEFT JOIN currency c ON c.code = x.currency_code
 WHERE x.partner_id = ? AND x.run_id = ? AND x.role_code = ` + sqlRoleCandidate + `
   AND (e.scope_code = ? OR ? = ` + sqlScopeGate + `)
   AND (? = '' OR x.example_id < ?)
 ORDER BY x.example_id DESC
 LIMIT ?`,
	qAgentEvalDraft: `
SELECT definition_json, settings_json FROM evaluation_run_subject WHERE partner_id = ? AND run_id = ? AND role_code = ?`,
	qAgentEvalRecorded: `
SELECT example_id, input_tokens + evaluator_input_tokens, output_tokens + evaluator_output_tokens,
       cost_minor_units + evaluator_cost_minor_units, currency_code, reason
  FROM evaluation_result
 WHERE partner_id = ? AND run_id = ? AND role_code = ` + sqlRoleCandidate + ``,
	qAgentEvalRenew: `
UPDATE evaluation_run
   SET lease_until = CURRENT_TIMESTAMP + (? * INTERVAL '1 second')
 WHERE partner_id = ? AND id = ? AND lease_token = ? AND status_code = ` + sqlRunRunning + `
RETURNING golden_set_id, set_version`,
	qAgentEvalPutResult: `
INSERT INTO evaluation_result (run_id, partner_id, golden_set_id, set_version, example_id, role_code, scores, latency_ms,
                               input_tokens, output_tokens, cost_minor_units, currency_code, needs_human_review, reason,
                               evaluator_input_tokens, evaluator_output_tokens, evaluator_cost_minor_units)
VALUES (?, ?, ?, ?, ?, ` + sqlRoleCandidate + `, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
 WHERE partner_id = ? AND id = ? AND lease_token = ? AND status_code = ` + sqlRunRunning + `
RETURNING id`,
	qAgentEvalBaseline: `
SELECT scope_code FROM evaluation_baseline WHERE partner_id = ? AND id = ? AND golden_set_id = ? AND set_version = ?`,
	qAgentEvalBaselineOutputs: `
SELECT example_id, output_uri, output_digest FROM evaluation_baseline_output WHERE partner_id = ? AND baseline_id = ?`,
	// A comparison and both its arms are recorded in one transaction, so the arms' spend joins the verdict's.
	qAgentEvalCompared: `
SELECT c.example_id, c.evaluator_input_tokens + COALESCE(SUM(x.input_tokens), 0),
       c.evaluator_output_tokens + COALESCE(SUM(x.output_tokens), 0),
       c.evaluator_cost_minor_units + COALESCE(SUM(x.cost_minor_units), 0),
       COALESCE(c.currency_code, MAX(x.currency_code)), c.reason
  FROM evaluation_comparison c
  LEFT JOIN evaluation_result x ON x.partner_id = c.partner_id AND x.run_id = c.run_id AND x.example_id = c.example_id
 WHERE c.partner_id = ? AND c.run_id = ?
 GROUP BY c.example_id, c.evaluator_input_tokens, c.evaluator_output_tokens, c.evaluator_cost_minor_units, c.currency_code, c.reason`,
	qAgentEvalPutComparison: `
INSERT INTO evaluation_comparison (run_id, partner_id, golden_set_id, set_version, example_id, verdict_code, confidence,
                                   evaluator_kind, evaluator_version, input_digest, rationale,
                                   evaluator_input_tokens, evaluator_output_tokens, evaluator_cost_minor_units, currency_code, reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, example_id) DO NOTHING
RETURNING example_id`,
	qAgentEvalPutArm: `
INSERT INTO evaluation_result (run_id, partner_id, golden_set_id, set_version, example_id, role_code, scores, latency_ms,
                               input_tokens, output_tokens, cost_minor_units, currency_code, output_uri, output_digest)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qAgentEvalComparisons: `
SELECT c.example_id, c.verdict_code, CAST(c.confidence AS DOUBLE PRECISION), c.evaluator_kind, c.evaluator_version, c.rationale, c.reason,
       c.evaluator_input_tokens, c.evaluator_output_tokens, c.evaluator_cost_minor_units, c.currency_code, COALESCE(cc.exponent, 0),
       a.scores, a.latency_ms, a.input_tokens, a.output_tokens, a.cost_minor_units, a.currency_code, COALESCE(ac.exponent, 0), a.output_uri, a.output_digest,
       b.scores, b.latency_ms, b.input_tokens, b.output_tokens, b.cost_minor_units, b.currency_code, COALESCE(bc.exponent, 0), b.output_uri, b.output_digest
  FROM evaluation_comparison c
  JOIN golden_example e ON e.partner_id = c.partner_id AND e.golden_set_id = c.golden_set_id
                       AND e.set_version = c.set_version AND e.example_id = c.example_id
  LEFT JOIN evaluation_result a ON a.partner_id = c.partner_id AND a.run_id = c.run_id AND a.example_id = c.example_id AND a.role_code = ` + sqlRoleBaseline + `
  LEFT JOIN evaluation_result b ON b.partner_id = c.partner_id AND b.run_id = c.run_id AND b.example_id = c.example_id AND b.role_code = ` + sqlRoleCandidate + `
  LEFT JOIN currency cc ON cc.code = c.currency_code
  LEFT JOIN currency ac ON ac.code = a.currency_code
  LEFT JOIN currency bc ON bc.code = b.currency_code
 WHERE c.partner_id = ? AND c.run_id = ?
   AND (e.scope_code = ? OR ? = ` + sqlScopeGate + `)
   AND (? = '' OR c.example_id < ?)
 ORDER BY c.example_id DESC
 LIMIT ?`,
	qAgentEvalTally: `
SELECT COALESCE(c.verdict_code, ''), COUNT(*)
  FROM evaluation_comparison c
  JOIN golden_example e ON e.partner_id = c.partner_id AND e.golden_set_id = c.golden_set_id
                       AND e.set_version = c.set_version AND e.example_id = c.example_id
 WHERE c.partner_id = ? AND c.run_id = ? AND (e.scope_code = ? OR ? = ` + sqlScopeGate + `)
 GROUP BY c.verdict_code`,
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
 WHERE status_code = `+sqlRunQueued+` AND agent_id IS NOT NULL
 ORDER BY id
 LIMIT %d`, batch),
		claim: fmt.Sprintf(`
UPDATE evaluation_run
   SET status_code = `+sqlRunRunning+`, lease_token = ?, lease_until = CURRENT_TIMESTAMP + INTERVAL '%d seconds',
       attempt = attempt + 1, started_at = COALESCE(started_at, CURRENT_TIMESTAMP)
 WHERE id = ? AND status_code = `+sqlRunQueued+` AND agent_id IS NOT NULL
RETURNING id, partner_id, lease_token`, leaseSeconds),
		reclaim: fmt.Sprintf(`
UPDATE evaluation_run
   SET status_code = CASE WHEN attempt >= %[1]d THEN `+sqlRunFailed+` ELSE `+sqlRunQueued+` END,
       completed_at = CASE WHEN attempt >= %[1]d THEN CURRENT_TIMESTAMP ELSE NULL END,
       reason = CASE WHEN attempt >= %[1]d THEN 'lease expired after last attempt' ELSE reason END,
       lease_token = NULL, lease_until = NULL
 WHERE status_code = `+sqlRunRunning+` AND lease_token IS NOT NULL AND lease_until < CURRENT_TIMESTAMP
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
	if request.Subject.Kind == domain.SubjectBaseline {
		return fmt.Errorf("%w: only arm a of a comparison may be a baseline", domain.ErrValidation)
	}
	if err := validArm(request.AgentID, domain.EvaluationArm{Subject: request.Subject, Draft: request.Draft, DraftSettings: request.DraftSettings}); err != nil {
		return err
	}
	if request.Baseline != nil {
		return validArm(request.AgentID, *request.Baseline)
	}
	return nil
}

// validArm checks that an arm pins what its kind executes: a draft its frozen definition, the
// settings that definition digests, and the model it was frozen on; a release its version; a
// baseline its id. Recorded outputs are loaded by the worker, never enqueued.
func validArm(agentID string, arm domain.EvaluationArm) error {
	subject := arm.Subject
	if subject.AgentID != agentID {
		return fmt.Errorf("%w: subject agent %q is not agent %q", domain.ErrValidation, subject.AgentID, agentID)
	}
	if arm.Outputs != nil || subject.Model != nil && subject.Kind != domain.SubjectDraft {
		return fmt.Errorf("%w: only a draft subject names a model, and outputs are never enqueued", domain.ErrValidation)
	}
	switch subject.Kind {
	case domain.SubjectDraft:
		draft := arm.Draft
		if draft == nil || subject.Version != "" || subject.BaselineID != 0 || !isSHA256Hex(subject.DefinitionDigest) ||
			draft.DefinitionDigest != subject.DefinitionDigest || draft.AgentID != agentID {
			return fmt.Errorf("%w: a draft subject carries its frozen definition and that definition's digest", domain.ErrValidation)
		}
		if draft.SettingsDigest == "" || scope.SettingsDigest(arm.DraftSettings) != draft.SettingsDigest {
			return fmt.Errorf("%w: a draft subject carries the settings its definition digests", domain.ErrValidation)
		}
		if model := subject.Model; model != nil && (draft.Models.Text == nil || *draft.Models.Text != *model || model.ProviderID == "" || model.ModelID == "") {
			return fmt.Errorf("%w: a draft frozen on another model carries that model as its text model", domain.ErrValidation)
		}
	case domain.SubjectRelease:
		if arm.Draft != nil || arm.DraftSettings != nil || strings.TrimSpace(subject.Version) == "" || subject.BaselineID != 0 {
			return fmt.Errorf("%w: a release subject names a version and carries no draft", domain.ErrValidation)
		}
	case domain.SubjectBaseline:
		if arm.Draft != nil || arm.DraftSettings != nil || subject.Version != "" || subject.DefinitionDigest != "" || subject.BaselineID <= 0 {
			return fmt.Errorf("%w: a baseline subject names only its baseline", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unknown evaluation subject %q", domain.ErrValidation, subject.Kind)
	}
	return nil
}

// Enqueue pins the set version and subject and queues the run, refusing an agent at its open-run cap.
func (store *AgentEvaluationStore) Enqueue(ctx context.Context, request domain.AgentEvaluationEnqueue) (domain.AgentEvaluationRun, error) {
	if err := validEnqueue(request); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	candidate := domain.EvaluationArm{Subject: request.Subject, Draft: request.Draft, DraftSettings: request.DraftSettings}
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
	if run.Subject, err = pinSubject(ctx, tx, request, candidate.Subject, run.GoldenSetID); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	if request.Baseline != nil {
		subject, err := pinSubject(ctx, tx, request, request.Baseline.Subject, run.GoldenSetID)
		if err != nil {
			return domain.AgentEvaluationRun{}, err
		}
		if subject.DefinitionDigest != "" && subject.DefinitionDigest == run.Subject.DefinitionDigest {
			return domain.AgentEvaluationRun{}, fmt.Errorf("%w: arms a and b are the same definition", domain.ErrValidation)
		}
		run.Baseline = &subject
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
	candidate.Subject = run.Subject
	if err = insertSubject(ctx, tx, run, domain.RoleCandidate, candidate); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	if request.Baseline != nil {
		baseline := *request.Baseline
		baseline.Subject = *run.Baseline
		if err = insertSubject(ctx, tx, run, domain.RoleBaseline, baseline); err != nil {
			return domain.AgentEvaluationRun{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentEvaluationRun{}, fmt.Errorf("commit evaluation run: %w", err)
	}
	committed = true
	return run, nil
}

// pinSubject resolves a release subject's digest and checks that a baseline subject answers
// the run's set version in a scope covering the run's.
func pinSubject(ctx context.Context, tx keelport.TxQueryService, request domain.AgentEvaluationEnqueue, subject domain.AgentEvaluationSubject, goldenSetID string) (domain.AgentEvaluationSubject, error) {
	switch subject.Kind {
	case domain.SubjectRelease:
		release, err := tx.Query(ctx, qAgentEvalRelease, request.TenantID, request.AgentID, subject.Version)
		if err != nil {
			return subject, fmt.Errorf("load release: %w", err)
		}
		if len(release.Rows) == 0 {
			return subject, fmt.Errorf("%w: release %q of agent %q", domain.ErrNotFound, subject.Version, request.AgentID)
		}
		subject.DefinitionDigest = strings.TrimSpace(common.AsString(release.Rows[0][0]))
	case domain.SubjectBaseline:
		baseline, err := tx.Query(ctx, qAgentEvalBaseline, request.TenantID, subject.BaselineID, goldenSetID, request.SetVersion)
		if err != nil {
			return subject, fmt.Errorf("load baseline: %w", err)
		}
		if len(baseline.Rows) == 0 {
			return subject, fmt.Errorf("%w: baseline %d of golden set %q version %d", domain.ErrNotFound, subject.BaselineID, goldenSetID, request.SetVersion)
		}
		if recorded := domain.GoldenScope(common.AsString(baseline.Rows[0][0])); recorded != domain.GoldenScopeGate && recorded != request.Scope {
			return subject, fmt.Errorf("%w: baseline %d does not answer the %s scope", domain.ErrValidation, subject.BaselineID, request.Scope)
		}
	}
	return subject, nil
}

func insertSubject(ctx context.Context, tx keelport.TxQueryService, run domain.AgentEvaluationRun, role domain.EvaluationRole, arm domain.EvaluationArm) error {
	var definition, settings, baseline, provider, model any
	if arm.Draft != nil {
		encoded, err := json.Marshal(arm.Draft)
		if err != nil {
			return fmt.Errorf("encode frozen draft: %w", err)
		}
		definition = string(encoded)
		if encoded, err = json.Marshal(append([]domain.EffectiveResource{}, arm.DraftSettings...)); err != nil {
			return fmt.Errorf("encode frozen draft settings: %w", err)
		}
		settings = string(encoded)
	}
	subject := arm.Subject
	if subject.BaselineID > 0 {
		baseline = subject.BaselineID
	}
	if subject.Model != nil {
		provider, model = subject.Model.ProviderID, subject.Model.ModelID
	}
	if _, err := tx.Query(ctx, qAgentEvalInsertSubj, run.RunID, run.TenantID, string(subject.Kind), run.AgentID,
		nullable(subject.Version), nullable(subject.DefinitionDigest), definition, settings, string(role), baseline, provider, model); err != nil {
		return fmt.Errorf("pin evaluation %s subject: %w", role, err)
	}
	return nil
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
	if len(row) < 31 {
		return domain.AgentEvaluationRun{}, 0, fmt.Errorf("evaluation run row: expected 31 columns, got %d", len(row))
	}
	run := domain.AgentEvaluationRun{
		RunID: common.AsInt64(row[0]), TenantID: tenantID, AgentID: common.AsString(row[1]), GoldenSetID: common.AsString(row[2]),
		SetVersion: common.AsInt64(row[3]), Scope: domain.GoldenScope(common.AsString(row[4])), Status: domain.EvaluationRunStatus(common.AsString(row[5])),
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
	if !run.Status.Valid() || !run.Scope.Valid() {
		return domain.AgentEvaluationRun{}, 0, fmt.Errorf("evaluation run %d: unknown status %q or scope %q", runID, run.Status, run.Scope)
	}
	run.Subject.Model = optionalModel(row[23], row[24])
	if kind := common.AsString(row[25]); kind != "" {
		run.Baseline = &domain.AgentEvaluationSubject{Kind: domain.EvaluationSubjectKind(kind), AgentID: run.AgentID,
			Version: common.AsString(row[26]), DefinitionDigest: strings.TrimSpace(common.AsString(row[27])), BaselineID: common.AsInt64(row[28]),
			Model: optionalModel(row[29], row[30])}
	}
	return run, common.AsInt64(row[21]), nil
}

func optionalModel(provider, model any) *domain.ModelReference {
	reference := domain.ModelReference{ProviderID: strings.TrimSpace(common.AsString(provider)), ModelID: strings.TrimSpace(common.AsString(model))}
	if reference.ProviderID == "" || reference.ModelID == "" {
		return nil
	}
	return &reference
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
		if len(row) < 13 {
			return nil, fmt.Errorf("evaluation result row: expected 13 columns, got %d", len(row))
		}
		result := domain.AgentCaseResult{
			ExampleID: common.AsString(row[0]), Latency: time.Duration(common.AsInt64(row[2])) * time.Millisecond,
			Usage: domain.Usage{InputTokens: common.AsInt64(row[3]), OutputTokens: common.AsInt64(row[4]),
				CostMinorUnits: common.AsInt64(row[5]), Currency: strings.TrimSpace(common.AsString(row[6]))},
			Exponent: int(common.AsInt64(row[7])), NeedsHumanReview: common.AsBool(row[8]), Reason: common.AsString(row[9]),
		}
		result.EvaluatorUsage = domain.Usage{InputTokens: common.AsInt64(row[10]), OutputTokens: common.AsInt64(row[11]), CostMinorUnits: common.AsInt64(row[12])}
		if result.EvaluatorUsage.CostMinorUnits > 0 {
			result.EvaluatorUsage.Currency = result.Usage.Currency
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
	claim := domain.AgentEvaluationClaim{Lease: lease, Run: run, Recorded: map[string]domain.Usage{}, Unscored: map[string]string{}}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return domain.AgentEvaluationClaim{}, err
	}
	candidate, err := store.pinnedArm(ctx, qs, run, domain.RoleCandidate, run.Subject)
	if err != nil {
		return domain.AgentEvaluationClaim{}, err
	}
	claim.Draft, claim.DraftSettings = candidate.Draft, candidate.DraftSettings
	recordedQuery := qAgentEvalRecorded
	if run.Baseline != nil {
		baseline, err := store.pinnedArm(ctx, qs, run, domain.RoleBaseline, *run.Baseline)
		if err != nil {
			return domain.AgentEvaluationClaim{}, err
		}
		claim.Baseline, recordedQuery = &baseline, qAgentEvalCompared
	}
	recorded, err := qs.Query(ctx, recordedQuery, lease.TenantID, lease.RunID)
	if err != nil {
		return domain.AgentEvaluationClaim{}, fmt.Errorf("load recorded results: %w", err)
	}
	for _, row := range recorded.Rows {
		if len(row) < 6 {
			return domain.AgentEvaluationClaim{}, fmt.Errorf("recorded result row: expected 6 columns, got %d", len(row))
		}
		exampleID := common.AsString(row[0])
		claim.Recorded[exampleID] = domain.Usage{InputTokens: common.AsInt64(row[1]), OutputTokens: common.AsInt64(row[2]),
			CostMinorUnits: common.AsInt64(row[3]), Currency: strings.TrimSpace(common.AsString(row[4]))}
		// Only a scoring failure records a reason; such an example must fail the run on every attempt.
		if reason := common.AsString(row[5]); reason != "" {
			claim.Unscored[exampleID] = reason
		}
	}
	return claim, nil
}

// pinnedArm loads what one arm of a claimed run executes: a draft's pinned definition and
// settings, checked against their digests, or a baseline's recorded outputs.
func (store *AgentEvaluationStore) pinnedArm(ctx context.Context, qs keelport.QueryService, run domain.AgentEvaluationRun, role domain.EvaluationRole, subject domain.AgentEvaluationSubject) (domain.EvaluationArm, error) {
	arm := domain.EvaluationArm{Subject: subject}
	switch subject.Kind {
	case domain.SubjectDraft:
		draft, err := qs.Query(ctx, qAgentEvalDraft, run.TenantID, run.RunID, string(role))
		if err != nil {
			return arm, fmt.Errorf("load pinned draft: %w", err)
		}
		if len(draft.Rows) == 0 {
			return arm, fmt.Errorf("%w: pinned draft of run %d", domain.ErrNotFound, run.RunID)
		}
		var definition domain.AgentDefinition
		if err := json.Unmarshal([]byte(common.AsString(draft.Rows[0][0])), &definition); err != nil {
			return arm, fmt.Errorf("decode pinned draft of run %d: %w", run.RunID, err)
		}
		if definition.DefinitionDigest != subject.DefinitionDigest || definition.AgentID != run.AgentID {
			return arm, fmt.Errorf("%w: pinned draft of run %d does not match its digest", domain.ErrConflict, run.RunID)
		}
		var settings []domain.EffectiveResource
		if err := json.Unmarshal([]byte(common.AsString(draft.Rows[0][1])), &settings); err != nil {
			return arm, fmt.Errorf("decode pinned draft settings of run %d: %w", run.RunID, err)
		}
		if definition.SettingsDigest == "" || scope.SettingsDigest(settings) != definition.SettingsDigest {
			return arm, fmt.Errorf("%w: pinned draft settings of run %d do not match its definition", domain.ErrConflict, run.RunID)
		}
		arm.Draft, arm.DraftSettings = &definition, settings
	case domain.SubjectBaseline:
		outputs, err := qs.Query(ctx, qAgentEvalBaselineOutputs, run.TenantID, subject.BaselineID)
		if err != nil {
			return arm, fmt.Errorf("load baseline outputs: %w", err)
		}
		arm.Outputs = make(map[string]domain.ObjectRef, len(outputs.Rows))
		for _, row := range outputs.Rows {
			if len(row) < 3 {
				return arm, fmt.Errorf("baseline output row: expected 3 columns, got %d", len(row))
			}
			arm.Outputs[common.AsString(row[0])] = domain.ObjectRef{URI: common.AsString(row[1]), Digest: strings.TrimSpace(common.AsString(row[2]))}
		}
	}
	return arm, nil
}

// Record renews the lease, stores one result, and adds it to the run's totals in one
// transaction; a result priced in another currency than the run is domain.ErrValidation.
func (store *AgentEvaluationStore) Record(ctx context.Context, lease domain.AgentEvaluationLease, result domain.EvaluationResult) error {
	if err := validLease(lease); err != nil {
		return err
	}
	if strings.TrimSpace(result.ExampleID) == "" || result.Latency < 0 {
		return fmt.Errorf("%w: a result needs an example id and a non-negative latency", domain.ErrValidation)
	}
	total, err := resultTotal(result)
	if err != nil {
		return err
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
	if total.CostMinorUnits > 0 {
		currency = total.Currency
	}
	evaluator := result.EvaluatorUsage
	inserted, err := tx.Query(ctx, qAgentEvalPutResult, lease.RunID, lease.TenantID, goldenSetID, setVersion, result.ExampleID, string(scores),
		result.Latency.Milliseconds(), result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CostMinorUnits, currency,
		result.NeedsHumanReview, nullable(truncate(result.Reason, 400)), evaluator.InputTokens, evaluator.OutputTokens, evaluator.CostMinorUnits)
	if err != nil {
		return fmt.Errorf("insert evaluation result %q: %w", result.ExampleID, err)
	}
	if len(inserted.Rows) > 0 {
		totals, err := tx.Query(ctx, qAgentEvalAddTotals, total.InputTokens, total.OutputTokens, total.CostMinorUnits,
			currency, lease.TenantID, lease.RunID, currency, currency)
		if err != nil {
			return fmt.Errorf("add evaluation totals: %w", err)
		}
		if len(totals.Rows) == 0 {
			return fmt.Errorf("%w: result %q is priced in %s, unlike the run", domain.ErrValidation, result.ExampleID, total.Currency)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit evaluation result: %w", err)
	}
	committed = true
	return nil
}

// resultTotal is the subject's and the evaluators' spend together, as the run's totals
// count it: both must be non-negative, priced spend needs an ISO currency, and the two
// priced parts must share it, since the row keeps one currency.
func resultTotal(result domain.EvaluationResult) (domain.Usage, error) {
	subject, evaluator := result.Usage, result.EvaluatorUsage
	for _, usage := range []domain.Usage{subject, evaluator} {
		if usage.CostMinorUnits < 0 || usage.InputTokens < 0 || usage.OutputTokens < 0 {
			return domain.Usage{}, fmt.Errorf("%w: a result needs non-negative usage", domain.ErrValidation)
		}
		if usage.CostMinorUnits > 0 && len(usage.Currency) != 3 {
			return domain.Usage{}, fmt.Errorf("%w: a priced result needs an ISO currency", domain.ErrValidation)
		}
	}
	if subject.CostMinorUnits > 0 && evaluator.CostMinorUnits > 0 && subject.Currency != evaluator.Currency {
		return domain.Usage{}, fmt.Errorf("%w: evaluator spend in %s differs from the result's %s", domain.ErrValidation, evaluator.Currency, subject.Currency)
	}
	if subject.InputTokens > math.MaxInt64-evaluator.InputTokens || subject.OutputTokens > math.MaxInt64-evaluator.OutputTokens ||
		subject.CostMinorUnits > math.MaxInt64-evaluator.CostMinorUnits {
		return domain.Usage{}, fmt.Errorf("%w: result usage overflows", domain.ErrValidation)
	}
	total := domain.Usage{InputTokens: subject.InputTokens + evaluator.InputTokens, OutputTokens: subject.OutputTokens + evaluator.OutputTokens,
		CostMinorUnits: subject.CostMinorUnits + evaluator.CostMinorUnits, Currency: subject.Currency}
	if subject.CostMinorUnits == 0 {
		total.Currency = evaluator.Currency
	}
	return total, nil
}

// Finish records the terminal status and releases the lease; totals were settled per result.
func (store *AgentEvaluationStore) Finish(ctx context.Context, lease domain.AgentEvaluationLease, status domain.EvaluationRunStatus, reason string) error {
	if err := validLease(lease); err != nil {
		return err
	}
	if !status.Terminal() {
		return fmt.Errorf("%w: %q is not a terminal run status", domain.ErrValidation, status)
	}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return err
	}
	finished, err := qs.Query(ctx, qAgentEvalFinish, string(status), nullable(truncate(reason, 400)), lease.TenantID, lease.RunID, lease.Token)
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
