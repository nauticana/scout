package charter

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/organization"
	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qUnitScopePut    = "scout_config_scope_unit_put"
	qUnitScopeDelete = "scout_config_scope_unit_delete"
	qUnitScopeList   = "scout_config_scope_unit_list"
)

var unitScopeQueries = map[string]string{
	qUnitScopePut: `
INSERT INTO config_scope_unit (partner_id, unit_id, scope_id)
SELECT u.partner_id, u.id, ?
  FROM organization_unit u
  JOIN enterprise_object o ON o.partner_id = u.partner_id AND o.id = u.id
 WHERE o.partner_id = ? AND o.namespace = ? AND o.identifier = ?
ON CONFLICT (partner_id, unit_id) DO UPDATE SET scope_id = EXCLUDED.scope_id
RETURNING unit_id`,
	qUnitScopeDelete: `
DELETE FROM config_scope_unit s
 USING enterprise_object o
 WHERE o.partner_id = s.partner_id AND o.id = s.unit_id
   AND s.partner_id = ? AND o.namespace = ? AND o.identifier = ?`,
	qUnitScopeList: `
SELECT o.identifier, s.scope_id
  FROM config_scope_unit s
  JOIN enterprise_object o ON o.partner_id = s.partner_id AND o.id = s.unit_id
 WHERE s.partner_id = ? AND o.namespace = ?
 ORDER BY o.identifier`,
}

// UnitScopes attaches a tenant's configuration scopes to Charter organization units, so scoped
// configuration inherits along the organization tree: a unit's scope must descend from the scope
// of its nearest attached ancestor. Namespace maps a tenant to the Charter namespace owning its units.
// An attachment references the unit's stored row, so Organization reads the Charter store in DB.
type UnitScopes struct {
	DB           keelport.DatabaseRepository
	Organization organization.Provider
	Scopes       contract.ScopeRepository
	Namespace    func(tenantID int64) string

	once sync.Once
	qs   keelport.QueryService
}

func (u *UnitScopes) init(ctx context.Context, tenantID int64) (string, error) {
	if u.DB == nil || u.Organization == nil || u.Scopes == nil || u.Namespace == nil {
		return "", fmt.Errorf("%w: unit scopes need a database, an organization provider, a scope repository, and a namespace", domain.ErrNotReady)
	}
	if tenantID <= 0 {
		return "", fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	u.once.Do(func() { u.qs = u.DB.GetQueryService(ctx, unitScopeQueries) })
	namespace := u.Namespace(tenantID)
	if namespace == "" {
		return "", fmt.Errorf("%w: tenant %d has no charter namespace", domain.ErrNotFound, tenantID)
	}
	return namespace, nil
}

// Attach binds the unit to the scope, replacing an earlier attachment, and refuses an attachment
// that would place a unit's scope outside the subtree of its nearest attached ancestor, or an
// attached descendant outside this scope's subtree.
func (u *UnitScopes) Attach(ctx context.Context, tenantID int64, unit model.Ref, scopeID string) (err error) {
	namespace, err := u.init(ctx, tenantID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(unit.ID) == "" || strings.TrimSpace(scopeID) == "" {
		return fmt.Errorf("%w: unit and scope are required", domain.ErrValidation)
	}
	if _, err = u.Organization.Unit(ctx, namespace, unit); err != nil {
		return fmt.Errorf("%w: charter unit %s: %v", domain.ErrNotFound, unit.ID, err)
	}
	if _, err = u.Scopes.Chain(ctx, tenantID, scopeID); err != nil {
		return err
	}
	tx, err := u.DB.BeginTx(ctx, unitScopeQueries)
	if err != nil {
		return fmt.Errorf("begin unit scope attachment: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback unit scope attachment: %w", rollbackErr))
			}
		}
	}()
	stored, err := tx.Query(ctx, qUnitScopePut, scopeID, tenantID, namespace, unit.ID)
	if err != nil {
		return fmt.Errorf("attach unit %s to scope %s: %w", unit.ID, scopeID, err)
	}
	if len(stored.Rows) == 0 {
		return fmt.Errorf("%w: charter unit %s is not stored in %s", domain.ErrNotFound, unit.ID, namespace)
	}
	attached, err := u.attachments(ctx, tx, tenantID, namespace)
	if err != nil {
		return err
	}
	if err = u.descends(ctx, tenantID, namespace, attached); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit unit scope attachment: %w", err)
	}
	committed = true
	return nil
}

// Detach removes the unit's attachment; its positions fall back to the nearest attached ancestor.
func (u *UnitScopes) Detach(ctx context.Context, tenantID int64, unit model.Ref) error {
	namespace, err := u.init(ctx, tenantID)
	if err != nil {
		return err
	}
	if _, err = u.qs.Query(ctx, qUnitScopeDelete, tenantID, namespace, unit.ID); err != nil {
		return fmt.Errorf("detach unit %s: %w", unit.ID, err)
	}
	return nil
}

// Resolve returns the scope of an organization unit, a position, or an assignment to a position
// or a unit: the nearest attached unit on the path from its unit to the root, or the tenant root
// as an empty scope id when none is attached.
func (u *UnitScopes) Resolve(ctx context.Context, tenantID int64, subject model.ObjectRef) (string, error) {
	namespace, err := u.init(ctx, tenantID)
	if err != nil {
		return "", err
	}
	unit, err := u.unitOf(ctx, namespace, subject)
	if err != nil {
		return "", err
	}
	attached, err := u.attachments(ctx, u.qs, tenantID, namespace)
	if err != nil {
		return "", err
	}
	path, err := organization.Queries{Provider: u.Organization}.UnitPath(ctx, namespace, unit)
	if err != nil {
		return "", fmt.Errorf("%w: unit path of %s: %v", domain.ErrValidation, unit.ID, err)
	}
	for i := len(path) - 1; i >= 0; i-- {
		if scopeID, ok := attached[path[i].ID]; ok {
			return scopeID, nil
		}
	}
	return "", nil
}

func (u *UnitScopes) unitOf(ctx context.Context, namespace string, subject model.ObjectRef) (model.Ref, error) {
	ref := model.Ref{Namespace: subject.Namespace, ID: subject.ID}
	switch subject.Kind {
	case model.KindOrganizationUnit:
		return ref, nil
	case model.KindPosition:
		position, err := u.Organization.Position(ctx, namespace, ref)
		if err != nil {
			return model.Ref{}, fmt.Errorf("%w: charter position %s: %v", domain.ErrNotFound, subject.ID, err)
		}
		return position.OrganizationUnitID, nil
	case model.KindAssignment:
		assignment, err := u.Organization.Assignment(ctx, namespace, ref)
		if err != nil {
			return model.Ref{}, fmt.Errorf("%w: charter assignment %s: %v", domain.ErrNotFound, subject.ID, err)
		}
		if assignment.Target.Kind != model.KindPosition && assignment.Target.Kind != model.KindOrganizationUnit {
			return model.Ref{}, fmt.Errorf("%w: assignment %s targets a %s, which has no organization unit", domain.ErrValidation, subject.ID, assignment.Target.Kind)
		}
		return u.unitOf(ctx, namespace, assignment.Target)
	}
	return model.Ref{}, fmt.Errorf("%w: a scope resolves for a unit, a position, or an assignment, not a %s", domain.ErrValidation, subject.Kind)
}

// attachments reads the tenant's attachments in its namespace, keyed by unit id.
func (u *UnitScopes) attachments(ctx context.Context, qs keelport.QueryService, tenantID int64, namespace string) (map[string]string, error) {
	result, err := qs.Query(ctx, qUnitScopeList, tenantID, namespace)
	if err != nil {
		return nil, fmt.Errorf("list unit scopes of tenant %d: %w", tenantID, err)
	}
	attached := make(map[string]string, len(result.Rows))
	for _, row := range result.Rows {
		attached[common.AsString(row[0])] = common.AsString(row[1])
	}
	return attached, nil
}

// descends checks every attached unit against its nearest attached ancestor: the unit's scope chain
// must contain the ancestor's scope.
func (u *UnitScopes) descends(ctx context.Context, tenantID int64, namespace string, attached map[string]string) error {
	queries := organization.Queries{Provider: u.Organization}
	for _, unitID := range slices.Sorted(maps.Keys(attached)) {
		path, err := queries.UnitPath(ctx, namespace, model.Ref{ID: unitID})
		if err != nil {
			return fmt.Errorf("%w: unit path of %s: %v", domain.ErrValidation, unitID, err)
		}
		for i := len(path) - 2; i >= 0; i-- {
			ancestorScope, ok := attached[path[i].ID]
			if !ok {
				continue
			}
			chain, err := u.Scopes.Chain(ctx, tenantID, attached[unitID])
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(chain, func(scope domain.Scope) bool { return scope.ScopeID == ancestorScope }) {
				return fmt.Errorf("%w: scope %s of unit %s does not descend from scope %s of unit %s", domain.ErrValidation,
					attached[unitID], unitID, ancestorScope, path[i].ID)
			}
			break
		}
	}
	return nil
}
