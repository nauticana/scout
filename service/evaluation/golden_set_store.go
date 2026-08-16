package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qGoldenVersionInsert = "scout_evaluation_golden_version_insert"
	qGoldenExampleInsert = "scout_evaluation_golden_example_insert"
	qGoldenExampleGet    = "scout_evaluation_golden_example_get"
	qGoldenExampleList   = "scout_evaluation_golden_example_list"
	qGoldenQueryInsert   = "scout_evaluation_golden_query_insert"
	qGoldenQueryList     = "scout_evaluation_golden_query_list"
)

const goldenExampleColumns = `tenant_id, golden_set_id, set_version, example_id, scope_code, provenance, consent_class, retention_class,
       risk_tier, domain_code, language_code, rubric_ref, expected_behavior, payload_uri, payload_digest, reviews`

// The list queries take the caller scope twice: dev callers see only dev rows,
// gate callers see every row. Authorization is in the predicate, never post-filtered.
var goldenQueries = map[string]string{
	qGoldenVersionInsert: `
INSERT INTO golden_set_version (tenant_id, golden_set_id, set_version, dataset_revision, example_count, frozen_at)
VALUES (?, ?, ?, ?, ?, ?)`,
	qGoldenExampleInsert: `
INSERT INTO golden_example (` + goldenExampleColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qGoldenExampleGet: `
SELECT ` + goldenExampleColumns + `
  FROM golden_example
 WHERE tenant_id = ? AND golden_set_id = ? AND set_version = ? AND example_id = ?`,
	qGoldenExampleList: `
SELECT ` + goldenExampleColumns + `
  FROM golden_example
 WHERE tenant_id = ? AND golden_set_id = ? AND set_version = ?
   AND (scope_code = ? OR ? = 'gate')
 ORDER BY example_id`,
	qGoldenQueryInsert: `
INSERT INTO golden_query (tenant_id, golden_set_id, set_version, query_id, scope_code, knowledge_base_id, query_text,
                          principal, entitlements, expected_document_ids, expect_abstention)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qGoldenQueryList: `
SELECT tenant_id, golden_set_id, set_version, query_id, knowledge_base_id, query_text, principal, entitlements,
       expected_document_ids, expect_abstention
  FROM golden_query
 WHERE tenant_id = ? AND golden_set_id = ? AND set_version = ?
   AND (scope_code = ? OR ? = 'gate')
 ORDER BY query_id`,
}

// GoldenSetStore persists golden sets, examples, and queries under a read scope.
type GoldenSetStore struct {
	keelStore
}

var _ contract.GoldenSetStore = (*GoldenSetStore)(nil)

func scopeOf(hidden bool) domain.GoldenScope {
	if hidden {
		return domain.GoldenScopeGate
	}
	return domain.GoldenScopeDev
}

func validScope(scope domain.GoldenScope) error {
	if scope != domain.GoldenScopeDev && scope != domain.GoldenScopeGate {
		return fmt.Errorf("%w: unknown golden scope %q", domain.ErrValidation, scope)
	}
	return nil
}

func validSetKey(tenantID int64, goldenSetID string, setVersion int64) error {
	if tenantID <= 0 || strings.TrimSpace(goldenSetID) == "" || setVersion <= 0 {
		return fmt.Errorf("%w: tenant, golden set id, and positive version are required", domain.ErrValidation)
	}
	return nil
}

// FreezeVersion records a set version with its dataset revision before examples are added under it.
func (store *GoldenSetStore) FreezeVersion(ctx context.Context, version domain.GoldenSetVersion) error {
	if err := validSetKey(version.TenantID, version.GoldenSetID, version.SetVersion); err != nil {
		return err
	}
	if !isSHA256Hex(version.DatasetRevision) || version.ExampleCount < 0 {
		return fmt.Errorf("%w: dataset revision digest and non-negative example count are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "golden set store", goldenQueries)
	if err != nil {
		return err
	}
	if _, err := qs.Query(ctx, qGoldenVersionInsert, version.TenantID, version.GoldenSetID, version.SetVersion, version.DatasetRevision, version.ExampleCount, version.FrozenAt.UTC()); err != nil {
		return fmt.Errorf("freeze golden set version: %w", err)
	}
	return nil
}

// PutExample inserts an example; hidden examples may only be written in the gate scope.
func (store *GoldenSetStore) PutExample(ctx context.Context, scope domain.GoldenScope, example domain.GoldenExample) error {
	if err := validScope(scope); err != nil {
		return err
	}
	if err := validSetKey(example.TenantID, example.GoldenSetID, example.SetVersion); err != nil {
		return err
	}
	if strings.TrimSpace(example.ExampleID) == "" || !isSHA256Hex(example.Payload.Digest) || strings.TrimSpace(example.Payload.URI) == "" {
		return fmt.Errorf("%w: example id and payload reference with digest are required", domain.ErrValidation)
	}
	if example.Hidden && scope != domain.GoldenScopeGate {
		return fmt.Errorf("%w: hidden examples require the gate scope", domain.ErrForbidden)
	}
	reviews, err := json.Marshal(example.Reviews)
	if err != nil {
		return fmt.Errorf("encode reviews: %w", err)
	}
	qs, err := store.queries(ctx, "golden set store", goldenQueries)
	if err != nil {
		return err
	}
	if _, err := qs.Query(ctx, qGoldenExampleInsert,
		example.TenantID, example.GoldenSetID, example.SetVersion, example.ExampleID, string(scopeOf(example.Hidden)),
		example.Provenance, example.ConsentClass, example.RetentionClass, example.RiskTier, nullable(example.Domain), nullable(example.Language),
		nullable(example.RubricRef), nullable(string(example.ExpectedBehavior)), example.Payload.URI, example.Payload.Digest, string(reviews),
	); err != nil {
		return fmt.Errorf("insert golden example: %w", err)
	}
	return nil
}

// GetExample returns one example; a hidden example read in the dev scope is forbidden.
func (store *GoldenSetStore) GetExample(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID string, setVersion int64, exampleID string) (domain.GoldenExample, error) {
	if err := validScope(scope); err != nil {
		return domain.GoldenExample{}, err
	}
	if err := validSetKey(tenantID, goldenSetID, setVersion); err != nil {
		return domain.GoldenExample{}, err
	}
	qs, err := store.queries(ctx, "golden set store", goldenQueries)
	if err != nil {
		return domain.GoldenExample{}, err
	}
	result, err := qs.Query(ctx, qGoldenExampleGet, tenantID, goldenSetID, setVersion, exampleID)
	if err != nil {
		return domain.GoldenExample{}, fmt.Errorf("get golden example: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.GoldenExample{}, fmt.Errorf("%w: golden example %q", domain.ErrNotFound, exampleID)
	}
	example, err := decodeGoldenExample(result.Rows[0])
	if err != nil {
		return domain.GoldenExample{}, err
	}
	if example.Hidden && scope != domain.GoldenScopeGate {
		return domain.GoldenExample{}, fmt.Errorf("%w: example %q is a hidden gate example", domain.ErrForbidden, exampleID)
	}
	return example, nil
}

// ListExamples returns the examples visible in the scope, ordered by id.
func (store *GoldenSetStore) ListExamples(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID string, setVersion int64) ([]domain.GoldenExample, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	if err := validSetKey(tenantID, goldenSetID, setVersion); err != nil {
		return nil, err
	}
	qs, err := store.queries(ctx, "golden set store", goldenQueries)
	if err != nil {
		return nil, err
	}
	result, err := qs.Query(ctx, qGoldenExampleList, tenantID, goldenSetID, setVersion, string(scope), string(scope))
	if err != nil {
		return nil, fmt.Errorf("list golden examples: %w", err)
	}
	examples := make([]domain.GoldenExample, 0, len(result.Rows))
	for _, row := range result.Rows {
		example, err := decodeGoldenExample(row)
		if err != nil {
			return nil, err
		}
		if example.Hidden && scope != domain.GoldenScopeGate {
			return nil, fmt.Errorf("%w: hidden example returned in dev scope", domain.ErrForbidden)
		}
		examples = append(examples, example)
	}
	return examples, nil
}

// PutQuery inserts a retrieval golden query under the given scope.
func (store *GoldenSetStore) PutQuery(ctx context.Context, scope domain.GoldenScope, query domain.GoldenQuery) error {
	if err := validScope(scope); err != nil {
		return err
	}
	if err := validSetKey(query.TenantID, query.GoldenSetID, query.SetVersion); err != nil {
		return err
	}
	if strings.TrimSpace(query.QueryID) == "" || strings.TrimSpace(query.KnowledgeBaseID) == "" || strings.TrimSpace(query.Principal) == "" || len(query.Query) == 0 {
		return fmt.Errorf("%w: query id, knowledge base, principal, and query text are required", domain.ErrValidation)
	}
	if len(query.Entitlements) == 0 {
		return fmt.Errorf("%w: golden query entitlements are required; nil fails closed", domain.ErrValidation)
	}
	expected, err := json.Marshal(query.ExpectedDocumentIDs)
	if err != nil {
		return fmt.Errorf("encode expected documents: %w", err)
	}
	qs, err := store.queries(ctx, "golden set store", goldenQueries)
	if err != nil {
		return err
	}
	if _, err := qs.Query(ctx, qGoldenQueryInsert,
		query.TenantID, query.GoldenSetID, query.SetVersion, query.QueryID, string(scope), query.KnowledgeBaseID, string(query.Query),
		query.Principal, string(query.Entitlements), string(expected), query.ExpectAbstention,
	); err != nil {
		return fmt.Errorf("insert golden query: %w", err)
	}
	return nil
}

// ListQueries returns the golden queries visible in the scope, ordered by id.
func (store *GoldenSetStore) ListQueries(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID string, setVersion int64) ([]domain.GoldenQuery, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	if err := validSetKey(tenantID, goldenSetID, setVersion); err != nil {
		return nil, err
	}
	qs, err := store.queries(ctx, "golden set store", goldenQueries)
	if err != nil {
		return nil, err
	}
	result, err := qs.Query(ctx, qGoldenQueryList, tenantID, goldenSetID, setVersion, string(scope), string(scope))
	if err != nil {
		return nil, fmt.Errorf("list golden queries: %w", err)
	}
	queries := make([]domain.GoldenQuery, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 10 {
			return nil, fmt.Errorf("golden query row: expected 10 columns, got %d", len(row))
		}
		var expected []string
		if raw := common.AsString(row[8]); raw != "" {
			if err := json.Unmarshal([]byte(raw), &expected); err != nil {
				return nil, fmt.Errorf("decode expected documents: %w", err)
			}
		}
		queries = append(queries, domain.GoldenQuery{
			TenantID: common.AsInt64(row[0]), GoldenSetID: common.AsString(row[1]), SetVersion: common.AsInt64(row[2]), QueryID: common.AsString(row[3]),
			KnowledgeBaseID: common.AsString(row[4]), Query: []byte(common.AsString(row[5])), Principal: common.AsString(row[6]),
			Entitlements: []byte(common.AsString(row[7])), ExpectedDocumentIDs: expected, ExpectAbstention: common.AsBool(row[9]),
		})
	}
	return queries, nil
}

func decodeGoldenExample(row []any) (domain.GoldenExample, error) {
	if len(row) < 16 {
		return domain.GoldenExample{}, fmt.Errorf("golden example row: expected 16 columns, got %d", len(row))
	}
	example := domain.GoldenExample{
		TenantID: common.AsInt64(row[0]), GoldenSetID: common.AsString(row[1]), SetVersion: common.AsInt64(row[2]), ExampleID: common.AsString(row[3]),
		Hidden: common.AsString(row[4]) == string(domain.GoldenScopeGate), Provenance: common.AsString(row[5]), ConsentClass: common.AsString(row[6]),
		RetentionClass: common.AsString(row[7]), RiskTier: common.AsString(row[8]), Domain: common.AsString(row[9]), Language: common.AsString(row[10]),
		RubricRef: common.AsString(row[11]), Payload: domain.ObjectRef{URI: common.AsString(row[13]), Digest: common.AsString(row[14])},
	}
	if expected := common.AsString(row[12]); expected != "" {
		example.ExpectedBehavior = []byte(expected)
	}
	if reviews := common.AsString(row[15]); reviews != "" && reviews != "null" {
		if err := json.Unmarshal([]byte(reviews), &example.Reviews); err != nil {
			return domain.GoldenExample{}, fmt.Errorf("decode example reviews: %w", err)
		}
	}
	return example, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
