package scope

import (
	"context"
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
	qScopeNode       = "scout_scope_node"
	qScopeBindings   = "scout_scope_bindings"
	qEffectiveGet    = "scout_effective_release_get"
	qEffectivePut    = "scout_effective_release_put"
	maxScopeChainRow = 64
)

var scopeQueries = map[string]string{
	qScopeNode: `
SELECT parent_scope_id, scope_kind_code, display_name
  FROM config_scope
 WHERE tenant_id = ? AND scope_id = ?`,
	qScopeBindings: `
SELECT scope_id, resource_kind_code, resource_id, resource_version, merge_mode_code,
       sealed, resource_value, resource_value_digest, begda, endda, bound_by
  FROM config_scope_binding
 WHERE tenant_id = ? AND begda <= ? AND (endda IS NULL OR endda > ?)
 ORDER BY resource_kind_code, resource_id, begda`,
	qEffectiveGet: `
SELECT scope_id, payload, payload_digest, compiled_by, compiled_at
  FROM effective_agent_release
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ?`,
	qEffectivePut: `
INSERT INTO effective_agent_release
       (tenant_id, agent_id, agent_version, scope_id, payload, payload_digest, compiled_by, compiled_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
}

// TableScopeRepository reads the scope hierarchy and its bindings through keel named SQL.
type TableScopeRepository struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (r *TableScopeRepository) init(ctx context.Context) error {
	if r.DB == nil {
		return fmt.Errorf("scope repository: database is required")
	}
	r.once.Do(func() { r.qs = r.DB.GetQueryService(ctx, scopeQueries) })
	if r.qs == nil {
		return fmt.Errorf("scope repository: query service is required")
	}
	return nil
}

// Chain walks parent links from scopeID to the root and returns them widest first.
func (r *TableScopeRepository) Chain(ctx context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
	if err := r.init(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(scopeID) == "" {
		return nil, fmt.Errorf("%w: scope id is required", domain.ErrValidation)
	}
	var reversed domain.ScopeChain
	seen := make(map[string]struct{}, maxScopeChainRow)
	for current := scopeID; current != ""; {
		if _, loop := seen[current]; loop {
			return nil, fmt.Errorf("%w: scope %q is part of a parent cycle", domain.ErrConflict, current)
		}
		seen[current] = struct{}{}
		if len(reversed) >= maxScopeChainRow {
			return nil, fmt.Errorf("%w: scope chain exceeds %d nodes", domain.ErrValidation, maxScopeChainRow)
		}
		result, err := r.qs.Query(ctx, qScopeNode, tenantID, current)
		if err != nil {
			return nil, fmt.Errorf("load scope: %w", err)
		}
		if len(result.Rows) == 0 {
			return nil, fmt.Errorf("%w: scope %q", domain.ErrNotFound, current)
		}
		row := result.Rows[0]
		parent := common.AsString(row[0])
		reversed = append(reversed, domain.Scope{
			TenantID: tenantID, ScopeID: current, ParentScopeID: parent,
			ScopeKind: common.AsString(row[1]), DisplayName: common.AsString(row[2]),
		})
		current = parent
	}
	chain := make(domain.ScopeChain, 0, len(reversed))
	for index := len(reversed) - 1; index >= 0; index-- {
		chain = append(chain, reversed[index])
	}
	return chain, nil
}

// Bindings returns every binding in force at asOf whose scope is in scopeIDs.
func (r *TableScopeRepository) Bindings(ctx context.Context, tenantID int64, scopeIDs []string, asOf time.Time) ([]domain.ScopedBinding, error) {
	if err := r.init(ctx); err != nil {
		return nil, err
	}
	if len(scopeIDs) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(scopeIDs))
	for _, id := range scopeIDs {
		wanted[id] = struct{}{}
	}
	result, err := r.qs.Query(ctx, qScopeBindings, tenantID, asOf, asOf)
	if err != nil {
		return nil, fmt.Errorf("load scoped bindings: %w", err)
	}
	bindings := make([]domain.ScopedBinding, 0, len(result.Rows))
	for _, row := range result.Rows {
		scopeID := common.AsString(row[0])
		if _, ok := wanted[scopeID]; !ok {
			continue
		}
		validTo, _ := common.AsTimeOK(row[9])
		binding := domain.ScopedBinding{
			TenantID: tenantID, ScopeID: scopeID,
			ResourceKind: domain.ResourceKind(common.AsString(row[1])), ResourceID: common.AsString(row[2]),
			ResourceVersion: common.AsString(row[3]), MergeMode: domain.MergeMode(common.AsString(row[4])),
			Sealed: common.AsBool(row[5]), Value: []byte(common.AsString(row[6])), ValueDigest: common.AsString(row[7]),
			ValidFrom: common.AsTime(row[8]), ValidTo: validTo,
		}
		if boundBy := common.AsInt64(row[10]); boundBy > 0 {
			binding.BoundBy = &boundBy
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// TableEffectiveReleaseStore persists compiled releases as canonical JSON with their digest.
type TableEffectiveReleaseStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (s *TableEffectiveReleaseStore) init(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("effective release store: database is required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, scopeQueries) })
	if s.qs == nil {
		return fmt.Errorf("effective release store: query service is required")
	}
	return nil
}

// Put stores a compiled release; the digest is recomputed and must match.
func (s *TableEffectiveReleaseStore) Put(ctx context.Context, release domain.EffectiveRelease) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if expected := Digest(release); release.Digest != expected {
		return fmt.Errorf("%w: effective release digest does not match its content", domain.ErrValidation)
	}
	payload, err := json.Marshal(release.Resources)
	if err != nil {
		return fmt.Errorf("encode effective release: %w", err)
	}
	var compiledBy any
	if release.CompiledBy != nil {
		compiledBy = *release.CompiledBy
	}
	// The agent version is already committed by the time a release is frozen, so a
	// client disconnect must not leave a published version without one.
	if _, err := s.qs.Query(context.WithoutCancel(ctx), qEffectivePut, release.TenantID, release.AgentID, release.AgentVersion,
		release.ScopeID, string(payload), release.Digest, compiledBy, release.CompiledAt); err != nil {
		return fmt.Errorf("put effective release: %w", err)
	}
	return nil
}

// Get returns the release compiled for one agent version.
func (s *TableEffectiveReleaseStore) Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.EffectiveRelease, error) {
	if err := s.init(ctx); err != nil {
		return domain.EffectiveRelease{}, err
	}
	result, err := s.qs.Query(ctx, qEffectiveGet, tenantID, agentID, agentVersion)
	if err != nil {
		return domain.EffectiveRelease{}, fmt.Errorf("get effective release: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.EffectiveRelease{}, fmt.Errorf("%w: effective release for %s/%s", domain.ErrNotFound, agentID, agentVersion)
	}
	row := result.Rows[0]
	release := domain.EffectiveRelease{
		TenantID: tenantID, AgentID: agentID, AgentVersion: agentVersion,
		ScopeID: common.AsString(row[0]), Digest: common.AsString(row[2]), CompiledAt: common.AsTime(row[4]),
	}
	if err := json.Unmarshal([]byte(common.AsString(row[1])), &release.Resources); err != nil {
		return domain.EffectiveRelease{}, fmt.Errorf("decode effective release: %w", err)
	}
	if compiledBy := common.AsInt64(row[3]); compiledBy > 0 {
		release.CompiledBy = &compiledBy
	}
	if expected := Digest(release); release.Digest != expected {
		return domain.EffectiveRelease{}, fmt.Errorf("%w: stored effective release digest does not match its content", domain.ErrValidation)
	}
	return release, nil
}

var (
	_ contract.ScopeRepository            = (*TableScopeRepository)(nil)
	_ contract.EffectiveReleaseRepository = (*TableEffectiveReleaseStore)(nil)
)
