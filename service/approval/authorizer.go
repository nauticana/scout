package approval

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DefaultAuthorizationObject is Scout's seeded approval decision object.
const DefaultAuthorizationObject = "AGENT_APPROVAL"

// RoleAuthorizer resolves a reviewer and checks the shared authorization-object
// model before a verdict or escalation can move durable work.
type RoleAuthorizer struct {
	Principals          contract.PrincipalResolver
	Roles               contract.PrincipalAuthorizer
	AuthorizationObject string
}

// AuthorizeApproval verifies that principal may decide request at its scope.
func (a *RoleAuthorizer) AuthorizeApproval(ctx context.Context, request domain.ApprovalRequest, reviewer domain.PrincipalRef) error {
	if a.Principals == nil || a.Roles == nil {
		return fmt.Errorf("approval authorizer: principal resolver and role authorizer are required")
	}
	if request.Approver.ID != "" && request.Approver != reviewer {
		return fmt.Errorf("%w: approval %d is assigned to %q", domain.ErrForbidden, request.ID, request.Approver.ID)
	}
	principal, err := a.Principals.Resolve(ctx, request.TenantID, reviewer)
	if err != nil {
		return err
	}
	value := request.ScopeID
	if value == "" {
		value = request.Resource
	}
	object := strings.TrimSpace(a.AuthorizationObject)
	if object == "" {
		object = DefaultAuthorizationObject
	}
	grant, err := a.Roles.Authorize(ctx, principal, object, "DECIDE", value)
	if err != nil {
		return err
	}
	if !grant.Allowed {
		return fmt.Errorf("%w: principal %q may not decide approval %d", domain.ErrForbidden, reviewer.ID, request.ID)
	}
	return nil
}

var _ contract.ApprovalAuthorizer = (*RoleAuthorizer)(nil)
