package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
	"github.com/nauticana/scout/service/scope"
)

// ReleaseRouter routes approvals by the approval_routing frozen into the asking agent's release:
// the first matching route names a role or position, the product resolver names its holders, and
// the first eligible holder decides. Nobody eligible opens the request vacant; nothing it does
// ever approves. It is also the sweeper's escalation policy, following the same route.
type ReleaseRouter struct {
	Releases contract.EffectiveReleaseRepository
	Resolver contract.ApproverResolver
	// Authorizer checks every candidate's own DECIDE grant at the request's scope.
	Authorizer contract.ApprovalAuthorizer
	// Fallback routes requests of a release that binds no approval_routing; nil refuses them.
	Fallback contract.ApproverRouter
	// Unrouted escalates requests opened without a route; nil expires them.
	Unrouted contract.EscalationPolicy
	// MaxCandidates bounds the holders one resolution considers; zero takes
	// domain.DefaultMaxApproverCandidates.
	MaxCandidates int
	Now           func() time.Time
}

var (
	_ contract.ApproverRouter   = (*ReleaseRouter)(nil)
	_ contract.EscalationPolicy = (*ReleaseRouter)(nil)
)

// Route assigns a new request under the first matching frozen route, walking its escalation
// steps until someone is eligible; prefer is chosen first when it is eligible.
func (r *ReleaseRouter) Route(ctx context.Context, request domain.ApprovalRequest, prefer domain.PrincipalRef) (domain.ApprovalAssignment, error) {
	if err := r.ready(); err != nil {
		return domain.ApprovalAssignment{}, err
	}
	routing, bound, err := r.routing(ctx, request)
	if err != nil {
		return domain.ApprovalAssignment{}, err
	}
	if !bound {
		if r.Fallback == nil {
			return domain.ApprovalAssignment{}, fmt.Errorf("%w: release %s of %q binds no approval routing", domain.ErrForbidden, request.ReleaseVersion, request.Principal.ID)
		}
		return r.Fallback.Route(ctx, request, prefer)
	}
	route, found := scope.MatchApprovalRoute(routing, request.Resource, request.Source, request.RiskTier)
	if !found {
		return domain.ApprovalAssignment{}, fmt.Errorf("%w: no approval route serves tool %q for %s at %s risk",
			domain.ErrForbidden, request.Resource, request.Source, request.RiskTier)
	}
	resolved := map[domain.ApproverReference]bool{}
	for step := 0; step <= len(route.Escalation); step++ {
		reference := referenceAt(route, step)
		if resolved[reference] {
			continue
		}
		resolved[reference] = true
		candidate, found, err := r.pick(ctx, request, reference, prefer, domain.PrincipalRef{})
		if err != nil {
			return domain.ApprovalAssignment{}, err
		}
		if found {
			return domain.ApprovalAssignment{
				Approver:  candidate.Principal,
				Authority: authority(route, step, reference, candidate.Resolution),
				Deadline:  stepDeadline(route, step),
			}, nil
		}
	}
	return domain.ApprovalAssignment{
		Authority: authority(route, 0, route.Approver, ""),
		Deadline:  stepDeadline(route, 0),
	}, nil
}

// Escalate moves one overdue or vacant request along the route it was opened under. A vacancy
// not yet overdue is filled in place or waits; an overdue request goes to the next step with an
// eligible holder, vacates the next step when nobody is eligible, and once every step is spent
// holds or expires as the route says. No outcome approves.
func (r *ReleaseRouter) Escalate(ctx context.Context, request domain.ApprovalRequest) (domain.EscalationStep, error) {
	if request.Route.RouteID == "" {
		if r.Unrouted != nil {
			return r.Unrouted.Escalate(ctx, request)
		}
		return domain.EscalationStep{Action: domain.EscalationExpire, Reason: "deadline passed"}, nil
	}
	if err := r.ready(); err != nil {
		return domain.EscalationStep{}, err
	}
	route, err := r.route(ctx, request)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.EscalationStep{Action: domain.EscalationExpire, Reason: "route unavailable"}, nil
	}
	if err != nil {
		return domain.EscalationStep{}, err
	}
	current := request.Route.Step
	overdue := !request.DeadlineAt.IsZero() && !request.DeadlineAt.After(r.now())
	if !overdue {
		if request.Approver.ID != "" {
			return domain.EscalationStep{Action: domain.EscalationWait, Reason: "not overdue"}, nil
		}
		reference := referenceAt(route, current)
		candidate, found, err := r.pick(ctx, request, reference, domain.PrincipalRef{}, domain.PrincipalRef{})
		if err != nil || !found {
			return domain.EscalationStep{Action: domain.EscalationWait, Reason: "no eligible approver yet"}, err
		}
		return domain.EscalationStep{
			Action: domain.EscalationReassign, Backup: candidate.Principal, KeepDeadline: true,
			Route: authority(route, current, reference, candidate.Resolution), Reason: "vacancy filled",
		}, nil
	}
	for step := current + 1; step <= len(route.Escalation); step++ {
		reference := referenceAt(route, step)
		prefer, exclude := request.Approver, domain.PrincipalRef{}
		if route.Escalation[step-1].Approver != nil {
			prefer, exclude = domain.PrincipalRef{}, request.Approver
		}
		candidate, found, err := r.pick(ctx, request, reference, prefer, exclude)
		if err != nil {
			return domain.EscalationStep{}, err
		}
		if found {
			return domain.EscalationStep{
				Action: domain.EscalationReassign, Backup: candidate.Principal, Extension: stepDeadline(route, step),
				Route: authority(route, step, reference, candidate.Resolution), Reason: "deadline passed",
			}, nil
		}
	}
	if current < len(route.Escalation) {
		next := current + 1
		return domain.EscalationStep{
			Action: domain.EscalationVacate, Extension: stepDeadline(route, next),
			Route: authority(route, next, referenceAt(route, next), ""), Reason: "no eligible approver",
		}, nil
	}
	if route.OnExhausted == domain.ApprovalExhaustHold {
		return domain.EscalationStep{
			Action: domain.EscalationHold, Backup: request.Approver, Route: request.Route, Reason: "escalation exhausted",
		}, nil
	}
	return domain.EscalationStep{Action: domain.EscalationExpire, Reason: "escalation exhausted"}, nil
}

func (r *ReleaseRouter) ready() error {
	if r.Releases == nil || r.Resolver == nil || r.Authorizer == nil {
		return fmt.Errorf("release approval router: releases, resolver, and authorizer are required")
	}
	if r.MaxCandidates < 0 {
		return fmt.Errorf("release approval router: max candidates cannot be negative")
	}
	return nil
}

func (r *ReleaseRouter) maxCandidates() int {
	if r.MaxCandidates > 0 {
		return r.MaxCandidates
	}
	return domain.DefaultMaxApproverCandidates
}

// routing reads the routing frozen into the release the request is pinned to.
func (r *ReleaseRouter) routing(ctx context.Context, request domain.ApprovalRequest) (domain.ApprovalRouting, bool, error) {
	if request.Principal.Kind != domain.PrincipalAgent || strings.TrimSpace(request.ReleaseVersion) == "" {
		return domain.ApprovalRouting{}, false, fmt.Errorf("%w: approval is not pinned to a release", domain.ErrForbidden)
	}
	release, err := r.Releases.Get(ctx, request.TenantID, request.Principal.ID, request.ReleaseVersion)
	if err != nil {
		return domain.ApprovalRouting{}, false, err
	}
	if release.TenantID != request.TenantID || release.AgentID != request.Principal.ID {
		return domain.ApprovalRouting{}, false, fmt.Errorf("%w: release %s does not belong to the asking agent", domain.ErrForbidden, request.ReleaseVersion)
	}
	return scope.ApprovalRoutingOf(release.Resources)
}

// route finds the route a request was opened under by id; it is never selected again.
func (r *ReleaseRouter) route(ctx context.Context, request domain.ApprovalRequest) (domain.ApprovalRoute, error) {
	routing, bound, err := r.routing(ctx, request)
	if err != nil {
		return domain.ApprovalRoute{}, err
	}
	if bound {
		for _, route := range routing.Routes {
			if route.ID == request.Route.RouteID {
				return route, nil
			}
		}
	}
	return domain.ApprovalRoute{}, fmt.Errorf("%w: approval route %q of release %s", domain.ErrNotFound, request.Route.RouteID, request.ReleaseVersion)
}

// pick returns the first eligible holder of reference: a person other than the asking agent and
// the requester, not excluded, and holding their own DECIDE grant for the request.
func (r *ReleaseRouter) pick(ctx context.Context, request domain.ApprovalRequest, reference domain.ApproverReference, prefer, exclude domain.PrincipalRef) (domain.EligibleApprover, bool, error) {
	limit := r.maxCandidates()
	candidates, err := r.Resolver.Eligible(ctx, domain.ApproverQuery{
		TenantID: request.TenantID, Reference: reference, Request: request, Limit: limit,
	})
	if err != nil {
		return domain.EligibleApprover{}, false, fmt.Errorf("resolve %s %q: %w", reference.Kind, reference.Ref, err)
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	for index, candidate := range candidates {
		if prefer.ID != "" && candidate.Principal == prefer {
			candidates = append([]domain.EligibleApprover{candidate}, append(candidates[:index:index], candidates[index+1:]...)...)
			break
		}
	}
	for _, candidate := range candidates {
		principal := candidate.Principal
		if !decimalHuman(principal) || principal == request.Principal || principal == request.Requester ||
			(exclude.ID != "" && principal == exclude) {
			continue
		}
		assigned := request
		assigned.Approver = principal
		err := r.Authorizer.AuthorizeApproval(ctx, assigned, principal)
		if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPrincipalUnknown) {
			continue
		}
		if err != nil {
			return domain.EligibleApprover{}, false, err
		}
		candidate.Resolution = sanitizeResolution(candidate.Resolution)
		return candidate, true, nil
	}
	return domain.EligibleApprover{}, false, nil
}

func (r *ReleaseRouter) now() time.Time {
	return clk.Now(r.Now)
}

// referenceAt is the role or position deciding at step: the step's own, or the latest before it.
func referenceAt(route domain.ApprovalRoute, step int) domain.ApproverReference {
	reference := route.Approver
	for index := 0; index < step && index < len(route.Escalation); index++ {
		if approver := route.Escalation[index].Approver; approver != nil {
			reference = *approver
		}
	}
	return reference
}

// stepDeadline is the route's deadline at step 0 and the step's extension after it.
func stepDeadline(route domain.ApprovalRoute, step int) time.Duration {
	if step == 0 {
		return time.Duration(route.DeadlineSeconds) * time.Second
	}
	return time.Duration(route.Escalation[step-1].ExtensionSeconds) * time.Second
}

func authority(route domain.ApprovalRoute, step int, reference domain.ApproverReference, resolution string) domain.ApprovalRouteAuthority {
	return domain.ApprovalRouteAuthority{RouteID: route.ID, Reference: reference, Step: step, Resolution: resolution, AuthenticatedOnly: route.AuthenticatedOnly}
}

// sanitizeResolution drops control characters and keeps at most maxResolutionBytes whole runes.
func sanitizeResolution(resolution string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, resolution)
	if len(cleaned) <= maxResolutionBytes {
		return cleaned
	}
	cut := maxResolutionBytes
	for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
		cut--
	}
	return cleaned[:cut]
}
