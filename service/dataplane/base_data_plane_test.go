package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/toolgateway"
)

type planeProviderFactory struct{}

type planeProviderFactoryFunc func(context.Context, domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error)

func (function planeProviderFactoryFunc) Build(ctx context.Context, model domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	return function(ctx, model)
}

func (planeProviderFactory) Build(context.Context, domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
	return nil, nil, domain.ErrNotReady
}

func composablePlane() *BaseDataPlane {
	return &BaseDataPlane{
		DB: loopTableDB{table: &loopTableFake{rows: map[string][]any{}}}, Cache: cache.NewMemoryCacheService(),
		Storage: &fake.ObjectStorage{Name: "agent-state"}, Providers: planeProviderFactory{}, Transport: &toolgateway.InProcessTransport{},
		Budget: &fake.TenantBudgetManager{}, RateLimiter: &fake.TenantRateLimiter{}, Metrics: &fake.RuntimeMetrics{},
		Pricer: fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
			return 0, "USD", nil
		}),
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return nil, domain.ErrNotReady
		}),
		Settings: domain.DataPlaneSettings{
			StateBucket: "agent-state", StateMaxBytes: 1 << 20, QueuePartitions: 8, QueueShards: 2, QueueMaxAttempts: 3,
			QueueLease: 5 * time.Minute, QueueBatch: 4, ModelRegion: "eu-central",
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
	worker, err := plane.Worker("worker-1", 2*time.Minute, 4)
	if err != nil || worker.Runtime != plane.TurnRuntime {
		t.Fatalf("Worker: %v", err)
	}
	if _, err = plane.Worker("worker-1", time.Minute, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Worker must reject a lease that cannot cover the loop deadline, got %v", err)
	}
	if _, err = plane.Worker("worker-1", -time.Minute, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Worker must reject a negative lease instead of taking the configured one, got %v", err)
	}
}

// catalogTableDB answers the candidate catalog's query set; every other set falls back to loopTableFake.
type catalogTableDB struct {
	loopTableDB
	capabilities []string
}

func (db catalogTableDB) GetQueryService(ctx context.Context, queries map[string]string) keelport.QueryService {
	for _, sql := range queries {
		if strings.Contains(sql, "model_capability") {
			return &catalogTableFake{queries: queries, capabilities: db.capabilities}
		}
	}
	return db.loopTableDB.GetQueryService(ctx, queries)
}

type catalogTableFake struct {
	queries      map[string]string
	capabilities []string
}

func (table *catalogTableFake) Query(_ context.Context, name string, _ ...any) (*keelmodel.QueryResult, error) {
	if strings.Contains(table.queries[name], "model_capability") {
		rows := make([][]any, 0, len(table.capabilities))
		for _, capability := range table.capabilities {
			rows = append(rows, []any{"anthropic", "claude", capability})
		}
		return &keelmodel.QueryResult{Rows: rows}, nil
	}
	return &keelmodel.QueryResult{Rows: [][]any{{"anthropic", "claude", int64(200000), int64(8192), "", "", "", int64(0), true}}}, nil
}

func (*catalogTableFake) GenID() int64 { return 0 }

func toolCapablePlane(t *testing.T, capabilities ...string) *BaseDataPlane {
	t.Helper()
	plane := composablePlane()
	plane.DB = catalogTableDB{loopTableDB: loopTableDB{table: &loopTableFake{rows: map[string][]any{}}}, capabilities: capabilities}
	plane.Providers = planeProviderFactoryFunc(func(context.Context, domain.ModelReference) (contract.ModelProvider, contract.MediaProvider, error) {
		return &fake.ModelProvider{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{Output: []byte("answer"), FinishReason: "stop"}, nil
		}}, nil, nil
	})
	plane.Capacity = fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
		return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(context.Context, domain.Usage) error { return nil }}, nil
	})
	if err := plane.Compose(); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	return plane
}

func toolCapableRequest() domain.ModelRequest {
	return domain.ModelRequest{
		TenantContext: domain.TenantContext{TenantID: 7}, RequestID: "request-1", MaxOutputTokens: 256,
		Model:  domain.ModelReference{ProviderID: "anthropic", ModelID: "claude"},
		Prompt: []byte("rank this"),
		Tools:  []domain.ModelTool{{Name: "search", ToolID: "search", ToolVersion: "1", InputSchema: []byte(`{"type":"object"}`)}},
	}
}

func TestBaseDataPlaneRoutesAndConfirmsAToolCapableRequest(t *testing.T) {
	plane := toolCapablePlane(t, domain.CapabilityTools)
	ctx, request := context.Background(), toolCapableRequest()
	selection, err := plane.Router.Select(ctx, request)
	if err != nil || selection.Model != "claude" || selection.Region != "eu-central" {
		t.Fatalf("Select = %+v, %v", selection, err)
	}
	result, err := plane.Models.Generate(ctx, selection, request)
	if err != nil || string(result.Output) != "answer" {
		t.Fatalf("Generate through the default gateway = %+v, %v", result, err)
	}
}

func TestBaseDataPlaneRefusesAToolCallOnARouteWithoutTools(t *testing.T) {
	plane := toolCapablePlane(t, "vision")
	ctx, request := context.Background(), toolCapableRequest()
	selection := domain.ModelSelection{Provider: "anthropic", Model: "claude", RouteID: "anthropic/claude"}
	if _, err := plane.Models.Generate(ctx, selection, request); !errors.Is(err, domain.ErrCapabilityUnsupported) ||
		!strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("Generate = %v, want the catalog to refuse the route", err)
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
		"no state bucket":           {func(p *BaseDataPlane) { p.Settings.StateBucket = " " }, domain.ErrNotReady, "agent_state_bucket"},
		"storage of another bucket": {func(p *BaseDataPlane) { p.Storage = &fake.ObjectStorage{Name: "public-assets"} }, domain.ErrValidation, "agent_state_bucket"},
		"missing collaborator":      {func(p *BaseDataPlane) { p.Budget = nil }, domain.ErrValidation, "budget manager"},
		"claim lease under loop":    {func(p *BaseDataPlane) { p.Settings.StepClaimLease = time.Second }, domain.ErrValidation, "agent_step_claim_lease"},
		"queue lease under loop":    {func(p *BaseDataPlane) { p.Settings.QueueLease = time.Second }, domain.ErrValidation, "agent_queue_lease"},
		"no queue batch":            {func(p *BaseDataPlane) { p.Settings.QueueBatch = 0 }, domain.ErrValidation, "agent_queue_batch"},
		"shards above partitions":   {func(p *BaseDataPlane) { p.Settings.QueueShards = 99 }, domain.ErrValidation, "shards"},
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
