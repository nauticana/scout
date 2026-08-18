package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// PrincipalResolver turns an authenticated transport identity into the principal
// every governed boundary authorizes against. It fails closed: an unresolvable
// identity is domain.ErrPrincipalUnknown, never an anonymous principal.
type PrincipalResolver interface {
	Resolve(ctx context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error)
}

// ExternalPrincipalSource is the optional capability that lets an external identity
// plane own agent principals. Scout remains the authorization owner; the source
// only maps an issuer's subject onto a tenant principal.
type ExternalPrincipalSource interface {
	Lookup(ctx context.Context, issuer, subject string) (tenantID int64, ref domain.PrincipalRef, err error)
}

// PrincipalAuthorizer answers one keel authorization-object question for any
// principal kind. Agents and humans evaluate through the same objects, actions,
// and low/high limits, so a delegated grant is a subset comparison in one model.
type PrincipalAuthorizer interface {
	Authorize(ctx context.Context, principal domain.Principal, object, action, value string) (domain.AuthorizationGrant, error)
}

// DelegationVerifier validates an authority chain before a delegated principal acts.
type DelegationVerifier interface {
	// Verify checks depth, validity windows, and that every hop was granted by a
	// principal holding at least what it conveys.
	Verify(ctx context.Context, principal domain.Principal) (domain.AuthorityRef, error)
}
