package scope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// budgetValue is the budget binding; its field order is the canonical order.
type budgetValue struct {
	Tokens         int64  `json:"tokens"`
	CostMinorUnits int64  `json:"cost_minor_units"`
	Currency       string `json:"currency,omitempty"`
	Exponent       *int   `json:"exponent,omitempty"`
	Period         string `json:"period"`
	WindowSeconds  int64  `json:"window_seconds,omitempty"`
	Timezone       string `json:"timezone,omitempty"`
}

// DecodeBudget decodes and validates a stored budget value; an empty value is no budget, and a
// value that names no period is ErrValidation.
func DecodeBudget(raw []byte) (domain.ReleaseBudget, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return domain.ReleaseBudget{}, nil
	}
	var value budgetValue
	if err := strictDecode(raw, &value); err != nil {
		return domain.ReleaseBudget{}, fmt.Errorf("%w: budget value must be a token/cost object: %v", domain.ErrValidation, err)
	}
	if value.Period == "" {
		return domain.ReleaseBudget{}, errNoPeriod
	}
	if value.WindowSeconds < 0 || value.WindowSeconds > int64(domain.MaxBudgetWindow/time.Second) {
		return domain.ReleaseBudget{}, fmt.Errorf("%w: budget window_seconds is outside 60 to %d", domain.ErrValidation, int64(domain.MaxBudgetWindow/time.Second))
	}
	budget := domain.ReleaseBudget{
		Tokens: value.Tokens, CostMinorUnits: value.CostMinorUnits, Currency: value.Currency, Exponent: value.Exponent,
		Period: domain.BudgetPeriod{
			Kind: domain.BudgetPeriodKind(value.Period), Window: time.Duration(value.WindowSeconds) * time.Second, Timezone: value.Timezone,
		},
		Source: domain.BudgetSourceRelease,
	}
	if err := budget.Period.Validate(); err != nil {
		return domain.ReleaseBudget{}, err
	}
	if value.Tokens < 0 || value.Tokens > domain.MaxBudgetAmount || value.CostMinorUnits < 0 || value.CostMinorUnits > domain.MaxBudgetAmount {
		return domain.ReleaseBudget{}, fmt.Errorf("%w: budget tokens and cost_minor_units must be from 0 to %d", domain.ErrValidation, domain.MaxBudgetAmount)
	}
	if value.CostMinorUnits == 0 {
		if value.Currency != "" || value.Exponent != nil {
			return domain.ReleaseBudget{}, fmt.Errorf("%w: a budget without a cost ceiling takes no currency or exponent", domain.ErrValidation)
		}
		return budget, nil
	}
	if !currencyCode(value.Currency) {
		return domain.ReleaseBudget{}, fmt.Errorf("%w: a cost ceiling needs a three-letter upper-case currency", domain.ErrValidation)
	}
	if value.Exponent == nil || *value.Exponent < 0 || *value.Exponent > domain.MaxCurrencyExponent {
		return domain.ReleaseBudget{}, fmt.Errorf("%w: a cost ceiling needs its currency's exponent from 0 to %d", domain.ErrValidation, domain.MaxCurrencyExponent)
	}
	return budget, nil
}

var errNoPeriod = fmt.Errorf("%w: a budget must name its period: turn, rolling, day or month", domain.ErrValidation)

func currencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, letter := range code {
		if letter < 'A' || letter > 'Z' {
			return false
		}
	}
	return true
}

func encodeBudget(budget domain.ReleaseBudget) ([]byte, error) {
	return json.Marshal(budgetValue{
		Tokens: budget.Tokens, CostMinorUnits: budget.CostMinorUnits, Currency: budget.Currency, Exponent: budget.Exponent,
		Period: string(budget.Period.Kind), WindowSeconds: int64(budget.Period.Window / time.Second), Timezone: budget.Period.Timezone,
	})
}

// canonicalBudget is the strict write path: a new value names its period and caps something.
func canonicalBudget(raw []byte) ([]byte, error) {
	budget, err := DecodeBudget(raw)
	if err != nil {
		return nil, err
	}
	if !budget.Bounded() {
		return nil, fmt.Errorf("%w: a budget must cap tokens or cost; remove the binding instead", domain.ErrValidation)
	}
	return encodeBudget(budget)
}

// compatibleBudgets reports whether child counts over the same span and unit as a bounded
// parent: the same period, window, and timezone, and the same currency and exponent when both
// cap cost.
func compatibleBudgets(parent, child domain.ReleaseBudget) bool {
	if parent.Period != child.Period {
		return false
	}
	return parent.CostMinorUnits == 0 || child.CostMinorUnits == 0 ||
		parent.Currency == child.Currency && sameExponent(parent.Exponent, child.Exponent)
}

func sameExponent(left, right *int) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func incompatibleBudget(kind domain.ResourceKind, parent domain.ReleaseBudget) error {
	return fmt.Errorf("%w: %s must keep the inherited %s period, window, timezone, and currency unit", domain.ErrAuthorityExceeded, kind, parent.Period.Kind)
}

func checkBudgetAtMost(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := DecodeBudget(inherited)
	if err != nil {
		return err
	}
	child, err := DecodeBudget(candidate)
	if err != nil {
		return err
	}
	if !parent.Bounded() {
		return nil
	}
	if !compatibleBudgets(parent, child) {
		return incompatibleBudget(kind, parent)
	}
	if parent.Tokens > 0 && (child.Tokens == 0 || child.Tokens > parent.Tokens) {
		return fmt.Errorf("%w: %s raises the token ceiling above %d", domain.ErrAuthorityExceeded, kind, parent.Tokens)
	}
	if parent.CostMinorUnits > 0 && (child.CostMinorUnits == 0 || child.CostMinorUnits > parent.CostMinorUnits) {
		return fmt.Errorf("%w: %s raises the cost ceiling above %d", domain.ErrAuthorityExceeded, kind, parent.CostMinorUnits)
	}
	return nil
}

func mergeBudget(inherited, override []byte) ([]byte, error) {
	parent, err := DecodeBudget(inherited)
	if err != nil {
		return nil, err
	}
	child, err := DecodeBudget(override)
	if err != nil {
		return nil, err
	}
	if child.Period.Kind == "" {
		return nil, errNoPeriod
	}
	if !parent.Bounded() {
		return encodeBudget(child)
	}
	if !compatibleBudgets(parent, child) {
		return nil, incompatibleBudget(domain.ResourceBudget, parent)
	}
	merged := child
	if parent.Tokens > 0 {
		merged.Tokens = minPositive(parent.Tokens, child.Tokens)
	}
	if parent.CostMinorUnits > 0 {
		merged.CostMinorUnits = minPositive(parent.CostMinorUnits, child.CostMinorUnits)
	}
	if child.CostMinorUnits == 0 && merged.CostMinorUnits > 0 {
		merged.Currency, merged.Exponent = parent.Currency, parent.Exponent
	}
	return encodeBudget(merged)
}

// QCurrencyExponent reads a currency's catalog exponent; merge CurrencyQueries into the map of
// whichever query service passes to CheckBudgetUnit.
const QCurrencyExponent = "scout_scope_currency_exponent"

var CurrencyQueries = map[string]string{QCurrencyExponent: `SELECT exponent FROM currency WHERE code = ?`}

// CheckBudgetUnit verifies a cost ceiling's currency and frozen exponent against the catalog and
// returns the exponent to report; a budget without a cost ceiling needs no lookup. A frozen
// exponent the catalog no longer has fails closed rather than reinterpreting the ceiling.
func CheckBudgetUnit(ctx context.Context, qs keelport.QueryService, budget domain.ReleaseBudget) (int, error) {
	if budget.CostMinorUnits == 0 {
		return 0, nil
	}
	if budget.Currency == "" {
		return 0, &domain.BudgetUnenforceableError{Problem: domain.BudgetNoCurrency, Detail: "the cost ceiling names no currency"}
	}
	if qs == nil {
		return 0, fmt.Errorf("budget unit check: query service is required")
	}
	result, err := qs.Query(ctx, QCurrencyExponent, budget.Currency)
	if err != nil {
		return 0, fmt.Errorf("read currency %s: %w", budget.Currency, err)
	}
	if len(result.Rows) == 0 {
		return 0, &domain.BudgetUnenforceableError{Problem: domain.BudgetUnknownCurrency, Detail: "currency " + budget.Currency + " is not in the currency catalog"}
	}
	catalog, ok := common.AsInt64OK(result.Rows[0][0])
	if !ok {
		return 0, fmt.Errorf("read currency %s: invalid exponent %q", budget.Currency, common.AsString(result.Rows[0][0]))
	}
	if budget.Exponent != nil && int64(*budget.Exponent) != catalog {
		return 0, &domain.BudgetUnenforceableError{Problem: domain.BudgetExponentChanged,
			Detail: "currency " + budget.Currency + " has exponent " + strconv.FormatInt(catalog, 10) + ", not " + strconv.Itoa(*budget.Exponent)}
	}
	return int(catalog), nil
}
