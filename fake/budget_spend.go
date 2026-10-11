package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BudgetSpend is a configurable contract.PrincipalBudgetSpend.
type BudgetSpend struct {
	SpendFunc func(context.Context, int64, domain.PrincipalRef, domain.ReleaseBudget) (domain.BudgetSpend, error)
}

// Spend invokes SpendFunc when configured; the default reports nothing spent.
func (s *BudgetSpend) Spend(ctx context.Context, tenantID int64, principal domain.PrincipalRef, budget domain.ReleaseBudget) (domain.BudgetSpend, error) {
	if s.SpendFunc != nil {
		return s.SpendFunc(ctx, tenantID, principal, budget)
	}
	return domain.BudgetSpend{}, nil
}

var _ contract.PrincipalBudgetSpend = (*BudgetSpend)(nil)
