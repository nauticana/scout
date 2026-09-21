package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// modelBudget prices, reserves, and settles the tenant budget of one model call.
// The gateways that hold a budget share it, so a reservation is estimated and
// released the same way whether or not the call is hedged.
type modelBudget struct {
	budgets      contract.TenantBudgetManager
	pricer       contract.ModelPricer
	promptTokens func([]byte) int64
}

// reserve estimates one model call: its prompt, its output ceiling, and the
// grounding searches it may run.
func (budget modelBudget) reserve(ctx context.Context, requestID string, selection domain.ModelSelection, request domain.ModelRequest) (domain.BudgetReservation, error) {
	tokens := promptTokens(budget.promptTokens, request.Prompt) + request.MaxOutputTokens
	estimated := domain.ModelUsage{InputTokens: tokens - request.MaxOutputTokens, OutputTokens: request.MaxOutputTokens, SearchQueries: EstimatedSearches(request)}
	return budget.hold(ctx, domain.BudgetRequest{TenantID: request.TenantContext.TenantID, RequestID: requestID, Principal: request.Principal, Tokens: tokens},
		selectionReference(selection), estimated)
}

func selectionReference(selection domain.ModelSelection) domain.ModelReference {
	return domain.ModelReference{ProviderID: selection.Provider, ModelID: selection.Model}
}

// hold prices the estimated usage on the route and reserves it for the request.
func (budget modelBudget) hold(ctx context.Context, request domain.BudgetRequest, reference domain.ModelReference, estimated domain.ModelUsage) (domain.BudgetReservation, error) {
	cost, currency, err := budget.pricer.Cost(ctx, reference, estimated)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("price model call: %w", err)
	}
	request.CostMinorUnits, request.Currency = cost, currency
	reservation, err := budget.budgets.Reserve(ctx, request)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve model call: %w", err)
	}
	return reservation, nil
}

// settle prices confirmed usage the provider left unpriced, then closes the hold.
// A pricing failure still closes it against the confirmed tokens: an open hold leaks.
func (budget modelBudget) settle(ctx context.Context, reservation domain.BudgetReservation, reference domain.ModelReference, usage domain.Usage) error {
	usage, priceErr := budget.priced(ctx, reference, usage)
	return errors.Join(priceErr, budget.close(ctx, reservation, usage))
}

func spent(usage domain.Usage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.SearchQueries > 0 || usage.CostMinorUnits > 0
}

// priced fills the cost of spent usage that carries no currency.
func (budget modelBudget) priced(ctx context.Context, reference domain.ModelReference, usage domain.Usage) (domain.Usage, error) {
	if !spent(usage) || strings.TrimSpace(usage.Currency) != "" {
		return usage, nil
	}
	cost, currency, err := budget.pricer.Cost(context.WithoutCancel(ctx), reference, domain.ModelUsage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, SearchQueries: usage.SearchQueries,
	})
	if err != nil {
		return usage, fmt.Errorf("price confirmed model usage: %w", err)
	}
	usage.CostMinorUnits, usage.Currency = cost, currency
	return usage, nil
}

// close commits spent usage and releases a hold nothing was spent against.
func (budget modelBudget) close(ctx context.Context, reservation domain.BudgetReservation, usage domain.Usage) error {
	ctx = context.WithoutCancel(ctx)
	if spent(usage) {
		return budget.budgets.Commit(ctx, reservation, usage)
	}
	return budget.budgets.Release(ctx, reservation)
}

// BudgetedGateway decorates a ModelGateway with the tenant budget a turn
// otherwise only holds through the turn runtime: it estimates the call from the
// prompt, its output ceiling, and its grounding searches, reserves before the
// call, settles from confirmed usage, and releases when nothing was spent. A
// caller that is not a conversation turn composes it directly instead of
// adopting HedgingGateway, which would duplicate the call to hold a budget.
type BudgetedGateway struct {
	Inner   contract.ModelGateway
	Budgets contract.TenantBudgetManager
	// Pricer sizes the reservation in the catalog currency.
	Pricer contract.ModelPricer
	// PromptTokens estimates prompt work; nil uses EstimatePromptTokens.
	PromptTokens func([]byte) int64
}

var _ contract.ModelGateway = (*BudgetedGateway)(nil)

// NewBudgetedGateway validates the required collaborators.
func NewBudgetedGateway(inner contract.ModelGateway, budgets contract.TenantBudgetManager, pricer contract.ModelPricer) (*BudgetedGateway, error) {
	gateway := &BudgetedGateway{Inner: inner, Budgets: budgets, Pricer: pricer}
	if err := gateway.ready(); err != nil {
		return nil, err
	}
	return gateway, nil
}

func (gateway *BudgetedGateway) ready() error {
	if gateway.Inner == nil || gateway.Budgets == nil || gateway.Pricer == nil {
		return fmt.Errorf("budgeted gateway: inner gateway, budget manager, and pricer are required")
	}
	return nil
}

func (gateway *BudgetedGateway) validate(selection domain.ModelSelection, request domain.ModelRequest) error {
	if err := gateway.ready(); err != nil {
		return err
	}
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" ||
		strings.TrimSpace(selection.Provider) == "" || strings.TrimSpace(selection.Model) == "" {
		return fmt.Errorf("%w: tenant, request id, provider, and model are required", domain.ErrValidation)
	}
	return nil
}

func (gateway *BudgetedGateway) budget() modelBudget {
	return modelBudget{budgets: gateway.Budgets, pricer: gateway.Pricer, promptTokens: gateway.PromptTokens}
}

// Generate reserves the call's budget, runs it, and settles the confirmed usage.
func (gateway *BudgetedGateway) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if err := gateway.validate(selection, request); err != nil {
		return domain.ModelResult{}, err
	}
	budget := gateway.budget()
	reservation, err := budget.reserve(ctx, request.RequestID, selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	result, callErr := gateway.Inner.Generate(ctx, selection, request)
	return result, errors.Join(callErr, budget.settle(ctx, reservation, selectionReference(selection), result.Usage))
}

// Stream reserves the call's budget and settles it when the stream finishes,
// from the usage its frames confirmed.
func (gateway *BudgetedGateway) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	if err := gateway.validate(selection, request); err != nil {
		return nil, err
	}
	budget := gateway.budget()
	reservation, err := budget.reserve(ctx, request.RequestID, selection, request)
	if err != nil {
		return nil, err
	}
	stream, err := gateway.Inner.Stream(ctx, selection, request)
	if err == nil && stream == nil {
		err = errors.New("model gateway returned a nil stream")
	}
	if err != nil {
		return nil, errors.Join(err, budget.settle(ctx, reservation, selectionReference(selection), domain.Usage{}))
	}
	return &leasedModelStream{
		stream:     stream,
		lease:      &budgetLease{budget: budget, reservation: reservation, reference: selectionReference(selection)},
		releaseCtx: context.WithoutCancel(ctx),
	}, nil
}

// budgetLease settles the reservation exactly once, when the stream finishes.
type budgetLease struct {
	budget      modelBudget
	reservation domain.BudgetReservation
	reference   domain.ModelReference
	settled     atomic.Bool
}

func (lease *budgetLease) Pool() string { return "budget" }

func (lease *budgetLease) Release(ctx context.Context, usage domain.Usage) error {
	if !lease.settled.CompareAndSwap(false, true) {
		return nil
	}
	return lease.budget.settle(ctx, lease.reservation, lease.reference, usage)
}

var _ contract.CapacityLease = (*budgetLease)(nil)
