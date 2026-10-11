package controlplane

import (
	"context"
	"errors"
	"fmt"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// qCurrencyExponent is the catalog read scope.CheckBudgetUnit makes through Studio's query maps.
const qCurrencyExponent = scope.QCurrencyExponent

// checkSavedBudget refuses a cost ceiling whose currency the catalog does not hold or whose
// exponent is not the catalog's, so a client that misreads a currency's decimals is caught.
func (s *StudioService) checkSavedBudget(ctx context.Context, value []byte) error {
	budget, err := scope.DecodeBudget(value)
	if err != nil || budget.CostMinorUnits == 0 {
		return err
	}
	if err = s.init(ctx); err != nil {
		return err
	}
	_, err = scope.CheckBudgetUnit(ctx, s.qs, budget)
	var unenforceable *domain.BudgetUnenforceableError
	if errors.As(err, &unenforceable) {
		return validationError("value", unenforceable.Detail)
	}
	return err
}

// checkBudget refuses to freeze a cost ceiling the runtime could not enforce: one whose unit
// the catalog no longer matches, or one over a selected model that is not priced in its currency.
// A tokens-only budget needs neither.
func (s *StudioService) checkBudget(ctx context.Context, qs keelport.QueryService, resources []domain.EffectiveResource, definition domain.AgentDefinition) error {
	var budget domain.ReleaseBudget
	for _, resource := range resources {
		if resource.ResourceKind != domain.ResourceBudget {
			continue
		}
		decoded, err := scope.DecodeBudget(resource.Value)
		if err != nil {
			return err
		}
		budget = decoded
	}
	if budget.CostMinorUnits == 0 {
		return nil
	}
	_, err := scope.CheckBudgetUnit(ctx, qs, budget)
	var unenforceable *domain.BudgetUnenforceableError
	if errors.As(err, &unenforceable) {
		message := "budget: " + unenforceable.Detail
		if unenforceable.Problem == domain.BudgetExponentChanged {
			message += "; re-save the budget"
		}
		return validationError("settings", message)
	}
	if err != nil {
		return err
	}
	if s.Pricer == nil {
		return fmt.Errorf("%w: no model pricer is composed to check a cost budget", domain.ErrNotReady)
	}
	for _, reference := range selectedModels(definition.Models) {
		name := reference.ProviderID + "/" + reference.ModelID
		_, currency, err := s.Pricer.Cost(ctx, reference, domain.ModelUsage{})
		if errors.Is(err, domain.ErrNotFound) {
			return validationError("settings", "model "+name+" has no price; a cost budget needs every selected model priced")
		}
		if err != nil {
			return fmt.Errorf("price model %s: %w", name, err)
		}
		if currency != "" && currency != budget.Currency {
			return validationError("settings", "model "+name+" is priced in "+currency+"; the cost budget is in "+budget.Currency)
		}
	}
	return nil
}
