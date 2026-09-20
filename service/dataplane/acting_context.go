package dataplane

import (
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/domain"
)

// actingContext is who a queued turn acts as: references and bounds, never a
// credential. The authority chain is verified again wherever it is exercised.
type actingContext struct {
	Principal     domain.Principal        `json:"principal"`
	OnBehalfOf    domain.PrincipalRef     `json:"on_behalf_of,omitzero"`
	Bounds        domain.DelegationBounds `json:"bounds,omitzero"`
	WorkItemID    int64                   `json:"work_item_id,omitempty"`
	WorkItemDepth int                     `json:"work_item_depth,omitempty"`
	Tenant        domain.TenantContext    `json:"tenant"`
}

// encodeActingContext returns nil for a turn that names no principal; it then
// runs as its agent on its own authority.
func encodeActingContext(turn domain.TurnRequest) (any, error) {
	if turn.Principal.Kind == "" && turn.OnBehalfOf.ID == "" {
		return nil, nil
	}
	if turn.Principal.Kind != "" && turn.Principal.TenantID != turn.TenantContext.TenantID {
		return nil, fmt.Errorf("%w: principal tenant %d does not own the turn", domain.ErrForbidden, turn.Principal.TenantID)
	}
	encoded, err := json.Marshal(actingContext{
		Principal: turn.Principal, OnBehalfOf: turn.OnBehalfOf, Bounds: turn.DelegationBounds,
		WorkItemID: turn.WorkItemID, WorkItemDepth: turn.WorkItemDepth, Tenant: turn.TenantContext,
	})
	if err != nil {
		return nil, fmt.Errorf("encode acting context: %w", err)
	}
	return string(encoded), nil
}

// decodeActingContext restores it onto a claimed turn, whose tenant the queue row already fixed.
func decodeActingContext(encoded string, turn *domain.TurnRequest) error {
	if encoded == "" {
		return nil
	}
	var acting actingContext
	if err := json.Unmarshal([]byte(encoded), &acting); err != nil {
		return fmt.Errorf("decode acting context of request %q: %w", turn.RequestID, err)
	}
	tenantID := turn.TenantContext.TenantID
	if acting.Tenant.TenantID != tenantID || acting.Principal.Kind != "" && acting.Principal.TenantID != tenantID {
		return fmt.Errorf("%w: acting context of request %q belongs to another tenant", domain.ErrForbidden, turn.RequestID)
	}
	turn.TenantContext, turn.Principal, turn.OnBehalfOf = acting.Tenant, acting.Principal, acting.OnBehalfOf
	turn.DelegationBounds, turn.WorkItemID, turn.WorkItemDepth = acting.Bounds, acting.WorkItemID, acting.WorkItemDepth
	return nil
}
