package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/toolgateway"
)

type planeProviderFactory struct{}

func (planeProviderFactory) Build(context.Context, domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	return nil, nil, domain.ErrNotReady
}

func composablePlane() *BaseDataPlane {
	return &BaseDataPlane{
		DB: loopTableDB{table: &loopTableFake{rows: map[string][]any{}}}, Cache: cache.NewMemoryCacheService(),
		Storage: &fake.ObjectStorage{}, Providers: planeProviderFactory{}, Transport: &toolgateway.InProcessTransport{},
		Budget: &fake.TenantBudgetManager{}, RateLimiter: &fake.TenantRateLimiter{}, Metrics: &fake.RuntimeMetrics{},
		Pricer: fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
			return 0, "USD", nil
		}),
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return nil, domain.ErrNotReady
		}),
		Settings: domain.DataPlaneSettings{
			StateBucket: "agent-state", StateMaxBytes: 1 << 20, QueuePartitions: 8, QueueShards: 2, QueueMaxAttempts: 3,
			SessionCacheSize: 16, SessionCacheTTL: time.Minute, GraphCacheSize: 16, GraphCacheTTL: time.Minute,
			StepClaimLease: 2 * time.Minute, TurnMaxSteps: 4, ToolTimeout: time.Second, ToolMaxAttempts: 2,
			GuardrailMaxInputBytes: 1024, GuardrailMaxOutputBytes: 1024,
		},
		Limits:   domain.ToolLoopLimits{MaxIterations: 4, MaxToolCalls: 8, MaxTokens: 1000, MaxRepeatedCalls: 2, Deadline: time.Minute},
		Currency: "USD", UsageCategory: "agent", MaxOutputTokens: 512,
	}
}

func TestBaseDataPlaneComposesEveryDefault(t *testing.T) {
	plane := composablePlane()
	if err := plane.Compose(); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	var composed contract.DataPlane = plane
	if composed.Runtime() == nil || composed.Scheduler() == nil || composed.Ingress() == nil || composed.Replies() == nil || composed.Canceller() == nil {
		t.Fatal("a composed data plane exposes every surface")
	}
	worker, err := plane.Worker("worker-1", time.Minute, 4)
	if err != nil || worker.Runtime != plane.TurnRuntime {
		t.Fatalf("Worker: %v", err)
	}
}

func TestBaseDataPlaneKeepsAnInjectedCollaborator(t *testing.T) {
	plane := composablePlane()
	estimator := &fake.TurnBudgetEstimator{}
	plane.Estimator = estimator
	if err := plane.Compose(); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	if plane.Estimator != contract.TurnBudgetEstimator(estimator) || plane.TurnRuntime.(*TurnRuntime).Estimator != contract.TurnBudgetEstimator(estimator) {
		t.Fatal("the injected estimator must reach the runtime")
	}
}

func TestBaseDataPlaneRefusesToCompose(t *testing.T) {
	cases := map[string]struct {
		mutate func(*BaseDataPlane)
		want   error
		text   string
	}{
		"no state bucket":         {func(p *BaseDataPlane) { p.Settings.StateBucket = " " }, domain.ErrNotReady, "agent_state_bucket"},
		"missing collaborator":    {func(p *BaseDataPlane) { p.Budget = nil }, domain.ErrValidation, "budget manager"},
		"claim lease under loop":  {func(p *BaseDataPlane) { p.Settings.StepClaimLease = time.Second }, domain.ErrValidation, "agent_step_claim_lease"},
		"shards above partitions": {func(p *BaseDataPlane) { p.Settings.QueueShards = 99 }, domain.ErrValidation, "shards"},
		"foreign transport without credentials": {func(p *BaseDataPlane) {
			p.Transport = fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
				return domain.ToolResult{}, nil
			})
		}, domain.ErrValidation, "credential provider"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plane := composablePlane()
			tc.mutate(plane)
			err := plane.Compose()
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Compose = %v, want %v mentioning %q", err, tc.want, tc.text)
			}
			if plane.Objects != nil || plane.TurnRuntime != nil {
				t.Fatal("a refused composition must leave the plane untouched")
			}
			if _, err = plane.Worker("worker-1", time.Minute, 4); !errors.Is(err, domain.ErrNotReady) {
				t.Fatalf("Worker on an uncomposed plane = %v", err)
			}
		})
	}
}
