package evaluation

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"
	keelport "github.com/nauticana/keel/port"
	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

const (
	qBaselineExamples = "scout_evaluation_baseline_examples"
	qBaselineCount    = "scout_evaluation_baseline_count"
	qBaselineInsert   = "scout_evaluation_baseline_insert"
	qBaselineOutput   = "scout_evaluation_baseline_output"
	qBaselineList     = "scout_evaluation_baseline_list"
	qBaselineNamed    = "scout_evaluation_baseline_named"
)

// Defaults of EvaluationBaselineStore: the largest recorded output and the baselines one golden set version holds.
const (
	DefaultBaselineMaxOutputBytes = 32768
	DefaultMaxBaselines           = 20
)

var baselineQueries = map[string]string{
	qBaselineExamples: `
SELECT example_id FROM golden_example
 WHERE partner_id = ? AND golden_set_id = ? AND set_version = ? AND (scope_code = ? OR ? = ` + sqlScopeGate + `)`,
	qBaselineCount: `
SELECT COUNT(*) FROM evaluation_baseline WHERE partner_id = ? AND golden_set_id = ? AND set_version = ?`,
	qBaselineInsert: `
INSERT INTO evaluation_baseline (id, partner_id, golden_set_id, set_version, name, scope_code, example_count, content_digest,
                                 recorded_by_kind, recorded_by_id, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, golden_set_id, set_version, name) DO NOTHING
RETURNING id`,
	qBaselineOutput: `
INSERT INTO evaluation_baseline_output (baseline_id, partner_id, golden_set_id, set_version, example_id, output_uri, output_digest)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
	qBaselineNamed: `
SELECT id FROM evaluation_baseline WHERE partner_id = ? AND golden_set_id = ? AND set_version = ? AND name = ?`,
	qBaselineList: `
SELECT id, name, scope_code, example_count, content_digest, recorded_by_kind, recorded_by_id, recorded_at
  FROM evaluation_baseline
 WHERE partner_id = ? AND golden_set_id = ? AND set_version = ?
 ORDER BY id DESC
 LIMIT ?`,
}

// EvaluationBaselineStore keeps recorded human baselines in evaluation_baseline and their
// outputs in object storage under their content digest, one evaluation_baseline_output row each.
type EvaluationBaselineStore struct {
	keelStore
	// Storage is bound to the bucket baseline outputs are written to.
	Storage storage.ObjectStorage
	// Scheme prefixes output URIs; empty means "object".
	Scheme string
	// MaxOutputBytes bounds one output; zero takes DefaultBaselineMaxOutputBytes.
	MaxOutputBytes int
	// MaxBaselines bounds the baselines of one golden set version; zero takes DefaultMaxBaselines.
	MaxBaselines int
	Now          func() time.Time
}

var _ contract.EvaluationBaselineStore = (*EvaluationBaselineStore)(nil)

func (store *EvaluationBaselineStore) now() time.Time {
	return clk.Now(store.Now).UTC()
}

// Record stores the outputs as a new baseline after checking they answer exactly the
// examples of the version visible in scope; a name the version already holds is a conflict.
func (store *EvaluationBaselineStore) Record(ctx context.Context, scope domain.GoldenScope, baseline domain.EvaluationBaseline, outputs []domain.BaselineOutput) (domain.EvaluationBaseline, error) {
	if err := store.validRecord(scope, &baseline, outputs); err != nil {
		return domain.EvaluationBaseline{}, err
	}
	qs, err := store.queries(ctx, "evaluation baseline store", baselineQueries)
	if err != nil {
		return domain.EvaluationBaseline{}, err
	}
	visible, err := qs.Query(ctx, qBaselineExamples, baseline.TenantID, baseline.GoldenSetID, baseline.SetVersion, string(scope), string(scope))
	if err != nil {
		return domain.EvaluationBaseline{}, fmt.Errorf("list golden examples: %w", err)
	}
	expected := make(map[string]bool, len(visible.Rows))
	for _, row := range visible.Rows {
		expected[common.AsString(row[0])] = true
	}
	if len(expected) == 0 {
		return domain.EvaluationBaseline{}, fmt.Errorf("%w: golden set %q version %d has no examples in the %s scope", domain.ErrNotFound, baseline.GoldenSetID, baseline.SetVersion, scope)
	}
	for _, output := range outputs {
		if !expected[output.CaseID] {
			return domain.EvaluationBaseline{}, fmt.Errorf("%w: case %q is not in golden set version %d", domain.ErrValidation, output.CaseID, baseline.SetVersion)
		}
	}
	if len(outputs) != len(expected) {
		return domain.EvaluationBaseline{}, fmt.Errorf("%w: a baseline answers all %d cases of the version", domain.ErrValidation, len(expected))
	}
	// Checked before any upload; the count is approximate under concurrent records, and the name's
	// unique index refuses a duplicate inside the insert.
	if err = store.admissible(ctx, qs, baseline); err != nil {
		return domain.EvaluationBaseline{}, err
	}
	ordered := slices.Clone(outputs)
	slices.SortFunc(ordered, func(left, right domain.BaselineOutput) int { return strings.Compare(left.CaseID, right.CaseID) })
	refs := make([]domain.ObjectRef, len(ordered))
	var content strings.Builder
	prefix := fmt.Sprintf("evaluation/baseline/%d/%s", baseline.TenantID, baseline.GoldenSetID)
	for i, output := range ordered {
		if refs[i], err = putContent(ctx, store.Storage, store.Scheme, prefix, output.Output); err != nil {
			return domain.EvaluationBaseline{}, fmt.Errorf("upload baseline output: %w", err)
		}
		content.WriteString(output.CaseID + "\x00" + refs[i].Digest + "\n")
	}
	baseline.Scope, baseline.ExampleCount, baseline.ContentDigest, baseline.RecordedAt = scope, len(ordered), sha256Hex([]byte(content.String())), store.now()
	tx, err := store.begin(ctx, "evaluation baseline store", baselineQueries)
	if err != nil {
		return domain.EvaluationBaseline{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	baseline.BaselineID = tx.GenID()
	kind, id := principalColumns(baseline.RecordedBy)
	inserted, err := tx.Query(ctx, qBaselineInsert, baseline.BaselineID, baseline.TenantID, baseline.GoldenSetID, baseline.SetVersion, baseline.Name,
		string(scope), baseline.ExampleCount, baseline.ContentDigest, kind, id, baseline.RecordedAt)
	if err != nil {
		return domain.EvaluationBaseline{}, fmt.Errorf("insert baseline: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return domain.EvaluationBaseline{}, fmt.Errorf("%w: golden set version %d already has a baseline named %q", domain.ErrConflict, baseline.SetVersion, baseline.Name)
	}
	for i, output := range ordered {
		if _, err = tx.Query(ctx, qBaselineOutput, baseline.BaselineID, baseline.TenantID, baseline.GoldenSetID, baseline.SetVersion,
			output.CaseID, refs[i].URI, refs[i].Digest); err != nil {
			return domain.EvaluationBaseline{}, fmt.Errorf("insert baseline output %q: %w", output.CaseID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.EvaluationBaseline{}, fmt.Errorf("commit baseline: %w", err)
	}
	committed = true
	return baseline, nil
}

// admissible refuses a version already holding MaxBaselines baselines or one named like baseline.
func (store *EvaluationBaselineStore) admissible(ctx context.Context, qs keelport.QueryService, baseline domain.EvaluationBaseline) error {
	count, err := qs.Query(ctx, qBaselineCount, baseline.TenantID, baseline.GoldenSetID, baseline.SetVersion)
	if err != nil {
		return fmt.Errorf("count baselines: %w", err)
	}
	if limit := orDefault(store.MaxBaselines, DefaultMaxBaselines); len(count.Rows) > 0 && common.AsInt64(count.Rows[0][0]) >= int64(limit) {
		return fmt.Errorf("%w: a golden set version holds at most %d baselines", domain.ErrValidation, limit)
	}
	named, err := qs.Query(ctx, qBaselineNamed, baseline.TenantID, baseline.GoldenSetID, baseline.SetVersion, baseline.Name)
	if err != nil {
		return fmt.Errorf("look up baseline name: %w", err)
	}
	if len(named.Rows) > 0 {
		return fmt.Errorf("%w: golden set version %d already has a baseline named %q", domain.ErrConflict, baseline.SetVersion, baseline.Name)
	}
	return nil
}

func (store *EvaluationBaselineStore) validRecord(scope domain.GoldenScope, baseline *domain.EvaluationBaseline, outputs []domain.BaselineOutput) error {
	if err := validScope(scope); err != nil {
		return err
	}
	if store.MaxOutputBytes < 0 || store.MaxBaselines < 0 {
		return fmt.Errorf("%w: baseline store limits cannot be negative", domain.ErrValidation)
	}
	if baseline.TenantID <= 0 || baseline.SetVersion <= 0 || baseline.RecordedBy.Kind == "" || strings.TrimSpace(baseline.RecordedBy.ID) == "" {
		return fmt.Errorf("%w: tenant, a positive set version, and the recorder are required", domain.ErrValidation)
	}
	if err := validIdentifier("golden_set_id", baseline.GoldenSetID, 80); err != nil {
		return err
	}
	baseline.Name = strings.TrimSpace(baseline.Name)
	if baseline.Name == "" || utf8.RuneCountInString(baseline.Name) > 200 {
		return fmt.Errorf("%w: a baseline name has 1 to 200 characters", domain.ErrValidation)
	}
	if len(outputs) == 0 {
		return fmt.Errorf("%w: a baseline records at least one output", domain.ErrValidation)
	}
	maxBytes := orDefault(store.MaxOutputBytes, DefaultBaselineMaxOutputBytes)
	seen := make(map[string]bool, len(outputs))
	for _, output := range outputs {
		if err := validIdentifier("case_id", output.CaseID, 120); err != nil {
			return err
		}
		if seen[output.CaseID] {
			return fmt.Errorf("%w: case %q is answered twice", domain.ErrValidation, output.CaseID)
		}
		seen[output.CaseID] = true
		if len(output.Output) == 0 || len(output.Output) > maxBytes || !utf8.Valid(output.Output) {
			return fmt.Errorf("%w: the output of case %q must be 1 to %d bytes of UTF-8 text", domain.ErrValidation, output.CaseID, maxBytes)
		}
	}
	return nil
}

// List returns the set version's baselines, newest first.
func (store *EvaluationBaselineStore) List(ctx context.Context, tenantID int64, goldenSetID string, setVersion int64) ([]domain.EvaluationBaseline, error) {
	if tenantID <= 0 || setVersion <= 0 {
		return nil, fmt.Errorf("%w: tenant and a positive set version are required", domain.ErrValidation)
	}
	if err := validIdentifier("golden_set_id", goldenSetID, 80); err != nil {
		return nil, err
	}
	qs, err := store.queries(ctx, "evaluation baseline store", baselineQueries)
	if err != nil {
		return nil, err
	}
	rows, err := qs.Query(ctx, qBaselineList, tenantID, goldenSetID, setVersion, orDefault(store.MaxBaselines, DefaultMaxBaselines))
	if err != nil {
		return nil, fmt.Errorf("list baselines: %w", err)
	}
	baselines := make([]domain.EvaluationBaseline, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		if len(row) < 8 {
			return nil, fmt.Errorf("baseline row: expected 8 columns, got %d", len(row))
		}
		baselines = append(baselines, domain.EvaluationBaseline{
			BaselineID: common.AsInt64(row[0]), TenantID: tenantID, GoldenSetID: goldenSetID, SetVersion: setVersion,
			Name: common.AsString(row[1]), Scope: domain.GoldenScope(common.AsString(row[2])), ExampleCount: int(common.AsInt64(row[3])),
			ContentDigest: strings.TrimSpace(common.AsString(row[4])),
			RecordedBy:    domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[5])), ID: common.AsString(row[6])},
			RecordedAt:    common.AsTime(row[7]),
		})
	}
	return baselines, nil
}
