package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

// TestIsolationConstructorsRejectUnsafeLimits keeps every configured limit explicit: a negative or
// zero value must fail loudly instead of silently taking a default.
func TestIsolationConstructorsRejectUnsafeLimits(t *testing.T) {
	ctx := context.Background()
	usage := domain.Usage{CostMinorUnits: 1, Currency: "EUR"}
	cases := []struct {
		name string
		run  func() error
	}{
		{"loop detector without threshold", func() error {
			return (&MemoryLoopDetector{MaxConversations: 4}).Observe(ctx, 1, "c", "f")
		}},
		{"loop detector without conversation cap", func() error {
			return (&MemoryLoopDetector{Threshold: 2}).Observe(ctx, 1, "c", "f")
		}},
		{"loop detector with negative window", func() error {
			return (&MemoryLoopDetector{Threshold: 2, MaxConversations: 4, Window: -time.Second}).Observe(ctx, 1, "c", "f")
		}},
		{"loop detector with negative fingerprint cap", func() error {
			return (&MemoryLoopDetector{Threshold: 2, MaxConversations: 4, MaxFingerprints: -1}).Observe(ctx, 1, "c", "f")
		}},
		{"cost breaker without window", func() error {
			return (&WindowedCostBreaker{TenantLimit: 10, Currency: "EUR", MaxEntries: 4}).Allow(ctx, 1, "a", 1)
		}},
		{"cost breaker with negative limit", func() error {
			return (&WindowedCostBreaker{TenantLimit: -1, Currency: "EUR", Window: time.Minute, MaxEntries: 4}).Allow(ctx, 1, "a", 1)
		}},
		{"cost breaker with negative buckets", func() error {
			return (&WindowedCostBreaker{TenantLimit: 10, Currency: "EUR", Window: time.Minute, MaxEntries: 4, Buckets: -1}).Record(ctx, 1, "a", usage)
		}},
		{"cost breaker without entry cap", func() error {
			return (&WindowedCostBreaker{TenantLimit: 10, Currency: "EUR", Window: time.Minute}).Allow(ctx, 1, "a", 1)
		}},
		{"cost breaker window smaller than buckets", func() error {
			return (&WindowedCostBreaker{TenantLimit: 10, Currency: "EUR", Window: time.Nanosecond, MaxEntries: 4, Buckets: 6}).Allow(ctx, 1, "a", 1)
		}},
		{"execution governor without collaborators", func() error {
			_, err := (&ExecutionGovernor{}).Start(ctx, domain.TurnRequest{}, domain.TenantRuntimePolicy{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestExecutionGovernorRejectsUnsafePolicy(t *testing.T) {
	governor := &ExecutionGovernor{Loops: noopLoops{}, Costs: noopCosts{}}
	request := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 1}, RequestID: "r", ConversationID: "c", AgentID: "a"}
	base := domain.TenantRuntimePolicy{MaxSteps: 2, MaxTokens: 10, MaxCostMinorUnits: 5, CostCurrency: "EUR", TurnTimeout: time.Minute}
	cases := map[string]func(*domain.TenantRuntimePolicy){
		"zero steps":       func(p *domain.TenantRuntimePolicy) { p.MaxSteps = 0 },
		"zero tokens":      func(p *domain.TenantRuntimePolicy) { p.MaxTokens = 0 },
		"negative cost":    func(p *domain.TenantRuntimePolicy) { p.MaxCostMinorUnits = -1 },
		"zero timeout":     func(p *domain.TenantRuntimePolicy) { p.TurnTimeout = 0 },
		"negative timeout": func(p *domain.TenantRuntimePolicy) { p.TurnTimeout = -time.Second },
		"bad currency":     func(p *domain.TenantRuntimePolicy) { p.CostCurrency = "EU" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			policy := base
			mutate(&policy)
			if _, err := governor.Start(context.Background(), request, policy); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestBudgetLedgerRejectsNegativeReservationTTL(t *testing.T) {
	ledger := &BudgetLedger{
		DB:             budgetDBFake{query: &budgetQueryFake{}},
		Policy:         staticBudgetPolicy{limits: domain.BudgetLimits{WindowTokens: 100, WindowCostMinorUnits: 100, Currency: "EUR", Window: time.Minute}},
		ReservationTTL: -time.Second,
	}
	if _, err := ledger.Reserve(context.Background(), 1, "req", 10, 1, "EUR"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative TTL = %v", err)
	}
}

func TestRateLimiterFactoryRejectsUnsafeLimits(t *testing.T) {
	cases := map[string]RateLimiterConfig{
		"no tenant cap":      {Turn: RateLimit{PerSecond: 1, Burst: 1}},
		"negative rate":      {Turn: RateLimit{PerSecond: -1, Burst: 1}, MaxTenants: 4},
		"negative burst":     {Tool: RateLimit{PerSecond: 1, Burst: -1}, MaxTenants: 4},
		"burst without rate": {Model: RateLimit{Burst: 5}, MaxTenants: 4},
		"negative fleet":     {FleetTurn: RateLimit{PerSecond: -2, Burst: 2}, MaxTenants: 4},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewTenantRateLimiter(config); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

type noopLoops struct{}

func (noopLoops) Observe(context.Context, int64, string, string) error { return nil }
func (noopLoops) Reset(context.Context, int64, string) error           { return nil }

type noopCosts struct{}

func (noopCosts) Allow(context.Context, int64, string, int64) error         { return nil }
func (noopCosts) Record(context.Context, int64, string, domain.Usage) error { return nil }

type staticBudgetPolicy struct{ limits domain.BudgetLimits }

func (p staticBudgetPolicy) BudgetFor(context.Context, int64) (domain.BudgetLimits, error) {
	return p.limits, nil
}
