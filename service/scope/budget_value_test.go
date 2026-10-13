package scope

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/keel/geo"

	"github.com/nauticana/scout/domain"
)

const (
	monthEUR = `{"tokens":200000,"cost_minor_units":5000,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`
	hourJPY  = `{"tokens":0,"cost_minor_units":300,"currency":"JPY","exponent":0,"period":"rolling","window_seconds":3600}`
	turnOnly = `{"tokens":20000,"cost_minor_units":0,"period":"turn"}`
)

func TestBudgetWithoutAPeriodIsRefusedAtCompile(t *testing.T) {
	periodless := `{"tokens":1000,"cost_minor_units":5000,"currency":"EUR"}`
	for name, chain := range map[string][]domain.ScopedBinding{
		"alone": {binding("agent", domain.ResourceBudget, periodless)},
		"under a period": {
			binding("tenant", domain.ResourceBudget, monthEUR),
			binding("agent", domain.ResourceBudget, periodless),
		},
		"over a period": {
			binding("tenant", domain.ResourceBudget, periodless),
			binding("agent", domain.ResourceBudget, monthEUR),
		},
		"empty value": {
			binding("tenant", domain.ResourceBudget, turnOnly),
			binding("agent", domain.ResourceBudget, ``),
		},
	} {
		if _, err := compile(t, chain); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	release, err := compile(t, []domain.ScopedBinding{
		binding("tenant", domain.ResourceBudget, monthEUR),
		binding("agent", domain.ResourceBudget, `{"tokens":400,"cost_minor_units":0,"period":"month","timezone":"Europe/Istanbul"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tokens":400,"cost_minor_units":5000,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`
	if value := resource(t, release, domain.ResourceBudget).Value; string(value) != want {
		t.Fatalf("compiled = %s, want %s", value, want)
	}
}

func TestCanonicalBudgetNamesItsPeriodAndKeepsAZeroExponent(t *testing.T) {
	for raw, want := range map[string]string{
		monthEUR: monthEUR,
		`{"period":"rolling","window_seconds":3600,"exponent":0,"currency":"JPY","cost_minor_units":300,"tokens":0}`: hourJPY,
		`{"period":"turn","tokens":20000}`:                                           turnOnly,
		`{"tokens":5,"cost_minor_units":0,"period":"day","timezone":"Asia/Kolkata"}`: `{"tokens":5,"cost_minor_units":0,"period":"day","timezone":"Asia/Kolkata"}`,
	} {
		canonical, err := CanonicalValue(domain.ResourceBudget, []byte(raw))
		if err != nil || string(canonical) != want {
			t.Errorf("%s: canonical = %s, %v; want %s", raw, canonical, err, want)
		}
		again, err := CanonicalValue(domain.ResourceBudget, canonical)
		if err != nil || string(again) != string(canonical) {
			t.Errorf("%s: canonical bytes are not stable: %s", raw, again)
		}
	}
	budget, err := DecodeBudget([]byte(hourJPY))
	if err != nil || budget.Exponent == nil || *budget.Exponent != 0 || budget.Period.Window != time.Hour || budget.Source != domain.BudgetSourceRelease {
		t.Fatalf("JPY = %+v, %v", budget, err)
	}
}

func TestCanonicalBudgetRefusesIncompleteOrMalformedValues(t *testing.T) {
	for name, raw := range map[string]string{
		"no period":              `{"tokens":100,"cost_minor_units":0}`,
		"empty period":           `{"tokens":100,"cost_minor_units":0,"period":""}`,
		"no period, with cost":   `{"tokens":0,"cost_minor_units":10,"currency":"EUR","exponent":2}`,
		"no period, with window": `{"tokens":10,"cost_minor_units":0,"window_seconds":3600}`,
		"unbounded":              `{"tokens":0,"cost_minor_units":0,"period":"turn"}`,
		"empty":                  ``,
		"cost without currency":  `{"tokens":0,"cost_minor_units":10,"exponent":2,"period":"turn"}`,
		"cost without exponent":  `{"tokens":0,"cost_minor_units":10,"currency":"EUR","period":"turn"}`,
		"negative exponent":      `{"tokens":0,"cost_minor_units":10,"currency":"EUR","exponent":-1,"period":"turn"}`,
		"exponent above six":     `{"tokens":0,"cost_minor_units":10,"currency":"EUR","exponent":7,"period":"turn"}`,
		"lower-case currency":    `{"tokens":0,"cost_minor_units":10,"currency":"eur","exponent":2,"period":"turn"}`,
		"currency without cost":  `{"tokens":10,"cost_minor_units":0,"currency":"EUR","period":"turn"}`,
		"exponent without cost":  `{"tokens":10,"cost_minor_units":0,"exponent":2,"period":"turn"}`,
		"amount above maximum":   `{"tokens":1000000000000000001,"cost_minor_units":0,"period":"turn"}`,
		"negative tokens":        `{"tokens":-1,"cost_minor_units":0,"period":"turn"}`,
		"unknown field":          `{"tokens":10,"cost_minor_units":0,"period":"turn","reset":"never"}`,
		"trailing JSON":          `{"tokens":10,"cost_minor_units":0,"period":"turn"}{}`,
		"unknown period":         `{"tokens":10,"cost_minor_units":0,"period":"week"}`,
		"rolling without window": `{"tokens":10,"cost_minor_units":0,"period":"rolling"}`,
		"huge window":            `{"tokens":10,"cost_minor_units":0,"period":"rolling","window_seconds":9223372036854775807}`,
		"day without timezone":   `{"tokens":10,"cost_minor_units":0,"period":"day"}`,
		"local timezone":         `{"tokens":10,"cost_minor_units":0,"period":"month","timezone":"Local"}`,
		"no period, with zone":   `{"tokens":10,"cost_minor_units":0,"timezone":"UTC"}`,
	} {
		if _, err := CanonicalValue(domain.ResourceBudget, []byte(raw)); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	if _, err := DecodeBudget([]byte(`{"tokens":100,"cost_minor_units":0}`)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("decoding a value without a period = %v, want validation", err)
	}
}

func TestBudgetNarrowingKeepsThePeriodAndUnit(t *testing.T) {
	checker := &LatticeChecker{}
	refused := map[string][2]string{
		"month under day":       {`{"tokens":10,"cost_minor_units":0,"period":"day","timezone":"UTC"}`, `{"tokens":5,"cost_minor_units":0,"period":"month","timezone":"UTC"}`},
		"turn under month":      {monthEUR, `{"tokens":5,"cost_minor_units":10,"currency":"EUR","exponent":2,"period":"turn"}`},
		"shorter rolling":       {`{"tokens":10,"cost_minor_units":0,"period":"rolling","window_seconds":86400}`, `{"tokens":5,"cost_minor_units":0,"period":"rolling","window_seconds":3600}`},
		"another timezone":      {monthEUR, `{"tokens":100,"cost_minor_units":10,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Berlin"}`},
		"another currency":      {monthEUR, `{"tokens":100,"cost_minor_units":10,"currency":"USD","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`},
		"another exponent":      {monthEUR, `{"tokens":100,"cost_minor_units":10,"currency":"EUR","exponent":3,"period":"month","timezone":"Europe/Istanbul"}`},
		"raised tokens":         {monthEUR, `{"tokens":200001,"cost_minor_units":10,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`},
		"dropped cost ceiling":  {monthEUR, `{"tokens":100,"cost_minor_units":0,"period":"month","timezone":"Europe/Istanbul"}`},
		"dropped token ceiling": {monthEUR, `{"tokens":0,"cost_minor_units":10,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`},
	}
	for name, pair := range refused {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceBudget, []byte(pair[0]), []byte(pair[1])); !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s: error = %v, want authority exceeded", name, err)
		}
	}
	allowed := map[string][2]string{
		"lower ceilings":       {monthEUR, `{"tokens":100,"cost_minor_units":10,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`},
		"cost under tokens":    {`{"tokens":100,"cost_minor_units":0,"period":"turn"}`, `{"tokens":100,"cost_minor_units":10,"currency":"USD","exponent":2,"period":"turn"}`},
		"anything, unbounded":  {`{"tokens":0,"cost_minor_units":0,"period":"turn"}`, hourJPY},
		"anything, no parent":  {``, monthEUR},
		"same rolling, lower":  {hourJPY, `{"tokens":0,"cost_minor_units":299,"currency":"JPY","exponent":0,"period":"rolling","window_seconds":3600}`},
		"same timezone, equal": {monthEUR, monthEUR},
	}
	for name, pair := range allowed {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceBudget, []byte(pair[0]), []byte(pair[1])); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, pair := range map[string][2]string{
		"no period under a period": {monthEUR, `{"tokens":50,"cost_minor_units":0}`},
		"a period under no period": {`{"tokens":100,"cost_minor_units":0}`, turnOnly},
	} {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceBudget, []byte(pair[0]), []byte(pair[1])); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
}

func TestBudgetMergerKeepsTheTighterValueOfOnePeriod(t *testing.T) {
	merge := func(parent, child string) ([]byte, error) {
		return BudgetMerger{}.Merge(context.Background(), []byte(parent), domain.ScopedBinding{Value: []byte(child), MergeMode: domain.MergeReplace})
	}
	merged, err := merge(monthEUR, `{"tokens":300000,"cost_minor_units":0,"period":"month","timezone":"Europe/Istanbul"}`)
	if err != nil || string(merged) != `{"tokens":200000,"cost_minor_units":5000,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}` {
		t.Fatalf("merged = %s, %v", merged, err)
	}
	merged, err = merge(`{"tokens":100,"cost_minor_units":0,"period":"turn"}`, `{"tokens":0,"cost_minor_units":10,"currency":"JPY","exponent":0,"period":"turn"}`)
	if err != nil || string(merged) != `{"tokens":100,"cost_minor_units":10,"currency":"JPY","exponent":0,"period":"turn"}` {
		t.Fatalf("merged cost under tokens = %s, %v", merged, err)
	}
	merged, err = merge(`{"tokens":0,"cost_minor_units":0,"period":"day","timezone":"UTC"}`, turnOnly)
	if err != nil || string(merged) != turnOnly {
		t.Fatalf("merged under an unbounded parent = %s, %v", merged, err)
	}
	for name, child := range map[string]string{
		"another period":   turnOnly,
		"another currency": `{"tokens":1,"cost_minor_units":1,"currency":"USD","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`,
	} {
		if _, err := merge(monthEUR, child); !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s: error = %v, want authority exceeded", name, err)
		}
	}
	for name, pair := range map[string][2]string{
		"no period, no parent":     {``, `{"tokens":1,"cost_minor_units":0}`},
		"no period under a period": {turnOnly, `{"tokens":1,"cost_minor_units":0}`},
		"a period under no period": {`{"tokens":1,"cost_minor_units":0}`, turnOnly},
		"empty child":              {turnOnly, ``},
	} {
		if _, err := merge(pair[0], pair[1]); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
}

type currencyCatalogFake struct {
	exponents map[string]int
	reads     int
}

func (f *currencyCatalogFake) Exponent(_ context.Context, code string) (int, error) {
	f.reads++
	exponent, ok := f.exponents[code]
	if !ok {
		return 0, fmt.Errorf("%w %q", geo.ErrUnknownCurrency, code)
	}
	return exponent, nil
}

func exponent(value int) *int { return &value }

func TestCheckBudgetUnitAgainstTheCatalog(t *testing.T) {
	catalog := &currencyCatalogFake{exponents: map[string]int{"EUR": 2, "JPY": 0}}
	if got, err := CheckBudgetUnit(context.Background(), catalog, domain.ReleaseBudget{Tokens: 10}); err != nil || got != 0 || catalog.reads != 0 {
		t.Fatalf("tokens only = %d, %v after %d reads", got, err, catalog.reads)
	}
	if got, err := CheckBudgetUnit(context.Background(), catalog, domain.ReleaseBudget{CostMinorUnits: 5, Currency: "JPY", Exponent: exponent(0)}); err != nil || got != 0 {
		t.Fatalf("JPY = %d, %v", got, err)
	}
	if got, err := CheckBudgetUnit(context.Background(), catalog, domain.ReleaseBudget{CostMinorUnits: 5, Currency: "EUR", Source: domain.BudgetSourceFallback}); err != nil || got != 2 {
		t.Fatalf("a fallback without an exponent takes the catalog's: %d, %v", got, err)
	}
	for problem, budget := range map[domain.BudgetProblem]domain.ReleaseBudget{
		domain.BudgetNoCurrency:      {CostMinorUnits: 5},
		domain.BudgetUnknownCurrency: {CostMinorUnits: 5, Currency: "XXX", Exponent: exponent(2)},
		domain.BudgetExponentChanged: {CostMinorUnits: 5, Currency: "EUR", Exponent: exponent(3)},
	} {
		_, err := CheckBudgetUnit(context.Background(), catalog, budget)
		var unenforceable *domain.BudgetUnenforceableError
		if !errors.As(err, &unenforceable) || unenforceable.Problem != problem || !errors.Is(err, domain.ErrDegraded) {
			t.Errorf("%s: error = %v", problem, err)
		}
	}
}

func TestCurrenciesDefaultsToKeelsCatalog(t *testing.T) {
	composed := &currencyCatalogFake{}
	if Currencies(composed, nil) != composed {
		t.Fatal("a composed catalog must be kept")
	}
	if _, ok := Currencies(nil, nil).(*geo.Currencies); !ok {
		t.Fatal("no catalog must default to keel's")
	}
	if _, err := CheckBudgetUnit(context.Background(), nil, domain.ReleaseBudget{CostMinorUnits: 5, Currency: "EUR"}); err == nil {
		t.Fatal("a cost ceiling without a catalog must fail closed")
	}
}
