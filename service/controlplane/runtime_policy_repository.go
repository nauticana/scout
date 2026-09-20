package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qRuntimePolicyCurrent = "scout_tenant_runtime_policy_current"
	qRuntimePolicyInsert  = "scout_tenant_runtime_policy_insert"
	qRuntimePolicyGet     = "scout_tenant_runtime_policy_get"
	qRuntimePolicyPoint   = "scout_tenant_current_policy_point"
	qRuntimePolicyDefault = "scout_tenant_current_policy_default"
	qGuardrailPublish     = "scout_guardrail_config_publish"
	qGuardrailGet         = "scout_guardrail_config_get"
	qGuardrailPinned      = "scout_guardrail_config_pinned"
)

var runtimePolicyQueries = map[string]string{
	qRuntimePolicyCurrent: `
SELECT p.priority_class_code, p.capacity_class_code, p.max_steps, p.max_tokens,
       p.max_cost_minor_units, p.cost_currency_code, p.turn_timeout_ms
  FROM tenant_current_policy c
  JOIN tenant_runtime_policy p ON p.tenant_id = c.tenant_id AND p.policy_version = c.policy_version
 WHERE c.tenant_id = ?`,
	qRuntimePolicyInsert: `
INSERT INTO tenant_runtime_policy (tenant_id, policy_version, priority_class_code, capacity_class_code,
       max_steps, max_tokens, max_cost_minor_units, cost_currency_code, turn_timeout_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, policy_version) DO NOTHING`,
	qRuntimePolicyGet: `
SELECT priority_class_code, capacity_class_code, max_steps, max_tokens,
       max_cost_minor_units, cost_currency_code, turn_timeout_ms
  FROM tenant_runtime_policy
 WHERE tenant_id = ? AND policy_version = ?`,
	qRuntimePolicyPoint: `
INSERT INTO tenant_current_policy (tenant_id, policy_version)
VALUES (?, ?)
ON CONFLICT (tenant_id) DO UPDATE SET policy_version = EXCLUDED.policy_version`,
	qRuntimePolicyDefault: `
INSERT INTO tenant_current_policy (tenant_id, policy_version)
VALUES (?, ?)
ON CONFLICT (tenant_id) DO NOTHING`,
	qGuardrailPublish: `
INSERT INTO guardrail_config (tenant_id, agent_id, guardrail_version, rules, rules_digest)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id, guardrail_version) DO NOTHING`,
	qGuardrailGet: `
SELECT guardrail_version, rules, rules_digest
  FROM guardrail_config
 WHERE tenant_id = ? AND agent_id = ? AND guardrail_version = ?`,
	qGuardrailPinned: `
SELECT g.guardrail_version, g.rules, g.rules_digest
  FROM agent_version v
  JOIN guardrail_config g ON g.tenant_id = v.tenant_id AND g.agent_id = v.agent_id AND g.guardrail_version = v.guardrail_version
 WHERE v.tenant_id = ? AND v.agent_id = ? AND v.agent_version = ?`,
}

// TableTenantPolicyRepository publishes immutable runtime policy versions and reads
// the tenant's current one. A tenant without one is ErrNotReady: limits are never assumed.
type TableTenantPolicyRepository struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (repository *TableTenantPolicyRepository) GetRuntimePolicy(ctx context.Context, tenantID int64) (domain.TenantRuntimePolicy, error) {
	if repository.DB == nil {
		return domain.TenantRuntimePolicy{}, fmt.Errorf("tenant policy repository: database is required")
	}
	repository.once.Do(func() { repository.qs = repository.DB.GetQueryService(ctx, runtimePolicyQueries) })
	result, err := repository.qs.Query(ctx, qRuntimePolicyCurrent, tenantID)
	if err != nil {
		return domain.TenantRuntimePolicy{}, fmt.Errorf("read runtime policy: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.TenantRuntimePolicy{}, fmt.Errorf("%w: tenant %d has no current runtime policy", domain.ErrNotReady, tenantID)
	}
	return runtimePolicyFromRow(result.Rows[0]), nil
}

func runtimePolicyFromRow(row []any) domain.TenantRuntimePolicy {
	return domain.TenantRuntimePolicy{
		PriorityClass: common.AsString(row[0]), CapacityClass: common.AsString(row[1]),
		MaxSteps: int(common.AsInt64(row[2])), MaxTokens: common.AsInt64(row[3]),
		MaxCostMinorUnits: common.AsInt64(row[4]), CostCurrency: strings.TrimSpace(common.AsString(row[5])),
		TurnTimeout: time.Duration(common.AsInt64(row[6])) * time.Millisecond, MidTurnPolicy: domain.AdmissionQueue,
	}
}

// PublishRuntimePolicy stores the version and makes it current in one transaction.
func (repository *TableTenantPolicyRepository) PublishRuntimePolicy(ctx context.Context, tenantID int64, version domain.RuntimePolicyVersion) error {
	if repository.DB == nil {
		return fmt.Errorf("tenant policy repository: database is required")
	}
	if err := validateRuntimePolicy(tenantID, version); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := repository.DB.BeginTx(ctx, runtimePolicyQueries)
	if err != nil {
		return fmt.Errorf("begin runtime policy transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = writeRuntimePolicy(ctx, tx, tenantID, version, qRuntimePolicyPoint); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit runtime policy: %w", err)
	}
	committed = true
	return nil
}

// writeRuntimePolicy inserts the immutable version and runs the given pointer statement inside the caller's transaction.
func writeRuntimePolicy(ctx context.Context, tx keelport.TxQueryService, tenantID int64, version domain.RuntimePolicyVersion, pointer string) error {
	policy := version.Policy
	if _, err := tx.Query(ctx, qRuntimePolicyInsert, tenantID, version.Version, policy.PriorityClass, policy.CapacityClass,
		policy.MaxSteps, policy.MaxTokens, policy.MaxCostMinorUnits, policy.CostCurrency, policy.TurnTimeout.Milliseconds()); err != nil {
		return fmt.Errorf("insert runtime policy: %w", err)
	}
	stored, err := tx.Query(ctx, qRuntimePolicyGet, tenantID, version.Version)
	if err != nil {
		return fmt.Errorf("read runtime policy: %w", err)
	}
	policy.MidTurnPolicy = domain.AdmissionQueue
	if len(stored.Rows) == 0 || runtimePolicyFromRow(stored.Rows[0]) != policy {
		return fmt.Errorf("%w: runtime policy version %s of tenant %d already holds different limits", domain.ErrConflict, version.Version, tenantID)
	}
	if _, err = tx.Query(ctx, pointer, tenantID, version.Version); err != nil {
		return fmt.Errorf("point current runtime policy: %w", err)
	}
	return nil
}

func validateRuntimePolicy(tenantID int64, version domain.RuntimePolicyVersion) error {
	policy := version.Policy
	timeoutMS := policy.TurnTimeout.Milliseconds()
	switch {
	case tenantID <= 0 || strings.TrimSpace(version.Version) == "":
		return fmt.Errorf("%w: tenant and policy version are required", domain.ErrValidation)
	case strings.TrimSpace(policy.PriorityClass) == "" || strings.TrimSpace(policy.CapacityClass) == "":
		return fmt.Errorf("%w: priority and capacity class are required", domain.ErrValidation)
	case policy.MaxSteps <= 0 || policy.MaxSteps > math.MaxInt32 || policy.MaxTokens <= 0 || policy.MaxCostMinorUnits < 0:
		return fmt.Errorf("%w: max steps and tokens must be positive and max cost non-negative", domain.ErrValidation)
	case len(policy.CostCurrency) != 3:
		return fmt.Errorf("%w: cost currency must be an ISO 4217 code", domain.ErrValidation)
	case timeoutMS <= 0 || timeoutMS > math.MaxInt32:
		return fmt.Errorf("%w: turn timeout must be between 1ms and %dms", domain.ErrValidation, math.MaxInt32)
	}
	return nil
}

// TableGuardrailConfigRepository stores immutable guardrail versions and reads
// the one an agent version pins. An agent version that pins none gets the empty
// policy, which leaves only the release-independent baseline in force.
type TableGuardrailConfigRepository struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (repository *TableGuardrailConfigRepository) queries(ctx context.Context) (keelport.QueryService, error) {
	if repository.DB == nil {
		return nil, fmt.Errorf("guardrail config repository: database is required")
	}
	repository.once.Do(func() { repository.qs = repository.DB.GetQueryService(ctx, runtimePolicyQueries) })
	return repository.qs, nil
}

func (repository *TableGuardrailConfigRepository) Publish(ctx context.Context, tenantID int64, agentID string, config domain.GuardrailConfig) error {
	qs, err := repository.queries(ctx)
	if err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(config.Version) == "" || len(config.Rules) == 0 {
		return fmt.Errorf("%w: tenant, agent, guardrail version, and rules are required", domain.ErrValidation)
	}
	sum := sha256.Sum256(config.Rules)
	digest := hex.EncodeToString(sum[:])
	if config.RulesDigest != "" && config.RulesDigest != digest {
		return fmt.Errorf("%w: guardrail rules do not match their digest", domain.ErrValidation)
	}
	ctx = context.WithoutCancel(ctx)
	if _, err = qs.Query(ctx, qGuardrailPublish, tenantID, agentID, config.Version, string(config.Rules), digest); err != nil {
		return fmt.Errorf("publish guardrail config: %w", err)
	}
	stored, err := qs.Query(ctx, qGuardrailGet, tenantID, agentID, config.Version)
	if err != nil {
		return fmt.Errorf("read guardrail config: %w", err)
	}
	if len(stored.Rows) == 0 || common.AsString(stored.Rows[0][2]) != digest {
		return fmt.Errorf("%w: guardrail version %s of %q already holds different rules", domain.ErrConflict, config.Version, agentID)
	}
	return nil
}

func (repository *TableGuardrailConfigRepository) Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.GuardrailConfig, error) {
	qs, err := repository.queries(ctx)
	if err != nil {
		return domain.GuardrailConfig{}, err
	}
	result, err := qs.Query(ctx, qGuardrailPinned, tenantID, agentID, agentVersion)
	if err != nil {
		return domain.GuardrailConfig{}, fmt.Errorf("read pinned guardrail config: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.GuardrailConfig{}, nil
	}
	row := result.Rows[0]
	return domain.GuardrailConfig{Version: common.AsString(row[0]), Rules: []byte(common.AsString(row[1])), RulesDigest: common.AsString(row[2])}, nil
}

var (
	_ contract.TenantPolicyRepository    = (*TableTenantPolicyRepository)(nil)
	_ contract.TenantPolicyPublisher     = (*TableTenantPolicyRepository)(nil)
	_ contract.GuardrailConfigRepository = (*TableGuardrailConfigRepository)(nil)
)
