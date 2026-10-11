package dataplane

import (
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/isolation"
)

func TestBaseDataPlaneRefusesABudgetLedgerThatEnforcesNoReleaseBudget(t *testing.T) {
	plane := composablePlane()
	plane.Budget = &isolation.BudgetLedger{DB: plane.DB}
	if err := plane.Compose(); !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "BudgetLedger.Releases") {
		t.Fatalf("Compose = %v, want the missing release limits refused", err)
	}
	if plane.TurnRuntime != nil {
		t.Fatal("a refused composition must leave the plane untouched")
	}
	plane = composablePlane()
	plane.Budget = &isolation.BudgetLedger{DB: plane.DB, Releases: &fake.PrincipalLimits{}}
	if err := plane.Compose(); err != nil {
		t.Fatalf("Compose with release limits: %v", err)
	}
}
