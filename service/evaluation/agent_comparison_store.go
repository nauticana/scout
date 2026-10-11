package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"

	"github.com/nauticana/scout/domain"
)

// RecordComparison renews the lease and stores one example's verdict and both arms, adding
// all their spend to the run's totals in one transaction; replaying a recorded example is a
// no-op, and spend priced in another currency than the run is domain.ErrValidation.
func (store *AgentEvaluationStore) RecordComparison(ctx context.Context, lease domain.AgentEvaluationLease, comparison domain.CaseComparison) error {
	if err := validLease(lease); err != nil {
		return err
	}
	if err := validComparison(comparison); err != nil {
		return err
	}
	total, err := comparisonTotal(comparison)
	if err != nil {
		return err
	}
	scoresA, err := json.Marshal(comparison.A.Scores)
	if err != nil {
		return fmt.Errorf("encode scores: %w", err)
	}
	scoresB, err := json.Marshal(comparison.B.Scores)
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
	judge := comparison.EvaluatorUsage
	inserted, err := tx.Query(ctx, qAgentEvalPutComparison, lease.RunID, lease.TenantID, goldenSetID, setVersion, comparison.ExampleID,
		nullable(string(comparison.Verdict)), comparison.Confidence, nullable(comparison.Evaluator.Kind), nullable(truncate(comparison.Evaluator.Version, 400)),
		nullable(comparison.InputDigest), nullable(truncate(comparison.Rationale, pairRationaleMaxRunes)),
		judge.InputTokens, judge.OutputTokens, judge.CostMinorUnits, pricedCurrency(judge), nullable(truncate(comparison.Reason, 400)))
	if err != nil {
		return fmt.Errorf("insert evaluation comparison %q: %w", comparison.ExampleID, err)
	}
	if len(inserted.Rows) > 0 {
		for _, arm := range []struct {
			role   domain.EvaluationRole
			result domain.EvaluationResult
			scores []byte
		}{{domain.RoleBaseline, comparison.A, scoresA}, {domain.RoleCandidate, comparison.B, scoresB}} {
			usage := arm.result.Usage
			if _, err = tx.Query(ctx, qAgentEvalPutArm, lease.RunID, lease.TenantID, goldenSetID, setVersion, comparison.ExampleID, string(arm.role),
				string(arm.scores), arm.result.Latency.Milliseconds(), usage.InputTokens, usage.OutputTokens, usage.CostMinorUnits, pricedCurrency(usage),
				nullable(arm.result.Output.URI), nullable(arm.result.Output.Digest)); err != nil {
				return fmt.Errorf("insert %s arm of %q: %w", arm.role, comparison.ExampleID, err)
			}
		}
		currency := pricedCurrency(total)
		totals, err := tx.Query(ctx, qAgentEvalAddTotals, total.InputTokens, total.OutputTokens, total.CostMinorUnits,
			currency, lease.TenantID, lease.RunID, currency, currency)
		if err != nil {
			return fmt.Errorf("add evaluation totals: %w", err)
		}
		if len(totals.Rows) == 0 {
			return fmt.Errorf("%w: comparison %q is priced in %s, unlike the run", domain.ErrValidation, comparison.ExampleID, total.Currency)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit evaluation comparison: %w", err)
	}
	committed = true
	return nil
}

// pricedCurrency is the currency a priced amount is stored with; a free one carries none.
func pricedCurrency(usage domain.Usage) any {
	if usage.CostMinorUnits > 0 {
		return usage.Currency
	}
	return nil
}

func validComparison(comparison domain.CaseComparison) error {
	if strings.TrimSpace(comparison.ExampleID) == "" || comparison.A.Latency < 0 || comparison.B.Latency < 0 {
		return fmt.Errorf("%w: a comparison needs an example id and non-negative latencies", domain.ErrValidation)
	}
	switch comparison.Verdict {
	case domain.ComparisonPrefersA, domain.ComparisonPrefersB, domain.ComparisonTie:
		if comparison.Reason != "" {
			return fmt.Errorf("%w: a decided comparison carries no failure reason", domain.ErrValidation)
		}
	case "":
		if strings.TrimSpace(comparison.Reason) == "" {
			return fmt.Errorf("%w: an undecided comparison says why", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unknown comparison verdict %q", domain.ErrValidation, comparison.Verdict)
	}
	if math.IsNaN(comparison.Confidence) || comparison.Confidence < 0 || comparison.Confidence > 1 {
		return fmt.Errorf("%w: comparison confidence must lie in 0..1", domain.ErrValidation)
	}
	if (comparison.Evaluator.Kind == "") != (comparison.Evaluator.Version == "") || comparison.InputDigest != "" && !isSHA256Hex(comparison.InputDigest) {
		return fmt.Errorf("%w: a judged comparison names its evaluator kind and version and a SHA-256 input digest", domain.ErrValidation)
	}
	for _, output := range []domain.ObjectRef{comparison.A.Output, comparison.B.Output} {
		if (output.URI == "") != (output.Digest == "") || output.Digest != "" && !isSHA256Hex(output.Digest) || len(output.URI) > 500 {
			return fmt.Errorf("%w: a stored output has a URI of at most 500 bytes and a SHA-256 digest", domain.ErrValidation)
		}
	}
	return nil
}

// comparisonTotal is both arms' and the judge's spend together, as the run's totals count
// it: each must be non-negative, and priced parts need one ISO currency.
func comparisonTotal(comparison domain.CaseComparison) (domain.Usage, error) {
	var total domain.Usage
	for _, usage := range []domain.Usage{comparison.A.Usage, comparison.B.Usage, comparison.EvaluatorUsage} {
		if usage.CostMinorUnits < 0 || usage.InputTokens < 0 || usage.OutputTokens < 0 {
			return domain.Usage{}, fmt.Errorf("%w: a comparison needs non-negative usage", domain.ErrValidation)
		}
		if usage.CostMinorUnits > 0 {
			if len(usage.Currency) != 3 {
				return domain.Usage{}, fmt.Errorf("%w: priced spend needs an ISO currency", domain.ErrValidation)
			}
			if total.Currency != "" && total.Currency != usage.Currency {
				return domain.Usage{}, fmt.Errorf("%w: comparison spend mixes %s and %s", domain.ErrValidation, total.Currency, usage.Currency)
			}
			total.Currency = usage.Currency
		}
		if total.InputTokens > math.MaxInt64-usage.InputTokens || total.OutputTokens > math.MaxInt64-usage.OutputTokens ||
			total.CostMinorUnits > math.MaxInt64-usage.CostMinorUnits {
			return domain.Usage{}, fmt.Errorf("%w: comparison usage overflows", domain.ErrValidation)
		}
		total.InputTokens += usage.InputTokens
		total.OutputTokens += usage.OutputTokens
		total.CostMinorUnits += usage.CostMinorUnits
	}
	return total, nil
}

// Comparisons pages a comparison run's examples visible in scope, in descending example id.
func (store *AgentEvaluationStore) Comparisons(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64, before string, limit int) ([]domain.AgentCaseComparison, error) {
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
	rows, err := qs.Query(ctx, qAgentEvalComparisons, tenantID, runID, string(scope), string(scope), before, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list evaluation comparisons: %w", err)
	}
	comparisons := make([]domain.AgentCaseComparison, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		if len(row) < 30 {
			return nil, fmt.Errorf("evaluation comparison row: expected 30 columns, got %d", len(row))
		}
		comparison := domain.AgentCaseComparison{
			ExampleID: common.AsString(row[0]), Verdict: domain.ComparisonVerdict(common.AsString(row[1])), Confidence: asConfidence(row[2]),
			Evaluator: domain.EvaluatorVersion{Kind: common.AsString(row[3]), Version: common.AsString(row[4])},
			Rationale: common.AsString(row[5]), Reason: common.AsString(row[6]),
			EvaluatorUsage: domain.Usage{InputTokens: common.AsInt64(row[7]), OutputTokens: common.AsInt64(row[8]),
				CostMinorUnits: common.AsInt64(row[9]), Currency: strings.TrimSpace(common.AsString(row[10]))},
			Exponent: int(common.AsInt64(row[11])),
		}
		if comparison.A, err = scanArm(comparison.ExampleID, row[12:21]); err != nil {
			return nil, err
		}
		if comparison.B, err = scanArm(comparison.ExampleID, row[21:30]); err != nil {
			return nil, err
		}
		comparisons = append(comparisons, comparison)
	}
	return comparisons, nil
}

// asConfidence reads a stored confidence, numeric or textual; anything else is no confidence.
func asConfidence(value any) float64 {
	if number, ok := common.AsFloat64OK(value); ok {
		return number
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(common.AsString(value)), 64)
	if err != nil {
		return 0
	}
	return number
}

// scanArm reads scores, latency, usage, currency, exponent, and stored output of one arm.
func scanArm(exampleID string, row []any) (domain.AgentCaseResult, error) {
	arm := domain.AgentCaseResult{
		ExampleID: exampleID, Latency: time.Duration(common.AsInt64(row[1])) * time.Millisecond,
		Usage: domain.Usage{InputTokens: common.AsInt64(row[2]), OutputTokens: common.AsInt64(row[3]),
			CostMinorUnits: common.AsInt64(row[4]), Currency: strings.TrimSpace(common.AsString(row[5]))},
		Exponent: int(common.AsInt64(row[6])),
		Output:   domain.ObjectRef{URI: common.AsString(row[7]), Digest: strings.TrimSpace(common.AsString(row[8]))},
	}
	if raw := common.AsString(row[0]); raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &arm.Scores); err != nil {
			return domain.AgentCaseResult{}, fmt.Errorf("decode scores for %q: %w", exampleID, err)
		}
	}
	return arm, nil
}

// Tally counts a comparison run's examples visible in scope by verdict.
func (store *AgentEvaluationStore) Tally(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64) (domain.ComparisonTally, error) {
	if err := validScope(scope); err != nil {
		return domain.ComparisonTally{}, err
	}
	if tenantID <= 0 || runID <= 0 {
		return domain.ComparisonTally{}, fmt.Errorf("%w: tenant and run id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "agent evaluation store", agentEvaluationQueries)
	if err != nil {
		return domain.ComparisonTally{}, err
	}
	rows, err := qs.Query(ctx, qAgentEvalTally, tenantID, runID, string(scope), string(scope))
	if err != nil {
		return domain.ComparisonTally{}, fmt.Errorf("count evaluation comparisons: %w", err)
	}
	var tally domain.ComparisonTally
	for _, row := range rows.Rows {
		if len(row) < 2 {
			return domain.ComparisonTally{}, fmt.Errorf("comparison tally row: expected 2 columns, got %d", len(row))
		}
		count := int(common.AsInt64(row[1]))
		switch domain.ComparisonVerdict(common.AsString(row[0])) {
		case domain.ComparisonPrefersA:
			tally.A += count
		case domain.ComparisonPrefersB:
			tally.B += count
		case domain.ComparisonTie:
			tally.Ties += count
		default:
			tally.Failed += count
		}
	}
	return tally, nil
}
