package approval

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func approvalAuthorizer(allowed bool) *RoleAuthorizer {
	return &RoleAuthorizer{
		Principals: fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
			return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID}, nil
		}),
		Roles: fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
			return domain.AuthorizationGrant{Allowed: allowed}, nil
		}),
		AuthorizationObject: "AGENT_APPROVAL",
	}
}

func TestRoleAuthorizerFailsClosedForAnUnauthorizedReviewer(t *testing.T) {
	err := approvalAuthorizer(false).AuthorizeApproval(context.Background(), domain.ApprovalRequest{
		ID: 9, TenantID: 7, ScopeID: "unit",
	}, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want an unauthorized verdict refused", err)
	}
}
