package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

// The authorization object and actions a person needs to converse with an agent.
const (
	ConversationAuthorizationObject = "AGENT_CONVERSATION"
	ConversationActionUse           = "USE"
	ConversationActionPinRelease    = "PIN_RELEASE"
)

// TurnIngress admits one turn: rate limit, durable turn identity, budget
// reservation, reply subscription, durable dispatch. Nothing irreversible
// happens before the reservation, and a failed dispatch refunds it.
type TurnIngress struct {
	Limiter    contract.TenantRateLimiter
	Records    contract.TurnRecordStore
	Objects    ObjectStateCodec
	Estimator  contract.TurnBudgetEstimator
	Budget     contract.TenantBudgetManager
	Replies    contract.TurnReplySubscriber
	Dispatcher contract.TurnDispatcher
	// Roles authorizes the person a turn acts for: AGENT_CONVERSATION USE, and PIN_RELEASE
	// for a release pin, at the value "agent:<id>".
	Roles contract.PrincipalAuthorizer
	// Principals resolves the serving agent's principal for a turn that arrives without one.
	Principals contract.PrincipalResolver
	// Audit is optional; when set an admitted turn opens its decision chain with turn_admitted.
	Audit contract.AuditSink
	Now   func() time.Time
}

var _ contract.ConversationIngress = (*TurnIngress)(nil)

func (ingress *TurnIngress) validate() error {
	if ingress.Limiter == nil || ingress.Records == nil || ingress.Objects == nil ||
		ingress.Estimator == nil || ingress.Budget == nil || ingress.Replies == nil || ingress.Dispatcher == nil || ingress.Roles == nil || ingress.Principals == nil {
		return fmt.Errorf("%w: turn ingress needs a limiter, record store, object codec, estimator, budget manager, subscriber, dispatcher, role authorizer, and principal resolver", domain.ErrValidation)
	}
	return nil
}

func (ingress *TurnIngress) now() time.Time {
	if ingress.Now != nil {
		return ingress.Now()
	}
	return time.Now()
}

// OpenTurn returns the live reply subscription for the admitted turn. A repeat
// of a terminal request id only resubscribes: it neither reserves budget nor
// dispatches again.
func (ingress *TurnIngress) OpenTurn(ctx context.Context, request domain.TurnRequest) (contract.TurnReplySubscription, error) {
	tenantID := request.TenantContext.TenantID
	if tenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.ConversationID) == "" ||
		strings.TrimSpace(request.AliasID) == "" && strings.TrimSpace(request.AgentID) == "" || len(request.Input) == 0 {
		return nil, fmt.Errorf("%w: tenant, request, conversation, agent type or agent, and input are required", domain.ErrValidation)
	}
	if request.Release != "" && strings.TrimSpace(request.ReleaseReason) == "" {
		return nil, fmt.Errorf("%w: a release pin needs a reason", domain.ErrValidation)
	}
	if len(request.AliasID) > 80 || len(request.AgentID) > 80 || len(request.Release) > 80 || len(request.ReleaseReason) > 200 || len(request.WorkRef) > 200 {
		return nil, fmt.Errorf("%w: agent type, agent, and release are at most 80 bytes, a release reason and work reference at most 200", domain.ErrValidation)
	}
	if err := ingress.validate(); err != nil {
		return nil, err
	}
	var err error
	if err = ingress.Limiter.AllowTurn(ctx, request.TenantContext); err != nil {
		return nil, fmt.Errorf("admit turn %q: %w", request.RequestID, err)
	}
	if request.AgentID, err = ingress.Records.Address(ctx, tenantID, request.AliasID, request.AgentID, request.ConversationID); err != nil {
		return nil, err
	}
	if request.Principal, err = ingress.agentPrincipal(ctx, request); err != nil {
		return nil, err
	}
	if err = ingress.authorize(ctx, request.TenantContext, request.OnBehalfOf, request.AgentID, request.Release != ""); err != nil {
		return nil, err
	}
	record, err := ingress.Records.Find(ctx, tenantID, request.RequestID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if err == nil && record.Status.Terminal() {
		return ingress.Replies.Subscribe(ctx, tenantID, request.RequestID)
	}
	input, err := ingress.Objects.Dehydrate(ctx, turnInputName(tenantID, request.RequestID), request.Input)
	if err != nil {
		return nil, fmt.Errorf("store turn input: %w", err)
	}
	if _, err = ingress.Records.Open(ctx, request, input); err != nil {
		return nil, err
	}
	quote, err := ingress.Estimator.Estimate(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("estimate turn budget: %w", err)
	}
	reservation, err := ingress.Budget.Reserve(ctx, turnBudgetRequest(request, quote))
	if err != nil {
		return nil, fmt.Errorf("reserve turn budget: %w", err)
	}
	// Subscribe before dispatch so a fast worker cannot publish before the route exists.
	subscription, err := ingress.Replies.Subscribe(ctx, tenantID, request.RequestID)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("subscribe reply route: %w", err), ingress.refund(ctx, reservation, request, turnSubscribeFailed))
	}
	dispatch := domain.TurnDispatch{Turn: request, ReplyRoute: subscription.Route(), EnqueuedAt: ingress.now()}
	if err = ingress.admitted(ctx, request); err == nil {
		err = ingress.Dispatcher.Enqueue(ctx, dispatch)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("dispatch turn %q: %w", request.RequestID, err),
			ingress.refund(ctx, reservation, request, turnDispatchFailed), subscription.Close())
	}
	return subscription, nil
}

// authorize checks the person a turn acts for. A turn acting for nobody is an agent's own or
// delegated work, whose authority the delegation chain and tool gateway already bound; it
// may not pin a release.
func (ingress *TurnIngress) authorize(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, agentID string, pin bool) error {
	if person.ID == "" {
		if pin {
			return fmt.Errorf("%w: only a person may pin a release", domain.ErrForbidden)
		}
		return nil
	}
	principal := domain.Principal{Kind: person.Kind, ID: person.ID, TenantID: tenant.TenantID, ScopeID: tenant.ScopeID}
	actions := []string{ConversationActionUse}
	if pin {
		actions = append(actions, ConversationActionPinRelease)
	}
	for _, action := range actions {
		grant, err := ingress.Roles.Authorize(ctx, principal, ConversationAuthorizationObject, action, "agent:"+agentID)
		if err != nil {
			return fmt.Errorf("authorize turn: %w", err)
		}
		if !grant.Allowed {
			return fmt.Errorf("%w: %s %s on agent %q", domain.ErrForbidden, ConversationAuthorizationObject, action, agentID)
		}
	}
	return nil
}

// agentPrincipal resolves the serving agent unless the caller already acts as it, as a
// delegated turn does with its authority chain; a principal for another agent is refused.
func (ingress *TurnIngress) agentPrincipal(ctx context.Context, request domain.TurnRequest) (domain.Principal, error) {
	if request.Principal.ID != "" {
		if request.Principal.Kind != domain.PrincipalAgent || request.Principal.ID != request.AgentID ||
			request.Principal.TenantID != request.TenantContext.TenantID {
			return domain.Principal{}, fmt.Errorf("%w: the turn's principal is not agent %q of its tenant", domain.ErrForbidden, request.AgentID)
		}
		return request.Principal, nil
	}
	principal, err := ingress.Principals.Resolve(ctx, request.TenantContext.TenantID, domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: request.AgentID})
	if err != nil {
		return domain.Principal{}, err
	}
	if request.TenantContext.ScopeID != "" {
		principal.ScopeID = request.TenantContext.ScopeID
	}
	return principal, nil
}

// Selectable lists the agents of aliasID the person may address, the alias target first.
func (ingress *TurnIngress) Selectable(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, aliasID string) ([]domain.SelectableAgent, error) {
	if err := ingress.validate(); err != nil {
		return nil, err
	}
	if person.ID == "" {
		return nil, fmt.Errorf("%w: a person is required", domain.ErrValidation)
	}
	agents, err := ingress.Records.Selectable(ctx, tenant.TenantID, aliasID)
	if err != nil {
		return nil, err
	}
	selectable := agents[:0]
	for _, agent := range agents {
		err := ingress.authorize(ctx, tenant, person, agent.AgentID, false)
		if errors.Is(err, domain.ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, err
		}
		selectable = append(selectable, agent)
	}
	return selectable, nil
}

// admitted is recorded before dispatch, so no turn runs without the head of its chain.
func (ingress *TurnIngress) admitted(ctx context.Context, request domain.TurnRequest) error {
	if ingress.Audit == nil {
		return nil
	}
	err := ingress.Audit.Record(domain.WithDecisionScope(ctx, "turn/", request.RequestID), domain.DecisionRecord{
		TenantID: request.TenantContext.TenantID, Principal: turnPrincipal(request), ScopeID: request.TenantContext.ScopeID,
		Category: observability.AuditCategoryTurnAdmitted, Action: "turn", Resource: request.AgentID, Outcome: domain.DecisionAllow,
		ReleaseVersion: request.Release, Reason: admissionReason(request), WorkRef: request.WorkRef,
		RequestID: request.RequestID, ConversationID: request.ConversationID, OccurredAt: ingress.now(),
	})
	if err != nil {
		return fmt.Errorf("record %s: %w", observability.AuditCategoryTurnAdmitted, err)
	}
	return nil
}

// Failure codes of a turn whose admission could not deliver it.
const (
	turnSubscribeFailed = "subscribe_failed"
	turnDispatchFailed  = "dispatch_failed"
)

// refund releases the reservation and fails the turn record; a client that
// disconnects later only closes its subscription, and its provider-confirmed
// usage is still settled by the runtime.
func (ingress *TurnIngress) refund(ctx context.Context, reservation domain.BudgetReservation, request domain.TurnRequest, errorCode string) error {
	ctx = context.WithoutCancel(ctx)
	return errors.Join(
		ingress.Budget.Release(ctx, reservation),
		ingress.Records.FailParked(ctx, request.TenantContext.TenantID, request.RequestID, errorCode),
	)
}

func admissionReason(request domain.TurnRequest) string {
	reason := ""
	if request.AliasID != "" {
		reason = "requested as " + request.AliasID
	}
	if request.Release != "" {
		reason = strings.TrimPrefix(reason+"; release "+request.Release+" pinned: "+request.ReleaseReason, "; ")
	}
	return reason
}
