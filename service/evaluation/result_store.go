package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qRunInsert    = "scout_evaluation_run_insert"
	qRunFinish    = "scout_evaluation_run_finish"
	qRunGetSet    = "scout_evaluation_run_get_set"
	qResultInsert = "scout_evaluation_result_insert"
	qResultList   = "scout_evaluation_result_list"
)

var resultQueries = map[string]string{
	qRunInsert: `
INSERT INTO evaluation_run (id, tenant_id, manifest_id, scope_code, status_code, started_at)
VALUES (?, ?, ?, ?, ?, ?)`,
	qRunFinish: `
UPDATE evaluation_run
   SET status_code = ?, completed_at = ?, sample_count = ?, input_tokens = ?, output_tokens = ?, cost_minor_units = ?, currency_code = ?
 WHERE tenant_id = ? AND id = ? AND status_code = 'running'
RETURNING id`,
	qRunGetSet: `
SELECT run.manifest_id, manifest.golden_set_id, manifest.golden_set_version
  FROM evaluation_run run
  JOIN evaluation_manifest manifest ON manifest.manifest_id = run.manifest_id
 WHERE run.tenant_id = ? AND run.id = ?`,
	qResultInsert: `
INSERT INTO evaluation_result (run_id, tenant_id, golden_set_id, set_version, example_id, role_code, scores, latency_ms,
                               input_tokens, output_tokens, cost_minor_units, currency_code, needs_human_review, reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qResultList: `
SELECT run.manifest_id, result.example_id, result.role_code, result.scores, result.latency_ms, result.input_tokens,
       result.output_tokens, result.cost_minor_units, result.currency_code, result.needs_human_review, result.reason
  FROM evaluation_result result
  JOIN evaluation_run run ON run.id = result.run_id
 WHERE result.tenant_id = ? AND result.run_id = ?
 ORDER BY result.example_id, result.role_code`,
}

// ResultStore persists evaluation runs and per-arm results.
type ResultStore struct {
	keelStore
	Now func() time.Time
}

var _ contract.EvaluationResultStore = (*ResultStore)(nil)

func (store *ResultStore) now() time.Time {
	if store.Now != nil {
		return store.Now()
	}
	return time.Now()
}

// StartRun inserts a running row and returns its generated id.
func (store *ResultStore) StartRun(ctx context.Context, run domain.EvaluationRun) (int64, error) {
	if run.TenantID <= 0 || !isSHA256Hex(run.ManifestID) {
		return 0, fmt.Errorf("%w: run tenant and manifest id are required", domain.ErrValidation)
	}
	if err := validScope(run.Scope); err != nil {
		return 0, err
	}
	qs, err := store.queries(ctx, "result store", resultQueries)
	if err != nil {
		return 0, err
	}
	startedAt := run.StartedAt
	if startedAt.IsZero() {
		startedAt = store.now()
	}
	runID := qs.GenID()
	if _, err := qs.Query(ctx, qRunInsert, runID, run.TenantID, run.ManifestID, string(run.Scope), runStatusRunning, startedAt.UTC()); err != nil {
		return 0, fmt.Errorf("insert evaluation run: %w", err)
	}
	return runID, nil
}

// FinishRun records the terminal status and totals of a running run.
func (store *ResultStore) FinishRun(ctx context.Context, run domain.EvaluationRun) error {
	if run.TenantID <= 0 || run.RunID <= 0 {
		return fmt.Errorf("%w: run tenant and id are required", domain.ErrValidation)
	}
	switch run.Status {
	case runStatusCompleted, runStatusStoppedEarly, runStatusFailed:
	default:
		return fmt.Errorf("%w: %q is not a terminal run status", domain.ErrValidation, run.Status)
	}
	qs, err := store.queries(ctx, "result store", resultQueries)
	if err != nil {
		return err
	}
	completedAt := run.CompletedAt
	if completedAt.IsZero() {
		completedAt = store.now()
	}
	result, err := qs.Query(ctx, qRunFinish, run.Status, completedAt.UTC(), run.Samples, run.Usage.InputTokens, run.Usage.OutputTokens,
		run.Usage.CostMinorUnits, nullable(run.Usage.Currency), run.TenantID, run.RunID)
	if err != nil {
		return fmt.Errorf("finish evaluation run: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: run %d is not running", domain.ErrConflict, run.RunID)
	}
	return nil
}

// PutResults inserts a batch atomically, resolving the golden set from the run's manifest.
func (store *ResultStore) PutResults(ctx context.Context, tenantID int64, runID int64, results []domain.EvaluationResult) error {
	if tenantID <= 0 || runID <= 0 {
		return fmt.Errorf("%w: tenant and run id are required", domain.ErrValidation)
	}
	if len(results) == 0 {
		return nil
	}
	tx, err := store.begin(ctx, "result store", resultQueries)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	set, err := tx.Query(ctx, qRunGetSet, tenantID, runID)
	if err != nil {
		return fmt.Errorf("resolve run golden set: %w", err)
	}
	if len(set.Rows) == 0 {
		return fmt.Errorf("%w: evaluation run %d", domain.ErrNotFound, runID)
	}
	manifestID, goldenSetID, setVersion := common.AsString(set.Rows[0][0]), common.AsString(set.Rows[0][1]), common.AsInt64(set.Rows[0][2])
	for _, result := range results {
		if result.ManifestID != manifestID {
			return fmt.Errorf("%w: result for %q belongs to manifest %q, run has %q", domain.ErrValidation, result.ExampleID, result.ManifestID, manifestID)
		}
		if result.Role != domain.RoleBaseline && result.Role != domain.RoleCandidate {
			return fmt.Errorf("%w: unknown role %q", domain.ErrValidation, result.Role)
		}
		scores, err := json.Marshal(result.Scores)
		if err != nil {
			return fmt.Errorf("encode scores: %w", err)
		}
		if _, err := tx.Query(ctx, qResultInsert,
			runID, tenantID, goldenSetID, setVersion, result.ExampleID, string(result.Role), string(scores), result.Latency.Milliseconds(),
			result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CostMinorUnits, nullable(result.Usage.Currency),
			result.NeedsHumanReview, nullable(result.Reason),
		); err != nil {
			return fmt.Errorf("insert evaluation result %q/%s: %w", result.ExampleID, result.Role, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit evaluation results: %w", err)
	}
	committed = true
	return nil
}

// ListResults returns every result of a run ordered by example and role.
func (store *ResultStore) ListResults(ctx context.Context, tenantID int64, runID int64) ([]domain.EvaluationResult, error) {
	if tenantID <= 0 || runID <= 0 {
		return nil, fmt.Errorf("%w: tenant and run id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "result store", resultQueries)
	if err != nil {
		return nil, err
	}
	rows, err := qs.Query(ctx, qResultList, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list evaluation results: %w", err)
	}
	results := make([]domain.EvaluationResult, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		if len(row) < 11 {
			return nil, fmt.Errorf("evaluation result row: expected 11 columns, got %d", len(row))
		}
		result := domain.EvaluationResult{
			ManifestID: common.AsString(row[0]), ExampleID: common.AsString(row[1]), Role: domain.EvaluationRole(common.AsString(row[2])),
			Latency:          time.Duration(common.AsInt64(row[4])) * time.Millisecond,
			Usage:            domain.Usage{InputTokens: common.AsInt64(row[5]), OutputTokens: common.AsInt64(row[6]), CostMinorUnits: common.AsInt64(row[7]), Currency: strings.TrimSpace(common.AsString(row[8]))},
			NeedsHumanReview: common.AsBool(row[9]), Reason: common.AsString(row[10]),
		}
		if raw := common.AsString(row[3]); raw != "" && raw != "null" {
			if err := json.Unmarshal([]byte(raw), &result.Scores); err != nil {
				return nil, fmt.Errorf("decode scores for %q: %w", result.ExampleID, err)
			}
		}
		results = append(results, result)
	}
	return results, nil
}
