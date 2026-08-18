package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	qTypePublish      = "scout_agent_type_publish"
	qTypePut          = "scout_agent_type_put"
	qTypeCapability   = "scout_agent_type_capability"
	qTypeGet          = "scout_agent_type_get"
	qTypeLatest       = "scout_agent_type_latest"
	qTypeInstances    = "scout_agent_type_instances"
	qTypePackages     = "scout_agent_type_packages"
	qPackagePut       = "scout_capability_package_put"
	qPackageGet       = "scout_capability_package_get"
	qInstanceProfile  = "scout_agent_type_instance_profile"
	qInstanceBinding  = "scout_agent_type_instance_binding"
	qAgentStateGet    = "scout_agent_state_get"
	qAgentStateSet    = "scout_agent_state_set"
	qQuarantinePut    = "scout_agent_quarantine_put"
	qQuarantineLift   = "scout_agent_quarantine_lift"
	qQuarantineActive = "scout_agent_quarantine_active"
)

var agentTypeQueries = map[string]string{
	qTypePut: `
INSERT INTO agent_type (tenant_id, agent_type_id, display_name, description)
VALUES (?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_type_id) DO UPDATE
   SET display_name = EXCLUDED.display_name, description = EXCLUDED.description`,
	qTypePublish: `
INSERT INTO agent_type_version (tenant_id, agent_type_id, type_version, definition, definition_digest, change_summary, published_by)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
	qTypeCapability: `
INSERT INTO agent_type_capability (tenant_id, agent_type_id, type_version, package_id, package_version, is_required)
VALUES (?, ?, ?, ?, ?, ?)`,
	qTypeGet: `
SELECT definition, definition_digest, change_summary, published_by, published_at
  FROM agent_type_version
 WHERE tenant_id = ? AND agent_type_id = ? AND type_version = ?`,
	qTypeLatest: `
SELECT type_version, definition, definition_digest, change_summary, published_by, published_at
  FROM agent_type_version
 WHERE tenant_id = ? AND agent_type_id = ?
 ORDER BY published_at DESC, type_version DESC
 LIMIT 1`,
	qTypeInstances: `
SELECT p.agent_id, p.agent_type_version
  FROM agent_profile p
 WHERE p.tenant_id = ? AND p.agent_type_id = ? AND p.agent_type_version IS NOT NULL`,
	qTypePackages: `
SELECT package_id, package_version, is_required
  FROM agent_type_capability
 WHERE tenant_id = ? AND agent_type_id = ? AND type_version = ?`,
	qPackagePut: `
INSERT INTO agent_capability_package (tenant_id, package_id, package_version, display_name, payload, payload_digest)
VALUES (?, ?, ?, ?, ?, ?)`,
	qPackageGet: `
SELECT display_name, payload, payload_digest, created_at
  FROM agent_capability_package
 WHERE tenant_id = ? AND package_id = ? AND package_version = ?`,
	qInstanceProfile: `
INSERT INTO agent_profile (tenant_id, agent_id, agent_type_id, agent_type_version, display_name, state_code)
VALUES (?, ?, ?, ?, ?, 'draft')`,
	qInstanceBinding: `
INSERT INTO config_scope_binding (tenant_id, scope_id, resource_kind_code, resource_id, resource_version,
                            merge_mode_code, sealed, resource_value, resource_value_digest, bound_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qAgentStateGet: `SELECT state_code FROM agent_profile WHERE tenant_id = ? AND agent_id = ?`,
	qAgentStateSet: `
UPDATE agent_profile
   SET state_code = ?, state_reason = ?, state_changed_by = ?, state_changed_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ? AND state_code = ?
RETURNING state_code`,
	qQuarantinePut: `
INSERT INTO agent_version_quarantine (tenant_id, agent_id, agent_version, reason, actor_kind, actor_id)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id, agent_version) DO UPDATE
   SET reason = EXCLUDED.reason, actor_kind = EXCLUDED.actor_kind, actor_id = EXCLUDED.actor_id,
       quarantined_at = CURRENT_TIMESTAMP, lifted_at = NULL`,
	qQuarantineLift: `
UPDATE agent_version_quarantine SET lifted_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND lifted_at IS NULL
RETURNING agent_version`,
	qQuarantineActive: `
SELECT 1 FROM agent_version_quarantine
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND lifted_at IS NULL`,
}

// PutType creates or updates the mutable label of a reusable type. Published
// versions remain immutable and reference this identity.
func (s *AgentTypeStore) PutType(ctx context.Context, agentType domain.AgentType) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if agentType.TenantID <= 0 || strings.TrimSpace(agentType.AgentTypeID) == "" || strings.TrimSpace(agentType.DisplayName) == "" {
		return fmt.Errorf("%w: an agent type needs tenant, id, and display name", domain.ErrValidation)
	}
	if _, err := s.qs.Query(context.WithoutCancel(ctx), qTypePut, agentType.TenantID, agentType.AgentTypeID,
		agentType.DisplayName, nullableText(agentType.Description)); err != nil {
		return fmt.Errorf("put agent type: %w", err)
	}
	return nil
}

// PutPackage stores an immutable capability package.
func (s *AgentTypeStore) PutPackage(ctx context.Context, pkg domain.CapabilityPackage) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if pkg.TenantID <= 0 || strings.TrimSpace(pkg.PackageID) == "" || strings.TrimSpace(pkg.PackageVersion) == "" || strings.TrimSpace(pkg.DisplayName) == "" {
		return fmt.Errorf("%w: a capability package needs tenant, id, version, and display name", domain.ErrValidation)
	}
	payload, err := json.Marshal(pkg.Resources)
	if err != nil {
		return fmt.Errorf("encode capability package: %w", err)
	}
	digest := valueDigest(payload)
	if _, err := s.qs.Query(context.WithoutCancel(ctx), qPackagePut, pkg.TenantID, pkg.PackageID,
		pkg.PackageVersion, pkg.DisplayName, string(payload), digest); err != nil {
		return fmt.Errorf("put capability package: %w", err)
	}
	return nil
}

// Get returns one immutable capability package.
func (s *AgentTypeStore) GetPackage(ctx context.Context, tenantID int64, packageID, packageVersion string) (domain.CapabilityPackage, error) {
	if err := s.init(ctx); err != nil {
		return domain.CapabilityPackage{}, err
	}
	result, err := s.qs.Query(ctx, qPackageGet, tenantID, packageID, packageVersion)
	if err != nil {
		return domain.CapabilityPackage{}, fmt.Errorf("get capability package: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.CapabilityPackage{}, fmt.Errorf("%w: capability package %s@%s", domain.ErrNotFound, packageID, packageVersion)
	}
	row := result.Rows[0]
	payload := []byte(common.AsString(row[1]))
	if digest := valueDigest(payload); digest != common.AsString(row[2]) {
		return domain.CapabilityPackage{}, fmt.Errorf("%w: capability package %s@%s digest mismatch", domain.ErrValidation, packageID, packageVersion)
	}
	pkg := domain.CapabilityPackage{
		TenantID: tenantID, PackageID: packageID, PackageVersion: packageVersion,
		DisplayName: common.AsString(row[0]), Digest: common.AsString(row[2]), CreatedAt: common.AsTime(row[3]),
	}
	if err := json.Unmarshal(payload, &pkg.Resources); err != nil {
		return domain.CapabilityPackage{}, fmt.Errorf("decode capability package: %w", err)
	}
	return pkg, nil
}

// agentStateTransitions is the legal state machine. Retirement is terminal, and
// nothing reaches active without passing through draft or suspension.
var agentStateTransitions = map[domain.AgentState][]domain.AgentState{
	domain.AgentStateDraft:     {domain.AgentStateActive, domain.AgentStateRetired},
	domain.AgentStateActive:    {domain.AgentStateSuspended, domain.AgentStateDraining},
	domain.AgentStateSuspended: {domain.AgentStateActive, domain.AgentStateDraining, domain.AgentStateRetired},
	domain.AgentStateDraining:  {domain.AgentStateSuspended, domain.AgentStateRetired},
	domain.AgentStateRetired:   nil,
}

// AgentTypeStore publishes type versions, instantiates agents from them, owns
// the agent state machine, and quarantines individual versions.
type AgentTypeStore struct {
	DB       keelport.DatabaseRepository
	Packages contract.CapabilityPackageRepository
	// Checker is optional; when set an instantiation overlay is narrowing-checked
	// against the package values it overrides, so an instance is never born broader.
	Checker contract.NarrowingChecker
	// Audit is optional; when set every state change and quarantine becomes evidence.
	Audit contract.AuditSink
	Now   func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

func (s *AgentTypeStore) init(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("agent type store: database is required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, agentTypeQueries) })
	if s.qs == nil {
		return fmt.Errorf("agent type store: query service is required")
	}
	return nil
}

func (s *AgentTypeStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Publish stores an immutable type version and its capability requirements.
func (s *AgentTypeStore) Publish(ctx context.Context, version domain.AgentTypeVersion) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if version.TenantID <= 0 || strings.TrimSpace(version.AgentTypeID) == "" || strings.TrimSpace(version.TypeVersion) == "" {
		return fmt.Errorf("%w: a type version needs tenant, type, and version", domain.ErrValidation)
	}
	seenPackages := make(map[string]struct{}, len(version.Packages))
	for _, pkg := range version.Packages {
		if strings.TrimSpace(pkg.PackageID) == "" || strings.TrimSpace(pkg.PackageVersion) == "" {
			return fmt.Errorf("%w: every capability reference needs an id and version", domain.ErrValidation)
		}
		key := pkg.PackageID + "\x1f" + pkg.PackageVersion
		if _, duplicate := seenPackages[key]; duplicate {
			return fmt.Errorf("%w: duplicate capability package %s@%s", domain.ErrValidation, pkg.PackageID, pkg.PackageVersion)
		}
		seenPackages[key] = struct{}{}
	}
	definition, err := json.Marshal(version)
	if err != nil {
		return fmt.Errorf("encode type version: %w", err)
	}
	digest := TypeVersionDigest(version)
	ctx = context.WithoutCancel(ctx)
	tx, err := s.DB.BeginTx(ctx, agentTypeQueries)
	if err != nil {
		return fmt.Errorf("publish type: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Query(ctx, qTypePublish, version.TenantID, version.AgentTypeID, version.TypeVersion,
		string(definition), digest, nullableText(version.Change), nullableUser(version.PublishedBy)); err != nil {
		return fmt.Errorf("publish type version: %w", err)
	}
	for _, pkg := range version.Packages {
		if _, err = tx.Query(ctx, qTypeCapability, version.TenantID, version.AgentTypeID, version.TypeVersion,
			pkg.PackageID, pkg.PackageVersion, pkg.Required); err != nil {
			return fmt.Errorf("bind capability %q: %w", pkg.PackageID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("publish type: commit: %w", err)
	}
	committed = true
	return nil
}

// Get returns one published type version with its capability requirements.
func (s *AgentTypeStore) Get(ctx context.Context, tenantID int64, ref domain.AgentTypeRef) (domain.AgentTypeVersion, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTypeVersion{}, err
	}
	result, err := s.qs.Query(ctx, qTypeGet, tenantID, ref.AgentTypeID, ref.TypeVersion)
	if err != nil {
		return domain.AgentTypeVersion{}, fmt.Errorf("get type version: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentTypeVersion{}, fmt.Errorf("%w: type %s@%s", domain.ErrNotFound, ref.AgentTypeID, ref.TypeVersion)
	}
	row := result.Rows[0]
	return s.hydrate(ctx, tenantID, ref.AgentTypeID, ref.TypeVersion, row[0], row[1], row[2], row[3], row[4])
}

// Latest returns the newest published version of a type.
func (s *AgentTypeStore) Latest(ctx context.Context, tenantID int64, agentTypeID string) (domain.AgentTypeVersion, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTypeVersion{}, err
	}
	result, err := s.qs.Query(ctx, qTypeLatest, tenantID, agentTypeID)
	if err != nil {
		return domain.AgentTypeVersion{}, fmt.Errorf("get latest type version: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentTypeVersion{}, fmt.Errorf("%w: type %q has no published version", domain.ErrNotFound, agentTypeID)
	}
	row := result.Rows[0]
	return s.hydrate(ctx, tenantID, agentTypeID, common.AsString(row[0]), row[1], row[2], row[3], row[4], row[5])
}

func (s *AgentTypeStore) hydrate(ctx context.Context, tenantID int64, typeID, typeVersion string, definition, digest, change, publishedBy, publishedAt any) (domain.AgentTypeVersion, error) {
	var version domain.AgentTypeVersion
	if err := json.Unmarshal([]byte(common.AsString(definition)), &version); err != nil {
		return domain.AgentTypeVersion{}, fmt.Errorf("decode type version: %w", err)
	}
	version.TenantID, version.AgentTypeID, version.TypeVersion = tenantID, typeID, typeVersion
	version.Digest, version.Change = common.AsString(digest), common.AsString(change)
	version.PublishedAt = common.AsTime(publishedAt)
	if by := common.AsInt64(publishedBy); by > 0 {
		version.PublishedBy = &by
	}
	packages, err := s.qs.Query(ctx, qTypePackages, tenantID, typeID, typeVersion)
	if err != nil {
		return domain.AgentTypeVersion{}, fmt.Errorf("load type capabilities: %w", err)
	}
	version.Packages = make([]domain.CapabilityRef, 0, len(packages.Rows))
	for _, row := range packages.Rows {
		version.Packages = append(version.Packages, domain.CapabilityRef{
			PackageID: common.AsString(row[0]), PackageVersion: common.AsString(row[1]), Required: common.AsBool(row[2]),
		})
	}
	return version, nil
}

// Instances lists agents of a type with the version each currently resolves to.
func (s *AgentTypeStore) Instances(ctx context.Context, tenantID int64, agentTypeID string) (map[string]domain.AgentTypeRef, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	result, err := s.qs.Query(ctx, qTypeInstances, tenantID, agentTypeID)
	if err != nil {
		return nil, fmt.Errorf("list type instances: %w", err)
	}
	instances := make(map[string]domain.AgentTypeRef, len(result.Rows))
	for _, row := range result.Rows {
		instances[common.AsString(row[0])] = domain.AgentTypeRef{AgentTypeID: agentTypeID, TypeVersion: common.AsString(row[1])}
	}
	return instances, nil
}

// Instantiate creates an agent pinned to a type version and expands the type's
// capability packages into bindings at the instance scope. The overlay is
// narrowing-checked against those values, so an instance is never born broader
// than the type it came from.
func (s *AgentTypeStore) Instantiate(ctx context.Context, request domain.InstantiateRequest) (domain.AgentTypeRef, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTypeRef{}, err
	}
	if request.TenantID <= 0 || strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.ScopeID) == "" {
		return domain.AgentTypeRef{}, fmt.Errorf("%w: instantiation needs tenant, agent, and scope", domain.ErrValidation)
	}
	version, err := s.Get(ctx, request.TenantID, request.Type)
	if err != nil {
		return domain.AgentTypeRef{}, err
	}
	bindings, err := s.expand(ctx, request.TenantID, version)
	if err != nil {
		return domain.AgentTypeRef{}, err
	}
	bindings, err = s.applyOverlay(ctx, bindings, request.Overlay)
	if err != nil {
		return domain.AgentTypeRef{}, err
	}

	ctx = context.WithoutCancel(ctx)
	tx, err := s.DB.BeginTx(ctx, agentTypeQueries)
	if err != nil {
		return domain.AgentTypeRef{}, fmt.Errorf("instantiate: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Query(ctx, qInstanceProfile, request.TenantID, request.AgentID, request.Type.AgentTypeID,
		request.Type.TypeVersion, request.DisplayName); err != nil {
		return domain.AgentTypeRef{}, fmt.Errorf("create instance profile: %w", err)
	}
	for _, binding := range bindings {
		if _, err = tx.Query(ctx, qInstanceBinding, request.TenantID, request.ScopeID,
			string(binding.ResourceKind), binding.ResourceID, binding.ResourceVersion,
			string(mergeModeOrReplace(binding.MergeMode)), binding.Sealed,
			string(binding.Value), valueDigest(binding.Value), nullableUser(request.CreatedBy)); err != nil {
			return domain.AgentTypeRef{}, fmt.Errorf("bind %s %q: %w", binding.ResourceKind, binding.ResourceID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentTypeRef{}, fmt.Errorf("instantiate: commit: %w", err)
	}
	committed = true
	return request.Type, nil
}

func (s *AgentTypeStore) expand(ctx context.Context, tenantID int64, version domain.AgentTypeVersion) ([]domain.ScopedBinding, error) {
	if len(version.Packages) == 0 {
		return nil, nil
	}
	packages := s.Packages
	if packages == nil {
		packages = s
	}
	var bindings []domain.ScopedBinding
	seenResources := make(map[resourceKey]struct{})
	for _, ref := range version.Packages {
		pkg, err := packages.GetPackage(ctx, tenantID, ref.PackageID, ref.PackageVersion)
		if err != nil {
			if !ref.Required {
				continue
			}
			return nil, fmt.Errorf("required capability %q: %w", ref.PackageID, err)
		}
		for _, resource := range pkg.Resources {
			key := resourceKey{resource.ResourceKind, resource.ResourceID}
			if _, duplicate := seenResources[key]; duplicate {
				return nil, fmt.Errorf("%w: capability packages bind %s %q more than once", domain.ErrValidation, resource.ResourceKind, resource.ResourceID)
			}
			if strings.TrimSpace(string(resource.ResourceKind)) == "" || strings.TrimSpace(resource.ResourceID) == "" {
				return nil, fmt.Errorf("%w: capability package %q contains an unnamed resource", domain.ErrValidation, ref.PackageID)
			}
			seenResources[key] = struct{}{}
			bindings = append(bindings, domain.ScopedBinding{
				TenantID: tenantID, ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
				ResourceVersion: ref.PackageVersion, MergeMode: domain.MergeReplace, Value: resource.Value,
			})
		}
	}
	return bindings, nil
}

func (s *AgentTypeStore) applyOverlay(ctx context.Context, inherited, overlay []domain.ScopedBinding) ([]domain.ScopedBinding, error) {
	if len(overlay) == 0 {
		return inherited, nil
	}
	if s.Checker == nil {
		return nil, fmt.Errorf("agent type store: a narrowing checker is required for an instance overlay")
	}
	positions := make(map[resourceKey]int, len(inherited))
	result := append([]domain.ScopedBinding(nil), inherited...)
	for index, binding := range inherited {
		positions[resourceKey{binding.ResourceKind, binding.ResourceID}] = index
	}
	seen := make(map[resourceKey]struct{}, len(overlay))
	for _, binding := range overlay {
		key := resourceKey{binding.ResourceKind, binding.ResourceID}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate overlay for %s %q", domain.ErrValidation, binding.ResourceKind, binding.ResourceID)
		}
		seen[key] = struct{}{}
		if binding.MergeMode != "" && binding.MergeMode != domain.MergeReplace {
			return nil, fmt.Errorf("%w: instance overlays must provide their complete replacement value", domain.ErrValidation)
		}
		position, found := positions[key]
		if !found {
			return nil, fmt.Errorf("%w: overlay adds ungranted %s %q", domain.ErrAuthorityExceeded, binding.ResourceKind, binding.ResourceID)
		}
		if strings.TrimSpace(binding.ResourceVersion) == "" {
			return nil, fmt.Errorf("%w: overlay for %s %q needs a resource version", domain.ErrValidation, binding.ResourceKind, binding.ResourceID)
		}
		if err := s.Checker.CheckNarrowing(ctx, binding.ResourceKind, result[position].Value, binding.Value); err != nil {
			return nil, err
		}
		result[position] = binding
	}
	return result, nil
}

func mergeModeOrReplace(mode domain.MergeMode) domain.MergeMode {
	if mode == "" {
		return domain.MergeReplace
	}
	return mode
}

// Conformance reports instances that no longer satisfy the type's latest
// version. It never upgrades one: the pinned version is the contract.
func (s *AgentTypeStore) Conformance(ctx context.Context, tenantID int64, agentTypeID string) ([]domain.ConformanceFinding, error) {
	latest, err := s.Latest(ctx, tenantID, agentTypeID)
	if err != nil {
		return nil, err
	}
	instances, err := s.Instances(ctx, tenantID, agentTypeID)
	if err != nil {
		return nil, err
	}
	current := domain.AgentTypeRef{AgentTypeID: agentTypeID, TypeVersion: latest.TypeVersion}
	findings := make([]domain.ConformanceFinding, 0)
	for agentID, pinned := range instances {
		if pinned.TypeVersion == latest.TypeVersion {
			continue
		}
		pinnedVersion, err := s.Get(ctx, tenantID, pinned)
		if err != nil {
			findings = append(findings, domain.ConformanceFinding{
				AgentID: agentID, PinnedType: pinned, CurrentType: current, Reason: "pinned type version is gone",
			})
			continue
		}
		findings = append(findings, domain.ConformanceFinding{
			AgentID: agentID, PinnedType: pinned, CurrentType: current,
			Missing: missingPackages(pinnedVersion.Packages, latest.Packages),
			Reason:  "pinned to an older type version",
		})
	}
	return findings, nil
}

// Transition moves an agent between states; an illegal move is a conflict.
func (s *AgentTypeStore) Transition(ctx context.Context, change domain.AgentStateChange) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if change.TenantID <= 0 || strings.TrimSpace(change.AgentID) == "" || strings.TrimSpace(change.Reason) == "" ||
		change.Actor.Kind == "" || strings.TrimSpace(change.Actor.ID) == "" || !knownAgentState(change.To) {
		return fmt.Errorf("%w: a state change needs tenant, agent, target state, reason, and actor", domain.ErrValidation)
	}
	from, err := s.State(ctx, change.TenantID, change.AgentID)
	if err != nil {
		return err
	}
	if !allowedTransition(from, change.To) {
		return fmt.Errorf("%w: agent %q cannot move from %s to %s", domain.ErrConflict, change.AgentID, from, change.To)
	}
	ctx = context.WithoutCancel(ctx)
	updated, err := s.qs.Query(ctx, qAgentStateSet, string(change.To), change.Reason, change.Actor.ID,
		change.TenantID, change.AgentID, string(from))
	if err != nil {
		return fmt.Errorf("set agent state: %w", err)
	}
	if len(updated.Rows) == 0 {
		return fmt.Errorf("%w: agent %q changed state concurrently", domain.ErrConflict, change.AgentID)
	}
	return s.record(ctx, change.TenantID, change.Actor, domain.DecisionCategoryStateChange,
		string(change.To), change.AgentID, change.Reason)
}

// State returns an agent's current lifecycle state.
func (s *AgentTypeStore) State(ctx context.Context, tenantID int64, agentID string) (domain.AgentState, error) {
	if err := s.init(ctx); err != nil {
		return "", err
	}
	result, err := s.qs.Query(ctx, qAgentStateGet, tenantID, agentID)
	if err != nil {
		return "", fmt.Errorf("get agent state: %w", err)
	}
	if len(result.Rows) == 0 {
		return "", fmt.Errorf("%w: agent %q", domain.ErrNotFound, agentID)
	}
	return domain.AgentState(common.AsString(result.Rows[0][0])), nil
}

// Quarantine withdraws one agent version from all traffic.
func (s *AgentTypeStore) Quarantine(ctx context.Context, quarantine domain.AgentQuarantine) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if err := validateQuarantine(quarantine); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := s.qs.Query(ctx, qQuarantinePut, quarantine.TenantID, quarantine.AgentID, quarantine.AgentVersion,
		quarantine.Reason, string(quarantine.Actor.Kind), quarantine.Actor.ID); err != nil {
		return fmt.Errorf("quarantine agent version: %w", err)
	}
	return s.record(ctx, quarantine.TenantID, quarantine.Actor, domain.DecisionCategoryStateChange,
		"quarantine", quarantine.AgentID+"@"+quarantine.AgentVersion, quarantine.Reason)
}

// Lift returns a quarantined version to normal selection.
func (s *AgentTypeStore) Lift(ctx context.Context, quarantine domain.AgentQuarantine) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if err := validateQuarantine(quarantine); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	lifted, err := s.qs.Query(ctx, qQuarantineLift, quarantine.TenantID, quarantine.AgentID, quarantine.AgentVersion)
	if err != nil {
		return fmt.Errorf("lift quarantine: %w", err)
	}
	if len(lifted.Rows) == 0 {
		return fmt.Errorf("%w: %s@%s is not quarantined", domain.ErrConflict, quarantine.AgentID, quarantine.AgentVersion)
	}
	return s.record(ctx, quarantine.TenantID, quarantine.Actor, domain.DecisionCategoryStateChange,
		"lift_quarantine", quarantine.AgentID+"@"+quarantine.AgentVersion, quarantine.Reason)
}

// Quarantined reports whether a version is withdrawn. A quarantine overrides
// every pin and deployment pointer rather than editing them.
func (s *AgentTypeStore) Quarantined(ctx context.Context, tenantID int64, agentID, agentVersion string) (bool, error) {
	if err := s.init(ctx); err != nil {
		return false, err
	}
	result, err := s.qs.Query(ctx, qQuarantineActive, tenantID, agentID, agentVersion)
	if err != nil {
		return false, fmt.Errorf("check quarantine: %w", err)
	}
	return len(result.Rows) > 0, nil
}

func (s *AgentTypeStore) record(ctx context.Context, tenantID int64, actor domain.PrincipalRef, category, action, resource, reason string) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: tenantID, Principal: actor, Category: category, Action: action,
		Resource: resource, Outcome: domain.DecisionAllow, Reason: reason, OccurredAt: s.now(),
	})
}

func allowedTransition(from, to domain.AgentState) bool {
	if from == to {
		return false
	}
	for _, candidate := range agentStateTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

func knownAgentState(state domain.AgentState) bool {
	_, known := agentStateTransitions[state]
	return known
}

func validateQuarantine(quarantine domain.AgentQuarantine) error {
	if quarantine.TenantID <= 0 || strings.TrimSpace(quarantine.AgentID) == "" || strings.TrimSpace(quarantine.AgentVersion) == "" ||
		strings.TrimSpace(quarantine.Reason) == "" || quarantine.Actor.Kind == "" || strings.TrimSpace(quarantine.Actor.ID) == "" {
		return fmt.Errorf("%w: a quarantine change needs tenant, agent, version, reason, and actor", domain.ErrValidation)
	}
	return nil
}

func missingPackages(pinned, latest []domain.CapabilityRef) []domain.CapabilityRef {
	held := make(map[string]string, len(pinned))
	for _, ref := range pinned {
		held[ref.PackageID] = ref.PackageVersion
	}
	var missing []domain.CapabilityRef
	for _, ref := range latest {
		if version, found := held[ref.PackageID]; !found || version != ref.PackageVersion {
			missing = append(missing, ref)
		}
	}
	return missing
}

// TypeVersionDigest is the canonical digest of a published type version.
func TypeVersionDigest(version domain.AgentTypeVersion) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "scout.agent_type.v1\n%s\x1f%s\x1f%s\x1f%s\x1f",
		version.AgentTypeID, version.TypeVersion, version.Purpose, version.Autonomy)
	for _, pkg := range version.Packages {
		fmt.Fprintf(sum, "%s@%s:%t\x1e", pkg.PackageID, pkg.PackageVersion, pkg.Required)
	}
	sum.Write(version.Definition)
	return hex.EncodeToString(sum.Sum(nil))
}

type resourceKey struct {
	kind domain.ResourceKind
	id   string
}

func valueDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableUser(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

var (
	_ contract.AgentTypeRepository         = (*AgentTypeStore)(nil)
	_ contract.CapabilityPackageRepository = (*AgentTypeStore)(nil)
	_ contract.AgentTypeService            = (*AgentTypeStore)(nil)
	_ contract.AgentLifecycle              = (*AgentTypeStore)(nil)
	_ contract.AgentVersionQuarantine      = (*AgentTypeStore)(nil)
)
