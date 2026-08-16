package release

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qPinInsert          = "scout_release_pin_insert"
	qPinActive          = "scout_release_pin_active"
	qPinExpire          = "scout_release_pin_expire"
	qCohortsForAgent    = "scout_release_cohorts_for_agent"
	qDeploymentGet      = "scout_release_deployment_get"
	qDeploymentCanary   = "scout_release_deployment_canary"
	qDeploymentPromote  = "scout_release_deployment_promote"
	qDeploymentPrevious = "scout_release_deployment_previous"
	qDeploymentRestore  = "scout_release_deployment_restore"
	qDeploymentClear    = "scout_release_deployment_clear"
	qVersionRetained    = "scout_release_version_retained"
)

var versionPinQueries = map[string]string{
	qPinInsert: `
INSERT INTO agent_version_pin
       (tenant_id, agent_id, agent_version, scope_code, region, reason, owner, approved_by, signature,
        compatible_policy_versions, compatible_index_versions, effective_at, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id`,
	qPinActive: `
SELECT id, agent_version, scope_code, region, reason, owner, approved_by, signature,
       compatible_policy_versions, compatible_index_versions, effective_at, expires_at, created_at
  FROM agent_version_pin
 WHERE tenant_id = ? AND agent_id = ? AND effective_at <= ? AND (expires_at IS NULL OR expires_at > ?)
 ORDER BY CASE scope_code WHEN 'compliance' THEN 0 ELSE 1 END, created_at DESC, id DESC`,
	qPinExpire: `
UPDATE agent_version_pin SET expires_at = ?
 WHERE tenant_id = ? AND id = ? AND (expires_at IS NULL OR expires_at > ?)`,
	qCohortsForAgent: `
SELECT experiment_id, agent_version, percentage, salt
  FROM experiment_cohort
 WHERE tenant_id = ? AND agent_id = ?
 ORDER BY experiment_id`,
	qDeploymentGet: `
SELECT stable_version, canary_version, canary_percentage
  FROM agent_deployment
 WHERE tenant_id = ? AND agent_id = ?`,
	qDeploymentCanary: `
UPDATE agent_deployment
   SET canary_version = ?, canary_percentage = ?, updated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ? AND stable_version <> ?
RETURNING stable_version`,
	qDeploymentPromote: `
UPDATE agent_deployment
   SET stable_version = ?, canary_version = NULL, canary_percentage = 0, updated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ?
RETURNING stable_version`,
	qDeploymentClear: `
UPDATE agent_deployment
   SET canary_version = NULL, canary_percentage = 0, updated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ? AND canary_version IS NOT NULL
RETURNING stable_version`,
	qDeploymentPrevious: `
SELECT v.agent_version
  FROM agent_version v
  JOIN agent_deployment dep ON dep.tenant_id = v.tenant_id AND dep.agent_id = v.agent_id
  JOIN agent_version stable ON stable.tenant_id = dep.tenant_id AND stable.agent_id = dep.agent_id AND stable.agent_version = dep.stable_version
 WHERE v.tenant_id = ? AND v.agent_id = ? AND v.agent_version <> stable.agent_version
   AND (v.published_at < stable.published_at OR (v.published_at = stable.published_at AND v.agent_version < stable.agent_version))
 ORDER BY v.published_at DESC, v.agent_version DESC
 LIMIT 1`,
	qDeploymentRestore: `
UPDATE agent_deployment
   SET stable_version = ?, canary_version = NULL, canary_percentage = 0, updated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ? AND stable_version = ?
RETURNING stable_version`,
	// A version is retained while it is deployed, pinned, in a cohort, or held by an open conversation.
	qVersionRetained: `
SELECT EXISTS (SELECT 1 FROM agent_deployment WHERE tenant_id = ? AND agent_id = ? AND (stable_version = ? OR canary_version = ?))
    OR EXISTS (SELECT 1 FROM agent_version_pin WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND (expires_at IS NULL OR expires_at > ?))
    OR EXISTS (SELECT 1 FROM experiment_cohort WHERE tenant_id = ? AND agent_id = ? AND agent_version = ?)
    OR EXISTS (SELECT 1 FROM agent_conversation WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND closed_at IS NULL)`,
}

// TableVersionPinStore is the keel-backed VersionPinStore over agent_version_pin.
type TableVersionPinStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.VersionPinStore = (*TableVersionPinStore)(nil)

func (store *TableVersionPinStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("version pin store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, versionPinQueries) })
	if store.qs == nil {
		return fmt.Errorf("version pin store: query service is required")
	}
	return nil
}

func (store *TableVersionPinStore) Put(ctx context.Context, pin domain.VersionPin) (int64, error) {
	switch {
	case pin.TenantID <= 0 || strings.TrimSpace(pin.AgentID) == "" || strings.TrimSpace(pin.Version) == "":
		return 0, fmt.Errorf("%w: pin tenant, agent, and version are required", domain.ErrValidation)
	case pin.Scope != domain.PinScopeCompliance && pin.Scope != domain.PinScopeTenant:
		return 0, fmt.Errorf("%w: pin scope must be compliance or tenant", domain.ErrValidation)
	case strings.TrimSpace(pin.Reason) == "" || strings.TrimSpace(pin.Owner) == "" || pin.EffectiveAt.IsZero():
		return 0, fmt.Errorf("%w: pin reason, owner, and effective time are required", domain.ErrValidation)
	case !pin.ExpiresAt.IsZero() && !pin.ExpiresAt.After(pin.EffectiveAt):
		return 0, fmt.Errorf("%w: pin expiry must follow its effective time", domain.ErrValidation)
	case pin.Scope == domain.PinScopeCompliance && (strings.TrimSpace(pin.ApprovedBy) == "" || strings.TrimSpace(pin.Signature) == ""):
		return 0, fmt.Errorf("%w: compliance pins require approval and signature", domain.ErrForbidden)
	}
	if err := store.init(ctx); err != nil {
		return 0, err
	}
	createdAt := pin.CreatedAt
	if createdAt.IsZero() {
		createdAt = pin.EffectiveAt
	}
	result, err := store.qs.Query(ctx, qPinInsert,
		pin.TenantID, pin.AgentID, pin.Version, string(pin.Scope), nullableString(pin.Region), pin.Reason, pin.Owner,
		nullableString(pin.ApprovedBy), nullableString(pin.Signature), encodeList(pin.CompatiblePolicyVersions),
		encodeList(pin.CompatibleIndexVersions), pin.EffectiveAt, nullableTime(pin.ExpiresAt), createdAt)
	if err != nil {
		return 0, fmt.Errorf("insert version pin: %w", err)
	}
	if len(result.Rows) == 0 {
		return 0, fmt.Errorf("insert version pin: no id returned")
	}
	return common.AsInt64(result.Rows[0][0]), nil
}

func (store *TableVersionPinStore) Active(ctx context.Context, tenantID int64, agentID string, at time.Time) ([]domain.VersionPin, error) {
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	result, err := store.qs.Query(ctx, qPinActive, tenantID, agentID, at, at)
	if err != nil {
		return nil, fmt.Errorf("list active pins: %w", err)
	}
	pins := make([]domain.VersionPin, 0, len(result.Rows))
	for _, row := range result.Rows {
		policies, err := decodeList(row[8])
		if err != nil {
			return nil, fmt.Errorf("decode pin policy versions: %w", err)
		}
		indexes, err := decodeList(row[9])
		if err != nil {
			return nil, fmt.Errorf("decode pin index versions: %w", err)
		}
		pins = append(pins, domain.VersionPin{
			ID: common.AsInt64(row[0]), TenantID: tenantID, AgentID: agentID, Version: common.AsString(row[1]),
			Scope: domain.PinScope(common.AsString(row[2])), Region: common.AsString(row[3]), Reason: common.AsString(row[4]),
			Owner: common.AsString(row[5]), ApprovedBy: common.AsString(row[6]), Signature: common.AsString(row[7]),
			CompatiblePolicyVersions: policies, CompatibleIndexVersions: indexes,
			EffectiveAt: common.AsTime(row[10]), ExpiresAt: common.AsTime(row[11]), CreatedAt: common.AsTime(row[12]),
		})
	}
	return pins, nil
}

func (store *TableVersionPinStore) Expire(ctx context.Context, tenantID int64, pinID int64, at time.Time) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	if _, err := store.qs.Query(ctx, qPinExpire, at, tenantID, pinID, at); err != nil {
		return fmt.Errorf("expire version pin: %w", err)
	}
	return nil
}

// TableExperimentCohortResolver hashes a subject into at most one experiment_cohort row.
type TableExperimentCohortResolver struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.ExperimentCohortResolver = (*TableExperimentCohortResolver)(nil)

func (resolver *TableExperimentCohortResolver) init(ctx context.Context) error {
	if resolver.DB == nil {
		return fmt.Errorf("experiment cohort resolver: database is required")
	}
	resolver.once.Do(func() { resolver.qs = resolver.DB.GetQueryService(ctx, versionPinQueries) })
	if resolver.qs == nil {
		return fmt.Errorf("experiment cohort resolver: query service is required")
	}
	return nil
}

func (resolver *TableExperimentCohortResolver) Resolve(ctx context.Context, tenantID int64, agentID, subjectKey string) (domain.ExperimentCohort, bool, error) {
	if subjectKey == "" {
		return domain.ExperimentCohort{}, false, nil
	}
	if err := resolver.init(ctx); err != nil {
		return domain.ExperimentCohort{}, false, err
	}
	result, err := resolver.qs.Query(ctx, qCohortsForAgent, tenantID, agentID)
	if err != nil {
		return domain.ExperimentCohort{}, false, fmt.Errorf("list experiment cohorts: %w", err)
	}
	for _, row := range result.Rows {
		cohort := domain.ExperimentCohort{
			TenantID: tenantID, AgentID: agentID, ExperimentID: common.AsString(row[0]),
			Version: common.AsString(row[1]), Percentage: int(common.AsInt64(row[2])), Salt: common.AsString(row[3]),
		}
		if CohortSelected(cohort, tenantID, subjectKey) {
			return cohort, true, nil
		}
	}
	return domain.ExperimentCohort{}, false, nil
}

// CohortSelected is the stable-hash membership rule shared by every cohort resolver.
func CohortSelected(cohort domain.ExperimentCohort, tenantID int64, subjectKey string) bool {
	if cohort.Percentage <= 0 || subjectKey == "" {
		return false
	}
	return StableBucket(strconv.FormatInt(tenantID, 10), cohort.AgentID, cohort.ExperimentID, cohort.Salt, subjectKey) < cohort.Percentage
}

// TableAgentDeploymentStore is the keel-backed AgentDeploymentStore over agent_deployment.
type TableAgentDeploymentStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.AgentDeploymentStore = (*TableAgentDeploymentStore)(nil)

func (store *TableAgentDeploymentStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("agent deployment store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, versionPinQueries) })
	if store.qs == nil {
		return fmt.Errorf("agent deployment store: query service is required")
	}
	return nil
}

func (store *TableAgentDeploymentStore) Get(ctx context.Context, tenantID int64, agentID string) (domain.AgentDeployment, error) {
	if err := store.init(ctx); err != nil {
		return domain.AgentDeployment{}, err
	}
	result, err := store.qs.Query(ctx, qDeploymentGet, tenantID, agentID)
	if err != nil {
		return domain.AgentDeployment{}, fmt.Errorf("get agent deployment: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentDeployment{}, fmt.Errorf("%w: deployment for agent %s", domain.ErrNotFound, agentID)
	}
	row := result.Rows[0]
	return domain.AgentDeployment{
		TenantID: tenantID, AgentID: agentID, StableVersion: common.AsString(row[0]),
		CanaryVersion: common.AsString(row[1]), CanaryPercentage: int(common.AsInt64(row[2])),
	}, nil
}

func (store *TableAgentDeploymentStore) SetCanary(ctx context.Context, tenantID int64, agentID, version string, percentage int) error {
	if percentage < 1 || percentage > 100 || strings.TrimSpace(version) == "" {
		return fmt.Errorf("%w: canary needs a version and a percentage between 1 and 100", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	result, err := store.qs.Query(ctx, qDeploymentCanary, version, percentage, tenantID, agentID, version)
	if err != nil {
		return fmt.Errorf("set canary: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: agent %s has no deployment or %s is already stable", domain.ErrConflict, agentID, version)
	}
	return nil
}

func (store *TableAgentDeploymentStore) Promote(ctx context.Context, tenantID int64, agentID, version string) error {
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("%w: promote needs a version", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	result, err := store.qs.Query(ctx, qDeploymentPromote, version, tenantID, agentID)
	if err != nil {
		return fmt.Errorf("promote version: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: deployment for agent %s", domain.ErrNotFound, agentID)
	}
	return nil
}

func (store *TableAgentDeploymentStore) RestorePrevious(ctx context.Context, tenantID int64, agentID string) (string, error) {
	if err := store.init(ctx); err != nil {
		return "", err
	}
	cleared, err := store.qs.Query(ctx, qDeploymentClear, tenantID, agentID)
	if err != nil {
		return "", fmt.Errorf("clear canary: %w", err)
	}
	if len(cleared.Rows) > 0 {
		return common.AsString(cleared.Rows[0][0]), nil
	}
	current, err := store.Get(ctx, tenantID, agentID)
	if err != nil {
		return "", err
	}
	previous, err := store.qs.Query(ctx, qDeploymentPrevious, tenantID, agentID)
	if err != nil {
		return "", fmt.Errorf("find previous version: %w", err)
	}
	if len(previous.Rows) == 0 {
		return "", fmt.Errorf("%w: agent %s has no version before %s", domain.ErrNotFound, agentID, current.StableVersion)
	}
	version := common.AsString(previous.Rows[0][0])
	restored, err := store.qs.Query(ctx, qDeploymentRestore, version, tenantID, agentID, current.StableVersion)
	if err != nil {
		return "", fmt.Errorf("restore previous version: %w", err)
	}
	if len(restored.Rows) == 0 {
		return "", fmt.Errorf("%w: deployment for agent %s changed during rollback", domain.ErrRevisionConflict, agentID)
	}
	return version, nil
}

// PinnedTrafficManager resolves agent versions by precedence: compliance pin,
// approved tenant pin, experiment cohort, then the deployment's canary/stable
// split. Every resolution is audited with the winning rule.
type PinnedTrafficManager struct {
	Pins        contract.VersionPinStore
	Cohorts     contract.ExperimentCohortResolver
	Deployments contract.AgentDeploymentStore
	Audit       contract.AuditSink
	// Region scopes regional pins; a pin with another region is ignored.
	Region string
	// PolicyVersion and IndexVersion, when set, must appear in a pin's
	// compatibility lists or the pinned request is rejected instead of drifting.
	PolicyVersion string
	IndexVersion  string
	Now           func() time.Time
}

var (
	_ contract.AgentVersionTrafficManager      = (*PinnedTrafficManager)(nil)
	_ contract.AgentVersionResolutionExplainer = (*PinnedTrafficManager)(nil)
)

func (manager *PinnedTrafficManager) now() time.Time {
	if manager.Now != nil {
		return manager.Now()
	}
	return time.Now()
}

func (manager *PinnedTrafficManager) validate() error {
	if manager.Deployments == nil {
		return fmt.Errorf("pinned traffic manager: deployment store is required")
	}
	return nil
}

func (manager *PinnedTrafficManager) ResolveVersion(ctx context.Context, tenantID int64, agentID, conversationID string) (string, error) {
	resolution, err := manager.ExplainVersion(ctx, tenantID, agentID, conversationID)
	return resolution.Version, err
}

func (manager *PinnedTrafficManager) ExplainVersion(ctx context.Context, tenantID int64, agentID, conversationID string) (domain.AgentVersionResolution, error) {
	if err := manager.validate(); err != nil {
		return domain.AgentVersionResolution{}, err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" {
		return domain.AgentVersionResolution{}, fmt.Errorf("%w: tenant and agent are required", domain.ErrValidation)
	}
	resolution, err := manager.resolve(ctx, tenantID, agentID, conversationID)
	if err != nil {
		return domain.AgentVersionResolution{}, err
	}
	if manager.Audit != nil {
		if err := manager.auditResolution(ctx, tenantID, agentID, conversationID, resolution); err != nil {
			return domain.AgentVersionResolution{}, err
		}
	}
	return resolution, nil
}

func (manager *PinnedTrafficManager) resolve(ctx context.Context, tenantID int64, agentID, conversationID string) (domain.AgentVersionResolution, error) {
	if manager.Pins != nil {
		pins, err := manager.Pins.Active(ctx, tenantID, agentID, manager.now())
		if err != nil {
			return domain.AgentVersionResolution{}, err
		}
		for _, pin := range pins {
			if pin.Region != "" && pin.Region != manager.Region {
				continue
			}
			if pin.Scope == domain.PinScopeTenant && strings.TrimSpace(pin.ApprovedBy) == "" {
				continue
			}
			if err := manager.compatible(pin); err != nil {
				return domain.AgentVersionResolution{}, err
			}
			source := domain.VersionFromTenantPin
			if pin.Scope == domain.PinScopeCompliance {
				source = domain.VersionFromCompliancePin
			}
			return domain.AgentVersionResolution{Version: pin.Version, Source: source, PinID: pin.ID}, nil
		}
	}
	if manager.Cohorts != nil {
		cohort, selected, err := manager.Cohorts.Resolve(ctx, tenantID, agentID, conversationID)
		if err != nil {
			return domain.AgentVersionResolution{}, err
		}
		if selected {
			return domain.AgentVersionResolution{Version: cohort.Version, Source: domain.VersionFromCohort}, nil
		}
	}
	deployment, err := manager.Deployments.Get(ctx, tenantID, agentID)
	if err != nil {
		return domain.AgentVersionResolution{}, err
	}
	if deployment.CanaryVersion != "" && CanarySelected(tenantID, agentID, conversationID, deployment.CanaryPercentage) {
		return domain.AgentVersionResolution{Version: deployment.CanaryVersion, Source: domain.VersionFromCanary}, nil
	}
	return domain.AgentVersionResolution{Version: deployment.StableVersion, Source: domain.VersionFromStable}, nil
}

func (manager *PinnedTrafficManager) compatible(pin domain.VersionPin) error {
	if manager.PolicyVersion != "" && len(pin.CompatiblePolicyVersions) > 0 && !slices.Contains(pin.CompatiblePolicyVersions, manager.PolicyVersion) {
		return fmt.Errorf("%w: pin %d on %s is incompatible with policy %s", domain.ErrConflict, pin.ID, pin.Version, manager.PolicyVersion)
	}
	if manager.IndexVersion != "" && len(pin.CompatibleIndexVersions) > 0 && !slices.Contains(pin.CompatibleIndexVersions, manager.IndexVersion) {
		return fmt.Errorf("%w: pin %d on %s is incompatible with index %s", domain.ErrConflict, pin.ID, pin.Version, manager.IndexVersion)
	}
	return nil
}

func (manager *PinnedTrafficManager) auditResolution(ctx context.Context, tenantID int64, agentID, conversationID string, resolution domain.AgentVersionResolution) error {
	payload, err := json.Marshal(struct {
		AgentID        string               `json:"agent_id"`
		ConversationID string               `json:"conversation_id,omitempty"`
		Version        string               `json:"version"`
		Source         domain.VersionSource `json:"source"`
		PinID          int64                `json:"pin_id,omitempty"`
	}{agentID, conversationID, resolution.Version, resolution.Source, resolution.PinID})
	if err != nil {
		return fmt.Errorf("encode version audit: %w", err)
	}
	if err := manager.Audit.Record(ctx, domain.AuditEvent{TenantID: tenantID, Category: "agent_version.resolved", Payload: payload, OccurredAt: manager.now()}); err != nil {
		return fmt.Errorf("audit version resolution: %w", err)
	}
	return nil
}

func (manager *PinnedTrafficManager) SetCanary(ctx context.Context, tenantID int64, agentID, version string, percentage int) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.Deployments.SetCanary(ctx, tenantID, agentID, version, percentage); err != nil {
		return err
	}
	return manager.auditChange(ctx, tenantID, agentID, "agent_version.canary", version, percentage)
}

func (manager *PinnedTrafficManager) Promote(ctx context.Context, tenantID int64, agentID, version string) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if err := manager.Deployments.Promote(ctx, tenantID, agentID, version); err != nil {
		return err
	}
	return manager.auditChange(ctx, tenantID, agentID, "agent_version.promoted", version, 100)
}

// Rollback changes only new conversation assignments; live conversations stay on their persisted version.
func (manager *PinnedTrafficManager) Rollback(ctx context.Context, tenantID int64, agentID string) error {
	if err := manager.validate(); err != nil {
		return err
	}
	version, err := manager.Deployments.RestorePrevious(ctx, tenantID, agentID)
	if err != nil {
		return err
	}
	return manager.auditChange(ctx, tenantID, agentID, "agent_version.rolled_back", version, 100)
}

func (manager *PinnedTrafficManager) auditChange(ctx context.Context, tenantID int64, agentID, category, version string, percentage int) error {
	if manager.Audit == nil {
		return nil
	}
	payload, err := json.Marshal(struct {
		AgentID    string `json:"agent_id"`
		Version    string `json:"version"`
		Percentage int    `json:"percentage"`
	}{agentID, version, percentage})
	if err != nil {
		return fmt.Errorf("encode traffic audit: %w", err)
	}
	if err := manager.Audit.Record(ctx, domain.AuditEvent{TenantID: tenantID, Category: category, Payload: payload, OccurredAt: manager.now()}); err != nil {
		return fmt.Errorf("audit traffic change: %w", err)
	}
	return nil
}

// PinAwareGarbageCollector answers whether an agent version may be dropped:
// never while deployed, pinned, in a cohort, or held by an open conversation.
type PinAwareGarbageCollector struct {
	DB  keelport.DatabaseRepository
	Now func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

func (collector *PinAwareGarbageCollector) init(ctx context.Context) error {
	if collector.DB == nil {
		return fmt.Errorf("pin-aware garbage collector: database is required")
	}
	collector.once.Do(func() { collector.qs = collector.DB.GetQueryService(ctx, versionPinQueries) })
	if collector.qs == nil {
		return fmt.Errorf("pin-aware garbage collector: query service is required")
	}
	return nil
}

// Collectable reports whether nothing live references the version.
func (collector *PinAwareGarbageCollector) Collectable(ctx context.Context, tenantID int64, agentID, version string) (bool, error) {
	if err := collector.init(ctx); err != nil {
		return false, err
	}
	now := time.Now()
	if collector.Now != nil {
		now = collector.Now()
	}
	result, err := collector.qs.Query(ctx, qVersionRetained,
		tenantID, agentID, version, version,
		tenantID, agentID, version, now,
		tenantID, agentID, version,
		tenantID, agentID, version)
	if err != nil {
		return false, fmt.Errorf("check version retention: %w", err)
	}
	if len(result.Rows) == 0 {
		return false, fmt.Errorf("check version retention: no row")
	}
	return !common.AsBool(result.Rows[0][0]), nil
}

func encodeList(values []string) any {
	if len(values) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func decodeList(value any) ([]string, error) {
	text := common.AsString(value)
	if text == "" {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(text), &values); err != nil {
		return nil, err
	}
	return values, nil
}
