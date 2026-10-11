package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// ApprovalAuthorizer verifies that a principal may decide one approval request.
type ApprovalAuthorizer interface {
	AuthorizeApproval(ctx context.Context, request domain.ApprovalRequest, principal domain.PrincipalRef) error
}

// ApprovalStore is the durable record of decisions humans owe. Open is
// idempotent per (tenant, request, step) so a replayed turn re-attaches to the
// request it already created instead of asking twice.
type ApprovalStore interface {
	Open(ctx context.Context, request domain.ApprovalRequest) (domain.ApprovalRequest, error)
	Get(ctx context.Context, key domain.ApprovalKey) (domain.ApprovalRequest, error)
	// Resolve records a verdict once. An exact retry returns the stored request;
	// a different second verdict or ProposedDigest is domain.ErrConflict.
	Resolve(ctx context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error)
	// Revision returns the edit recorded on an edited request; any other request is domain.ErrNotFound.
	Revision(ctx context.Context, key domain.ApprovalKey) (domain.ProposalRevision, error)
}

// ApproverRouter names the principal who decides a request. The product maps the request's
// rule to its own roles or positions and picks an eligible person; Scout records who it was.
type ApproverRouter interface {
	Route(ctx context.Context, request domain.ApprovalRequest) (domain.PrincipalRef, error)
}

// ApprovalInbox is the reviewer-facing read side.
type ApprovalInbox interface {
	// Pending returns the requests a principal may actually decide, soonest deadline first.
	Pending(ctx context.Context, filter domain.ApprovalFilter) ([]domain.ApprovalRequest, error)
	// DueBy returns requests whose deadline has passed and that still await a verdict.
	DueBy(ctx context.Context, tenantID int64, deadline domain.ApprovalFilter) ([]domain.ApprovalRequest, error)
}

// ApprovalDesk is the decider-facing service behind the approval routes.
type ApprovalDesk interface {
	Pending(ctx context.Context, tenantID int64, decider domain.PrincipalRef, limit int) ([]domain.ApprovalRequest, error)
	Resolve(ctx context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error)
	RedeemLink(ctx context.Context, token string, key domain.ApprovalKey, status domain.ApprovalStatus, reason string) (domain.ApprovalRequest, error)
}

// EscalationPolicy decides what happens when a deadline passes with no verdict.
// A backup needs its own grant: escalation may never widen authority.
type EscalationPolicy interface {
	Escalate(ctx context.Context, request domain.ApprovalRequest) (domain.EscalationStep, error)
}

// Notifier delivers outbound messages about owed work. Delivery is keel's
// messaging concern; Scout only emits through this port.
type Notifier interface {
	Notify(ctx context.Context, notification domain.Notification) error
}
