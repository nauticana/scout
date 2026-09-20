package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// TableTenantPolicyRepository reads the tenant's current runtime policy. A tenant
// without one is ErrNotReady: limits are never assumed.
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
	row := result.Rows[0]
	return domain.TenantRuntimePolicy{
		PriorityClass: common.AsString(row[0]), CapacityClass: common.AsString(row[1]),
		MaxSteps: int(common.AsInt64(row[2])), MaxTokens: common.AsInt64(row[3]),
		MaxCostMinorUnits: common.AsInt64(row[4]), CostCurrency: strings.TrimSpace(common.AsString(row[5])),
		TurnTimeout: time.Duration(common.AsInt64(row[6])) * time.Millisecond, MidTurnPolicy: domain.AdmissionQueue,
	}, nil
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
	_ contract.GuardrailConfigRepository = (*TableGuardrailConfigRepository)(nil)
)
