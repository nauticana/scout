package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
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
	Now        func() time.Time
}

var _ contract.ConversationIngress = (*TurnIngress)(nil)

func (ingress *TurnIngress) validate() error {
	if ingress.Limiter == nil || ingress.Records == nil || ingress.Objects == nil ||
		ingress.Estimator == nil || ingress.Budget == nil || ingress.Replies == nil || ingress.Dispatcher == nil {
		return fmt.Errorf("%w: turn ingress needs a limiter, record store, object codec, estimator, budget manager, subscriber, and dispatcher", domain.ErrValidation)
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
		strings.TrimSpace(request.AgentID) == "" || len(request.Input) == 0 {
		return nil, fmt.Errorf("%w: tenant, request, conversation, agent, and input are required", domain.ErrValidation)
	}
	if err := ingress.validate(); err != nil {
		return nil, err
	}
	if err := ingress.Limiter.AllowTurn(ctx, request.TenantContext); err != nil {
		return nil, fmt.Errorf("admit turn %q: %w", request.RequestID, err)
	}
	_, status, _, err := ingress.Records.Find(ctx, tenantID, request.RequestID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if err == nil && isTerminalTurnStatus(status) {
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
	reservation, err := ingress.Budget.Reserve(ctx, tenantID, request.RequestID,
		quote.InputTokens+quote.OutputTokens, quote.CostMinorUnits, quote.Currency)
	if err != nil {
		return nil, fmt.Errorf("reserve turn budget: %w", err)
	}
	// Subscribe before dispatch so a fast worker cannot publish before the route exists.
	subscription, err := ingress.Replies.Subscribe(ctx, tenantID, request.RequestID)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("subscribe reply route: %w", err), ingress.refund(ctx, reservation, request, "subscribe_failed"))
	}
	dispatch := domain.TurnDispatch{Turn: request, ReplyRoute: subscription.Route(), EnqueuedAt: ingress.now()}
	if err = ingress.Dispatcher.Enqueue(ctx, dispatch); err != nil {
		return nil, errors.Join(fmt.Errorf("dispatch turn %q: %w", request.RequestID, err),
			ingress.refund(ctx, reservation, request, "dispatch_failed"), subscription.Close())
	}
	return subscription, nil
}

// refund releases the reservation and fails the turn record; a client that
// disconnects later only closes its subscription, and its provider-confirmed
// usage is still settled by the runtime.
func (ingress *TurnIngress) refund(ctx context.Context, reservation domain.BudgetReservation, request domain.TurnRequest, errorCode string) error {
	ctx = context.WithoutCancel(ctx)
	return errors.Join(
		ingress.Budget.Release(ctx, reservation),
		ingress.Records.Fail(ctx, request.TenantContext.TenantID, request.RequestID, "failed", errorCode),
	)
}
