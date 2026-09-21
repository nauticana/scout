package controlplane

import (
	"context"
	"fmt"
	"strings"
	"sync"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qModelAccessGrant  = "scout_model_access_grant"
	qModelAccessRevoke = "scout_model_access_revoke"
)

var modelAccessQueries = map[string]string{
	qModelAccessGrant: `
INSERT INTO tenant_model_access (tenant_id, provider_id, model_id, priority_class_code)
VALUES (?, ?, ?, ?)
ON CONFLICT (tenant_id, provider_id, model_id) DO UPDATE
   SET priority_class_code = EXCLUDED.priority_class_code`,
	qModelAccessRevoke: `
DELETE FROM tenant_model_access
 WHERE tenant_id = ? AND provider_id = ? AND model_id = ?`,
}

// DefaultPriorityClass is the scheduling class a grant takes when none is named.
const DefaultPriorityClass = "standard"

// ModelAccess writes the tenant's routable model set. AgentProvisioner and
// Studio publishing grant the models an agent definition names; this grants
// the ones a product chooses for itself.
type ModelAccess struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.ModelAccessWriter = (*ModelAccess)(nil)

func (access *ModelAccess) init(ctx context.Context) error {
	if access.DB == nil {
		return fmt.Errorf("model access: database is required")
	}
	access.once.Do(func() { access.qs = access.DB.GetQueryService(ctx, modelAccessQueries) })
	if access.qs == nil {
		return fmt.Errorf("model access: query service is required")
	}
	return nil
}

func (access *ModelAccess) target(tenantID int64, reference domain.ModelReference) (domain.ModelReference, error) {
	reference.ProviderID = strings.TrimSpace(reference.ProviderID)
	reference.ModelID = strings.TrimSpace(reference.ModelID)
	if tenantID <= 0 || reference.ProviderID == "" || reference.ModelID == "" {
		return domain.ModelReference{}, fmt.Errorf("%w: tenant, provider, and model are required", domain.ErrValidation)
	}
	return reference, nil
}

// Grant makes the model routable for the tenant under a scheduling class.
func (access *ModelAccess) Grant(ctx context.Context, tenantID int64, reference domain.ModelReference, priorityClass string) error {
	reference, err := access.target(tenantID, reference)
	if err != nil {
		return err
	}
	if err := access.init(ctx); err != nil {
		return err
	}
	if priorityClass = strings.TrimSpace(priorityClass); priorityClass == "" {
		priorityClass = DefaultPriorityClass
	}
	if _, err := access.qs.Query(ctx, qModelAccessGrant, tenantID, reference.ProviderID, reference.ModelID, priorityClass); err != nil {
		return fmt.Errorf("grant model %s/%s to tenant %d: %w", reference.ProviderID, reference.ModelID, tenantID, err)
	}
	return nil
}

// Revoke withdraws the model; revoking one that was never granted is a no-op.
func (access *ModelAccess) Revoke(ctx context.Context, tenantID int64, reference domain.ModelReference) error {
	reference, err := access.target(tenantID, reference)
	if err != nil {
		return err
	}
	if err := access.init(ctx); err != nil {
		return err
	}
	if _, err := access.qs.Query(ctx, qModelAccessRevoke, tenantID, reference.ProviderID, reference.ModelID); err != nil {
		return fmt.Errorf("revoke model %s/%s from tenant %d: %w", reference.ProviderID, reference.ModelID, tenantID, err)
	}
	return nil
}
