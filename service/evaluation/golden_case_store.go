package evaluation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"
	keelport "github.com/nauticana/keel/port"
	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qCaseAgentSet      = "scout_evaluation_case_agent_set"
	qCaseSetRevision   = "scout_evaluation_case_set_revision"
	qCaseList          = "scout_evaluation_case_list"
	qCaseHiddenCount   = "scout_evaluation_case_hidden_count"
	qCaseVersions      = "scout_evaluation_case_versions"
	qCaseAgentExists   = "scout_evaluation_case_agent_exists"
	qCaseSetOwner      = "scout_evaluation_case_set_owner"
	qCaseSetCreate     = "scout_evaluation_case_set_create"
	qCaseAssociate     = "scout_evaluation_case_associate"
	qCaseBumpRevision  = "scout_evaluation_case_bump_revision"
	qCaseRiskTier      = "scout_evaluation_case_risk_tier"
	qCaseCountOthers   = "scout_evaluation_case_count_others"
	qCaseUpsert        = "scout_evaluation_case_upsert"
	qCaseDelete        = "scout_evaluation_case_delete"
	qCaseNextVersion   = "scout_evaluation_case_next_version"
	qCaseVersionInsert = "scout_evaluation_case_version_insert"
	qCaseExampleInsert = "scout_evaluation_case_example_insert"
)

// Classification every frozen Studio case carries in golden_example.
const (
	GoldenCaseProvenance = "studio"
	GoldenCaseConsent    = "authored"
	GoldenCaseRetention  = "golden_set"
)

const caseColumns = `c.case_id, c.scope_code, c.name, c.input_text, c.language_code, c.expected_behavior, c.rubric_ref, c.risk_tier_code,
       c.principal_kind, c.principal_id, c.modified_at, c.review_item_id, r.agent_id`

// Reads take the caller scope twice: dev callers see only dev rows, gate callers every row.
var caseQueries = map[string]string{
	qCaseAgentSet: `
SELECT a.golden_set_id, s.working_revision
  FROM agent_golden_set a
  JOIN golden_set s ON s.partner_id = a.partner_id AND s.golden_set_id = a.golden_set_id
 WHERE a.partner_id = ? AND a.agent_id = ?`,
	qCaseSetRevision: `
SELECT working_revision FROM golden_set WHERE partner_id = ? AND golden_set_id = ?`,
	qCaseList: `
SELECT ` + caseColumns + `
  FROM golden_case c
  LEFT JOIN human_review_item r ON r.partner_id = c.partner_id AND r.id = c.review_item_id
 WHERE c.partner_id = ? AND c.golden_set_id = ? AND (c.scope_code = ? OR ? = ` + sqlScopeGate + `)
 ORDER BY c.case_id`,
	qCaseHiddenCount: `
SELECT COUNT(*) FROM golden_case WHERE partner_id = ? AND golden_set_id = ? AND scope_code = ` + sqlScopeGate + ``,
	qCaseVersions: `
SELECT set_version, dataset_revision, example_count, frozen_at
  FROM golden_set_version
 WHERE partner_id = ? AND golden_set_id = ?
 ORDER BY set_version DESC
 LIMIT ?`,
	qCaseAgentExists: `
SELECT agent_id FROM agent_profile WHERE partner_id = ? AND agent_id = ? FOR SHARE`,
	qCaseSetOwner: `
SELECT owner_agent_id FROM golden_set WHERE partner_id = ? AND golden_set_id = ?`,
	qCaseSetCreate: `
INSERT INTO golden_set (partner_id, golden_set_id, name, working_revision, owner_agent_id)
VALUES (?, ?, ?, 0, ?)
ON CONFLICT (partner_id, golden_set_id) DO NOTHING
RETURNING golden_set_id`,
	qCaseAssociate: `
INSERT INTO agent_golden_set (partner_id, agent_id, golden_set_id, principal_kind, principal_id, associated_at)
SELECT partner_id, agent_id, ?, ?, ?, ?
  FROM agent_profile
 WHERE partner_id = ? AND agent_id = ?
ON CONFLICT (partner_id, agent_id) DO UPDATE
   SET golden_set_id = EXCLUDED.golden_set_id, principal_kind = EXCLUDED.principal_kind,
       principal_id = EXCLUDED.principal_id, associated_at = EXCLUDED.associated_at
RETURNING agent_id`,
	qCaseBumpRevision: `
UPDATE golden_set
   SET working_revision = working_revision + 1
 WHERE partner_id = ? AND golden_set_id = ? AND working_revision = ?
RETURNING working_revision`,
	qCaseRiskTier: `
SELECT code FROM approval_risk_tier WHERE code = ?`,
	qCaseCountOthers: `
SELECT COUNT(*) FROM golden_case WHERE partner_id = ? AND golden_set_id = ? AND case_id <> ?`,
	qCaseUpsert: `
INSERT INTO golden_case (partner_id, golden_set_id, case_id, scope_code, name, input_text, language_code, expected_behavior,
                         rubric_ref, risk_tier_code, principal_kind, principal_id, modified_at, review_item_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, golden_set_id, case_id) DO UPDATE
   SET scope_code = EXCLUDED.scope_code, name = EXCLUDED.name, input_text = EXCLUDED.input_text,
       language_code = EXCLUDED.language_code, expected_behavior = EXCLUDED.expected_behavior,
       rubric_ref = EXCLUDED.rubric_ref, risk_tier_code = EXCLUDED.risk_tier_code,
       principal_kind = EXCLUDED.principal_kind, principal_id = EXCLUDED.principal_id, modified_at = EXCLUDED.modified_at,
       review_item_id = COALESCE(golden_case.review_item_id, EXCLUDED.review_item_id)
 WHERE golden_case.scope_code = EXCLUDED.scope_code OR ? = ` + sqlScopeGate + `
RETURNING case_id`,
	qCaseDelete: `
DELETE FROM golden_case
 WHERE partner_id = ? AND golden_set_id = ? AND case_id = ? AND (scope_code = ? OR ? = ` + sqlScopeGate + `)
RETURNING case_id`,
	qCaseNextVersion: `
SELECT COALESCE(MAX(set_version), 0) + 1 FROM golden_set_version WHERE partner_id = ? AND golden_set_id = ?`,
	qCaseVersionInsert: goldenQueries[qGoldenVersionInsert],
	qCaseExampleInsert: goldenQueries[qGoldenExampleInsert],
}

var goldenIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// GoldenCaseStore keeps the golden_case working copy and the agent_golden_set
// association. Freeze uploads each case input to object storage under its content
// digest and writes the set version and its golden_example rows in one transaction.
type GoldenCaseStore struct {
	keelStore
	// Storage is bound to the bucket frozen case inputs are written to.
	Storage storage.ObjectStorage
	// Scheme prefixes payload URIs; empty means "object".
	Scheme string
	// MaxInputBytes bounds one case's input and expected behavior; zero means 32 KiB.
	MaxInputBytes int
	// MaxCases bounds the cases of one set; zero means 500.
	MaxCases int
	// VersionsShown bounds the frozen versions Cases returns, newest first; zero means 20.
	VersionsShown int
	Now           func() time.Time
}

var _ contract.GoldenCaseStore = (*GoldenCaseStore)(nil)

func (store *GoldenCaseStore) now() time.Time {
	if store.Now != nil {
		return store.Now().UTC()
	}
	return time.Now().UTC()
}

func orDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func validIdentifier(field, value string, maxLength int) error {
	if len(value) > maxLength || !goldenIdentifier.MatchString(value) {
		return fmt.Errorf("%w: %s must be 1 to %d letters, digits, dots, dashes, or underscores", domain.ErrValidation, field, maxLength)
	}
	return nil
}

func (store *GoldenCaseStore) validateLimits() error {
	if store.MaxInputBytes < 0 || store.MaxCases < 0 || store.VersionsShown < 0 {
		return fmt.Errorf("%w: golden case store limits cannot be negative", domain.ErrValidation)
	}
	return nil
}

// SetOwner returns the agent owning the golden set, empty for a set no agent owns.
func (store *GoldenCaseStore) SetOwner(ctx context.Context, tenantID int64, goldenSetID string) (string, error) {
	if tenantID <= 0 {
		return "", fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if err := validIdentifier("golden_set_id", goldenSetID, 80); err != nil {
		return "", err
	}
	qs, err := store.queries(ctx, "golden case store", caseQueries)
	if err != nil {
		return "", err
	}
	owner, err := qs.Query(ctx, qCaseSetOwner, tenantID, goldenSetID)
	if err != nil {
		return "", fmt.Errorf("load golden set owner: %w", err)
	}
	if len(owner.Rows) == 0 {
		return "", fmt.Errorf("%w: golden set %q", domain.ErrNotFound, goldenSetID)
	}
	return common.AsString(owner.Rows[0][0]), nil
}

// Associate points the agent at the golden set, creating it under its name and owner when it is new.
func (store *GoldenCaseStore) Associate(ctx context.Context, association domain.GoldenSetAssociation) error {
	tenantID, agentID, goldenSetID := association.TenantID, association.AgentID, association.GoldenSetID
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("%w: tenant and agent are required", domain.ErrValidation)
	}
	if err := validIdentifier("golden_set_id", goldenSetID, 80); err != nil {
		return err
	}
	name := strings.TrimSpace(association.Name)
	if name == "" {
		name = goldenSetID
	}
	if utf8.RuneCountInString(name) > 200 {
		return fmt.Errorf("%w: golden set name exceeds 200 characters", domain.ErrValidation)
	}
	tx, err := store.begin(ctx, "golden case store", caseQueries)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	agent, err := tx.Query(ctx, qCaseAgentExists, tenantID, agentID)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	if len(agent.Rows) == 0 {
		return fmt.Errorf("%w: agent %q", domain.ErrNotFound, agentID)
	}
	created, err := tx.Query(ctx, qCaseSetCreate, tenantID, goldenSetID, name, nullable(association.Owner))
	if err != nil {
		return fmt.Errorf("create golden set: %w", err)
	}
	if len(created.Rows) == 0 {
		owner, err := tx.Query(ctx, qCaseSetOwner, tenantID, goldenSetID)
		if err != nil {
			return fmt.Errorf("load golden set owner: %w", err)
		}
		if len(owner.Rows) == 0 || common.AsString(owner.Rows[0][0]) != association.Owner {
			return fmt.Errorf("%w: golden set %q changed owner", domain.ErrConflict, goldenSetID)
		}
	}
	kind, id := principalColumns(association.By)
	result, err := tx.Query(ctx, qCaseAssociate, goldenSetID, kind, id, store.now(), tenantID, agentID)
	if err != nil {
		return fmt.Errorf("associate golden set: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: agent %q", domain.ErrNotFound, agentID)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit golden set association: %w", err)
	}
	committed = true
	return nil
}

// AgentSet returns the agent's golden set and its working revision.
func (store *GoldenCaseStore) AgentSet(ctx context.Context, tenantID int64, agentID string) (string, int64, error) {
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" {
		return "", 0, fmt.Errorf("%w: tenant and agent are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "golden case store", caseQueries)
	if err != nil {
		return "", 0, err
	}
	association, err := qs.Query(ctx, qCaseAgentSet, tenantID, agentID)
	if err != nil {
		return "", 0, fmt.Errorf("load agent golden set: %w", err)
	}
	if len(association.Rows) == 0 {
		return "", 0, fmt.Errorf("%w: agent %q has no golden set", domain.ErrNotFound, agentID)
	}
	return common.AsString(association.Rows[0][0]), common.AsInt64(association.Rows[0][1]), nil
}

// Cases returns the agent's working copy visible in scope, the withheld gate count, and recent versions.
func (store *GoldenCaseStore) Cases(ctx context.Context, scope domain.GoldenScope, tenantID int64, agentID string) (domain.GoldenCaseSet, error) {
	if err := validScope(scope); err != nil {
		return domain.GoldenCaseSet{}, err
	}
	if err := store.validateLimits(); err != nil {
		return domain.GoldenCaseSet{}, err
	}
	set := domain.GoldenCaseSet{AgentID: agentID}
	var err error
	set.GoldenSetID, set.Revision, err = store.AgentSet(ctx, tenantID, agentID)
	if errors.Is(err, domain.ErrNotFound) {
		return set, nil
	}
	if err != nil {
		return domain.GoldenCaseSet{}, err
	}
	qs, err := store.queries(ctx, "golden case store", caseQueries)
	if err != nil {
		return domain.GoldenCaseSet{}, err
	}
	if set.Cases, err = store.list(ctx, qs, scope, tenantID, set.GoldenSetID); err != nil {
		return domain.GoldenCaseSet{}, err
	}
	if scope != domain.GoldenScopeGate {
		hidden, err := qs.Query(ctx, qCaseHiddenCount, tenantID, set.GoldenSetID)
		if err != nil {
			return domain.GoldenCaseSet{}, fmt.Errorf("count gate cases: %w", err)
		}
		if len(hidden.Rows) > 0 {
			set.HiddenCases = int(common.AsInt64(hidden.Rows[0][0]))
		}
	}
	versions, err := qs.Query(ctx, qCaseVersions, tenantID, set.GoldenSetID, orDefault(store.VersionsShown, 20))
	if err != nil {
		return domain.GoldenCaseSet{}, fmt.Errorf("list golden set versions: %w", err)
	}
	for _, row := range versions.Rows {
		set.Versions = append(set.Versions, domain.GoldenSetVersion{
			TenantID: tenantID, GoldenSetID: set.GoldenSetID, SetVersion: common.AsInt64(row[0]),
			DatasetRevision: common.AsString(row[1]), ExampleCount: int(common.AsInt64(row[2])), FrozenAt: common.AsTime(row[3]),
		})
	}
	return set, nil
}

func (store *GoldenCaseStore) list(ctx context.Context, qs keelport.QueryService, scope domain.GoldenScope, tenantID int64, goldenSetID string) ([]domain.GoldenCase, error) {
	result, err := qs.Query(ctx, qCaseList, tenantID, goldenSetID, string(scope), string(scope))
	if err != nil {
		return nil, fmt.Errorf("list golden cases: %w", err)
	}
	cases := make([]domain.GoldenCase, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 13 {
			return nil, fmt.Errorf("golden case row: expected 13 columns, got %d", len(row))
		}
		hidden := common.AsString(row[1]) == string(domain.GoldenScopeGate)
		if hidden && scope != domain.GoldenScopeGate {
			return nil, fmt.Errorf("%w: gate case returned in dev scope", domain.ErrForbidden)
		}
		goldenCase := domain.GoldenCase{
			TenantID: tenantID, GoldenSetID: goldenSetID, CaseID: common.AsString(row[0]), Hidden: hidden,
			Name: common.AsString(row[2]), Input: []byte(common.AsString(row[3])), Language: common.AsString(row[4]),
			RubricRef: common.AsString(row[6]), RiskTier: common.AsString(row[7]),
			ModifiedBy: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[8])), ID: common.AsString(row[9])},
			ModifiedAt: common.AsTime(row[10]), ReviewItemID: common.AsInt64(row[11]), ReviewAgentID: common.AsString(row[12]),
		}
		if expected := common.AsString(row[5]); expected != "" {
			goldenCase.ExpectedBehavior = []byte(expected)
		}
		cases = append(cases, goldenCase)
	}
	return cases, nil
}

func (store *GoldenCaseStore) validCase(scope domain.GoldenScope, goldenCase domain.GoldenCase) (domain.GoldenCase, error) {
	if err := validScope(scope); err != nil {
		return goldenCase, err
	}
	if goldenCase.TenantID <= 0 {
		return goldenCase, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if err := validIdentifier("golden_set_id", goldenCase.GoldenSetID, 80); err != nil {
		return goldenCase, err
	}
	if err := validIdentifier("case_id", goldenCase.CaseID, 120); err != nil {
		return goldenCase, err
	}
	if goldenCase.Hidden && scope != domain.GoldenScopeGate {
		return goldenCase, fmt.Errorf("%w: hidden cases require the gate scope", domain.ErrForbidden)
	}
	maxBytes := orDefault(store.MaxInputBytes, 32<<10)
	goldenCase.Name = strings.TrimSpace(goldenCase.Name)
	goldenCase.RiskTier = strings.TrimSpace(goldenCase.RiskTier)
	if goldenCase.RiskTier == "" {
		goldenCase.RiskTier = "low"
	}
	switch {
	case goldenCase.Name == "" || utf8.RuneCountInString(goldenCase.Name) > 200:
		return goldenCase, fmt.Errorf("%w: case name must contain 1 to 200 characters", domain.ErrValidation)
	case goldenCase.ReviewItemID < 0:
		return goldenCase, fmt.Errorf("%w: review item id cannot be negative", domain.ErrValidation)
	case len(bytes.TrimSpace(goldenCase.Input)) == 0 || len(goldenCase.Input) > maxBytes || !validText(goldenCase.Input):
		return goldenCase, fmt.Errorf("%w: case input must be 1 to %d bytes of UTF-8", domain.ErrValidation, maxBytes)
	case len(goldenCase.ExpectedBehavior) > maxBytes || !validText(goldenCase.ExpectedBehavior):
		return goldenCase, fmt.Errorf("%w: expected behavior must be at most %d bytes of UTF-8", domain.ErrValidation, maxBytes)
	case len(goldenCase.Language) > 20 || len(goldenCase.RubricRef) > 200:
		return goldenCase, fmt.Errorf("%w: language is at most 20 and rubric at most 200 characters", domain.ErrValidation)
	}
	return goldenCase, nil
}

// PutCase adds or replaces one case behind the set's working revision and returns the new revision.
func (store *GoldenCaseStore) PutCase(ctx context.Context, scope domain.GoldenScope, goldenCase domain.GoldenCase, expectedRevision int64) (int64, error) {
	if err := store.validateLimits(); err != nil {
		return 0, err
	}
	goldenCase, err := store.validCase(scope, goldenCase)
	if err != nil {
		return 0, err
	}
	return store.change(ctx, goldenCase.TenantID, goldenCase.GoldenSetID, expectedRevision, func(tx keelport.TxQueryService) error {
		tier, err := tx.Query(ctx, qCaseRiskTier, goldenCase.RiskTier)
		if err != nil {
			return fmt.Errorf("check risk tier: %w", err)
		}
		if len(tier.Rows) == 0 {
			return fmt.Errorf("%w: unknown risk tier %q", domain.ErrValidation, goldenCase.RiskTier)
		}
		others, err := tx.Query(ctx, qCaseCountOthers, goldenCase.TenantID, goldenCase.GoldenSetID, goldenCase.CaseID)
		if err != nil {
			return fmt.Errorf("count golden cases: %w", err)
		}
		if maxCases := orDefault(store.MaxCases, 500); len(others.Rows) > 0 && common.AsInt64(others.Rows[0][0]) >= int64(maxCases) {
			return fmt.Errorf("%w: a golden set holds at most %d cases", domain.ErrValidation, maxCases)
		}
		kind, id := principalColumns(goldenCase.ModifiedBy)
		written, err := tx.Query(ctx, qCaseUpsert,
			goldenCase.TenantID, goldenCase.GoldenSetID, goldenCase.CaseID, string(scopeOf(goldenCase.Hidden)), goldenCase.Name,
			string(goldenCase.Input), nullable(goldenCase.Language), nullable(string(goldenCase.ExpectedBehavior)), nullable(goldenCase.RubricRef),
			goldenCase.RiskTier, kind, id, store.now(), nullableID(goldenCase.ReviewItemID), string(scope))
		if err != nil {
			return fmt.Errorf("write golden case: %w", err)
		}
		if len(written.Rows) == 0 {
			return fmt.Errorf("%w: case id %q is not available in this scope", domain.ErrConflict, goldenCase.CaseID)
		}
		return nil
	})
}

// RemoveCase deletes one case visible in scope behind the set's working revision.
func (store *GoldenCaseStore) RemoveCase(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID, caseID string, expectedRevision int64) (int64, error) {
	if err := validScope(scope); err != nil {
		return 0, err
	}
	if tenantID <= 0 || strings.TrimSpace(goldenSetID) == "" || strings.TrimSpace(caseID) == "" {
		return 0, fmt.Errorf("%w: tenant, golden set, and case are required", domain.ErrValidation)
	}
	return store.change(ctx, tenantID, goldenSetID, expectedRevision, func(tx keelport.TxQueryService) error {
		removed, err := tx.Query(ctx, qCaseDelete, tenantID, goldenSetID, caseID, string(scope), string(scope))
		if err != nil {
			return fmt.Errorf("remove golden case: %w", err)
		}
		if len(removed.Rows) == 0 {
			return fmt.Errorf("%w: case %q", domain.ErrNotFound, caseID)
		}
		return nil
	})
}

// change advances the set's working revision and applies apply in the same transaction.
func (store *GoldenCaseStore) change(ctx context.Context, tenantID int64, goldenSetID string, expectedRevision int64, apply func(keelport.TxQueryService) error) (int64, error) {
	if expectedRevision < 0 {
		return 0, fmt.Errorf("%w: expected revision cannot be negative", domain.ErrValidation)
	}
	tx, err := store.begin(ctx, "golden case store", caseQueries)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	revision, err := bumpRevision(ctx, tx, tenantID, goldenSetID, expectedRevision)
	if err != nil {
		return 0, err
	}
	if err = apply(tx); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit golden case change: %w", err)
	}
	committed = true
	return revision, nil
}

func bumpRevision(ctx context.Context, tx keelport.TxQueryService, tenantID int64, goldenSetID string, expectedRevision int64) (int64, error) {
	bumped, err := tx.Query(ctx, qCaseBumpRevision, tenantID, goldenSetID, expectedRevision)
	if err != nil {
		return 0, fmt.Errorf("advance golden set revision: %w", err)
	}
	if len(bumped.Rows) == 0 {
		return 0, fmt.Errorf("%w: golden set %q is not at revision %d", domain.ErrRevisionConflict, goldenSetID, expectedRevision)
	}
	return common.AsInt64(bumped.Rows[0][0]), nil
}

// Freeze snapshots every case of the working copy into the next set version and returns it with the new working revision.
func (store *GoldenCaseStore) Freeze(ctx context.Context, tenantID int64, goldenSetID string, expectedRevision int64) (domain.GoldenSetVersion, int64, error) {
	if err := store.validateLimits(); err != nil {
		return domain.GoldenSetVersion{}, 0, err
	}
	if store.Storage == nil || strings.TrimSpace(store.Storage.Bucket()) == "" {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("golden case store: object storage with a bucket is required to freeze")
	}
	if err := validIdentifier("golden_set_id", goldenSetID, 80); err != nil {
		return domain.GoldenSetVersion{}, 0, err
	}
	if tenantID <= 0 || expectedRevision < 0 {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("%w: tenant and a non-negative expected revision are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "golden case store", caseQueries)
	if err != nil {
		return domain.GoldenSetVersion{}, 0, err
	}
	current, err := qs.Query(ctx, qCaseSetRevision, tenantID, goldenSetID)
	if err != nil {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("load golden set revision: %w", err)
	}
	if len(current.Rows) == 0 {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("%w: golden set %q", domain.ErrNotFound, goldenSetID)
	}
	if common.AsInt64(current.Rows[0][0]) != expectedRevision {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("%w: golden set %q is not at revision %d", domain.ErrRevisionConflict, goldenSetID, expectedRevision)
	}
	cases, err := store.list(ctx, qs, domain.GoldenScopeGate, tenantID, goldenSetID)
	if err != nil {
		return domain.GoldenSetVersion{}, 0, err
	}
	if len(cases) == 0 {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("%w: golden set %q has no cases to freeze", domain.ErrValidation, goldenSetID)
	}
	// Uploads are content-addressed, so a freeze that later loses its revision race leaves only reusable objects.
	payloads := make([]domain.ObjectRef, len(cases))
	for i, goldenCase := range cases {
		if payloads[i], err = store.upload(ctx, tenantID, goldenSetID, goldenCase.Input); err != nil {
			return domain.GoldenSetVersion{}, 0, err
		}
	}
	tx, err := store.begin(ctx, "golden case store", caseQueries)
	if err != nil {
		return domain.GoldenSetVersion{}, 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	revision, err := bumpRevision(ctx, tx, tenantID, goldenSetID, expectedRevision)
	if err != nil {
		return domain.GoldenSetVersion{}, 0, err
	}
	next, err := tx.Query(ctx, qCaseNextVersion, tenantID, goldenSetID)
	if err != nil || len(next.Rows) == 0 {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("next golden set version: %w", errors.Join(err, errNoRow))
	}
	version := domain.GoldenSetVersion{TenantID: tenantID, GoldenSetID: goldenSetID, SetVersion: common.AsInt64(next.Rows[0][0]), ExampleCount: len(cases), FrozenAt: store.now()}
	examples := make([]domain.GoldenExample, len(cases))
	for i, goldenCase := range cases {
		examples[i] = domain.GoldenExample{
			TenantID: tenantID, GoldenSetID: goldenSetID, SetVersion: version.SetVersion, ExampleID: goldenCase.CaseID,
			Provenance: GoldenCaseProvenance, ConsentClass: GoldenCaseConsent, RetentionClass: GoldenCaseRetention,
			RiskTier: goldenCase.RiskTier, Language: goldenCase.Language, RubricRef: goldenCase.RubricRef,
			ExpectedBehavior: goldenCase.ExpectedBehavior, Payload: payloads[i], Hidden: goldenCase.Hidden,
		}
	}
	version.DatasetRevision = DatasetRevision(examples)
	if _, err = tx.Query(ctx, qCaseVersionInsert, tenantID, goldenSetID, version.SetVersion, version.DatasetRevision, version.ExampleCount, version.FrozenAt); err != nil {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("insert golden set version: %w", err)
	}
	for _, example := range examples {
		if _, err = tx.Query(ctx, qCaseExampleInsert,
			tenantID, goldenSetID, version.SetVersion, example.ExampleID, string(scopeOf(example.Hidden)),
			example.Provenance, example.ConsentClass, example.RetentionClass, example.RiskTier, nil, nullable(example.Language),
			nullable(example.RubricRef), nullable(string(example.ExpectedBehavior)), example.Payload.URI, example.Payload.Digest, "[]",
		); err != nil {
			return domain.GoldenSetVersion{}, 0, fmt.Errorf("insert golden example %q: %w", example.ExampleID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.GoldenSetVersion{}, 0, fmt.Errorf("commit golden set freeze: %w", err)
	}
	committed = true
	return version, revision, nil
}

var errNoRow = errors.New("no row returned")

func (store *GoldenCaseStore) upload(ctx context.Context, tenantID int64, goldenSetID string, input []byte) (domain.ObjectRef, error) {
	ref, err := putContent(ctx, store.Storage, store.Scheme, fmt.Sprintf("evaluation/golden/%d/%s", tenantID, goldenSetID), input)
	if err != nil {
		return domain.ObjectRef{}, fmt.Errorf("upload golden case input: %w", err)
	}
	return ref, nil
}

func principalColumns(principal domain.PrincipalRef) (any, any) {
	if principal.Kind == "" || principal.ID == "" {
		return nil, nil
	}
	return string(principal.Kind), principal.ID
}

// validText accepts UTF-8 without NUL, which PostgreSQL text columns refuse.
func validText(value []byte) bool {
	return utf8.Valid(value) && bytes.IndexByte(value, 0) < 0
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
