package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func budgetConfig() LatencyBudgetConfig {
	return LatencyBudgetConfig{
		Embedding:   StageSlice{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
		Retrieval:   StageSlice{Min: 30 * time.Millisecond, Max: 200 * time.Millisecond},
		Rerank:      StageSlice{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
		PromptBuild: StageSlice{Min: 10 * time.Millisecond, Max: 100 * time.Millisecond},
		Guardrail:   StageSlice{Min: 20 * time.Millisecond, Max: 100 * time.Millisecond},
	}
}

func TestNewLatencyBudgetAllocatorRejectsInvalidConfig(t *testing.T) {
	cases := map[string]func(*LatencyBudgetConfig){
		"negative min":       func(c *LatencyBudgetConfig) { c.Retrieval.Min = -1 },
		"max below min":      func(c *LatencyBudgetConfig) { c.Rerank = StageSlice{Min: 20, Max: 10} },
		"zero prompt floor":  func(c *LatencyBudgetConfig) { c.PromptBuild.Min = 0 },
		"zero guardrail min": func(c *LatencyBudgetConfig) { c.Guardrail = StageSlice{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := budgetConfig()
			mutate(&config)
			if _, err := NewLatencyBudgetAllocator(nil, config); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestLatencyBudgetAllocate(t *testing.T) {
	// Context deadlines are absolute, so the injected clock is a frozen real instant.
	now := time.Now()
	request := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 1}, RequestID: "r1"}
	table := DefaultStageLatencyEstimate
	minimum := table.Admission + 10*time.Millisecond + table.MinGeneration + 20*time.Millisecond // 670ms

	cases := []struct {
		name        string
		ctxDeadline time.Duration
		timeout     time.Duration
		want        domain.TurnBudget
		wantErr     error
	}{
		{
			name: "no deadline at all", wantErr: domain.ErrDeadlineInfeasible,
		},
		{
			name: "below minimum path", timeout: minimum - time.Millisecond, wantErr: domain.ErrDeadlineInfeasible,
		},
		{
			name: "exactly minimum path squeezes required stages", timeout: minimum,
			want: domain.TurnBudget{Total: minimum, Generation: 600 * time.Millisecond, PromptBuild: 10 * time.Millisecond, Guardrail: 20 * time.Millisecond},
		},
		{
			name: "generous deadline funds every stage and leftover goes to generation", timeout: 3 * time.Second,
			want: domain.TurnBudget{
				Total: 3 * time.Second, Embedding: 20 * time.Millisecond, Retrieval: 80 * time.Millisecond,
				Rerank: 20 * time.Millisecond, PromptBuild: 30 * time.Millisecond, Guardrail: 60 * time.Millisecond,
				Generation: 3*time.Second - 40*time.Millisecond - 210*time.Millisecond,
			},
		},
		{
			// 40 admission + 30 prompt + 60 guardrail + 900 generation = 1030; 30ms left: embedding takes 20, retrieval floor 30 does not fit.
			name: "tight remainder skips stages below their floor", timeout: 1060 * time.Millisecond,
			want: domain.TurnBudget{
				Total: 1060 * time.Millisecond, Embedding: 20 * time.Millisecond, PromptBuild: 30 * time.Millisecond,
				Guardrail: 60 * time.Millisecond, Generation: 910 * time.Millisecond,
			},
		},
		{
			// 60ms left after required stages: embedding 20, retrieval gets the 40 that fit above its 30 floor.
			name: "partial slice above floor", timeout: 1090 * time.Millisecond,
			want: domain.TurnBudget{
				Total: 1090 * time.Millisecond, Embedding: 20 * time.Millisecond, Retrieval: 40 * time.Millisecond,
				PromptBuild: 30 * time.Millisecond, Guardrail: 60 * time.Millisecond, Generation: 900 * time.Millisecond,
			},
		},
		{
			name: "context deadline shorter than policy wins", ctxDeadline: minimum, timeout: time.Minute,
			want: domain.TurnBudget{Total: minimum, Generation: 600 * time.Millisecond, PromptBuild: 10 * time.Millisecond, Guardrail: 20 * time.Millisecond},
		},
		{
			name: "policy timeout shorter than context deadline wins", ctxDeadline: time.Minute, timeout: minimum,
			want: domain.TurnBudget{Total: minimum, Generation: 600 * time.Millisecond, PromptBuild: 10 * time.Millisecond, Guardrail: 20 * time.Millisecond},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allocator, err := NewLatencyBudgetAllocator(nil, budgetConfig())
			if err != nil {
				t.Fatal(err)
			}
			allocator.Now = func() time.Time { return now }
			ctx := context.Background()
			if tc.ctxDeadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, now.Add(tc.ctxDeadline))
				defer cancel()
			}
			budget, err := allocator.Allocate(ctx, request, domain.TenantRuntimePolicy{TurnTimeout: tc.timeout})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.want.Deadline = now.Add(tc.want.Total)
			if budget != tc.want {
				t.Fatalf("budget = %+v\nwant     %+v", budget, tc.want)
			}
			sum := budget.Generation + budget.Embedding + budget.Retrieval + budget.Rerank + budget.PromptBuild + budget.Guardrail
			if sum != budget.Total-table.Admission {
				t.Fatalf("slices sum to %s, want %s", sum, budget.Total-table.Admission)
			}
		})
	}
}

func TestLatencyBudgetDisabledOptionalStage(t *testing.T) {
	config := budgetConfig()
	config.Rerank = StageSlice{}
	allocator, err := NewLatencyBudgetAllocator(nil, config)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := allocator.Allocate(context.Background(),
		domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 1}, RequestID: "r"},
		domain.TenantRuntimePolicy{TurnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Rerank != 0 || budget.Retrieval == 0 {
		t.Fatalf("budget = %+v", budget)
	}
}

func TestLatencyBudgetRejectsInvalidEstimateAndRequest(t *testing.T) {
	allocator, err := NewLatencyBudgetAllocator(&StaticStageLatencyModel{Table: domain.StageLatencyEstimate{MinGeneration: 2 * time.Second, Generation: time.Second}}, budgetConfig())
	if err != nil {
		t.Fatal(err)
	}
	request := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 1}, RequestID: "r"}
	if _, err = allocator.Allocate(context.Background(), request, domain.TenantRuntimePolicy{TurnTimeout: time.Minute}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid estimate = %v", err)
	}
	if _, err = allocator.Allocate(context.Background(), domain.TurnRequest{}, domain.TenantRuntimePolicy{TurnTimeout: time.Minute}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid request = %v", err)
	}
}

func TestApplyBudget(t *testing.T) {
	budget := domain.TurnBudget{Embedding: 20 * time.Millisecond, Retrieval: 80 * time.Millisecond, Rerank: 20 * time.Millisecond}
	query, err := ApplyBudget(domain.KnowledgeQuery{RequestID: "r"}, budget)
	if err != nil || query.Budget != 120*time.Millisecond {
		t.Fatalf("index embeds: %s, %v", query.Budget, err)
	}
	query, err = ApplyBudget(domain.KnowledgeQuery{Embedding: []float32{1}}, budget)
	if err != nil || query.Budget != 100*time.Millisecond {
		t.Fatalf("pre-embedded: %s, %v", query.Budget, err)
	}
	if _, err = ApplyBudget(domain.KnowledgeQuery{}, domain.TurnBudget{Rerank: time.Second}); !errors.Is(err, domain.ErrDeadlineInfeasible) {
		t.Fatalf("no retrieval slice = %v", err)
	}
}
