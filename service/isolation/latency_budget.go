package isolation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StageSlice bounds one stage's share of a turn deadline; Max zero disables an optional stage.
type StageSlice struct {
	Min, Max time.Duration
}

func (s StageSlice) valid() bool { return s.Min >= 0 && s.Max >= s.Min }

func (s StageSlice) clamp(want time.Duration) time.Duration {
	return min(max(want, s.Min), s.Max)
}

// LatencyBudgetConfig bounds each budgeted stage; prompt build and guardrail are required, the rest may be skipped.
type LatencyBudgetConfig struct {
	Embedding, Retrieval, Rerank, PromptBuild, Guardrail StageSlice
}

// LatencyBudgetAllocator turns the caller deadline (or the policy turn timeout) into per-stage slices:
// generation is reserved first from the model estimate, optional stages take what remains, and a
// deadline that cannot cover admission, prompt build, minimum generation, and guardrail is rejected.
type LatencyBudgetAllocator struct {
	// Model predicts stage costs; nil uses StaticStageLatencyModel defaults.
	Model  contract.StageLatencyModel
	Config LatencyBudgetConfig
	Now    func() time.Time
}

var _ contract.LatencyBudgetAllocator = (*LatencyBudgetAllocator)(nil)

// NewLatencyBudgetAllocator validates the stage bounds; model nil uses the static README table.
func NewLatencyBudgetAllocator(model contract.StageLatencyModel, config LatencyBudgetConfig) (*LatencyBudgetAllocator, error) {
	for name, slice := range map[string]StageSlice{
		"embedding": config.Embedding, "retrieval": config.Retrieval, "rerank": config.Rerank,
		"prompt build": config.PromptBuild, "guardrail": config.Guardrail,
	} {
		if !slice.valid() {
			return nil, fmt.Errorf("latency budget: %s slice needs 0 <= min <= max", name)
		}
	}
	if config.PromptBuild.Min <= 0 || config.Guardrail.Min <= 0 {
		return nil, fmt.Errorf("latency budget: prompt build and guardrail minimums must be positive")
	}
	if model == nil {
		model = &StaticStageLatencyModel{}
	}
	return &LatencyBudgetAllocator{Model: model, Config: config}, nil
}

// Allocate reserves generation first and returns ErrDeadlineInfeasible when the minimum path cannot fit.
func (allocator *LatencyBudgetAllocator) Allocate(ctx context.Context, request domain.TurnRequest, policy domain.TenantRuntimePolicy) (domain.TurnBudget, error) {
	if err := ctx.Err(); err != nil {
		return domain.TurnBudget{}, err
	}
	if allocator.Model == nil {
		return domain.TurnBudget{}, fmt.Errorf("latency budget: stage latency model is required")
	}
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" {
		return domain.TurnBudget{}, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	now := time.Now()
	if allocator.Now != nil {
		now = allocator.Now()
	}
	total := policy.TurnTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := deadline.Sub(now)
		if total <= 0 || remaining < total {
			total = remaining
		}
	}
	if total <= 0 {
		return domain.TurnBudget{}, fmt.Errorf("%w: no deadline remains for request %s", domain.ErrDeadlineInfeasible, request.RequestID)
	}
	estimate, err := allocator.Model.Estimate(ctx, request, policy)
	if err != nil {
		return domain.TurnBudget{}, fmt.Errorf("estimate stage latency: %w", err)
	}
	if estimate.MinGeneration <= 0 || estimate.Generation < estimate.MinGeneration || estimate.Admission < 0 ||
		estimate.Embedding < 0 || estimate.Retrieval < 0 || estimate.Rerank < 0 || estimate.PromptBuild < 0 || estimate.Guardrail < 0 {
		return domain.TurnBudget{}, fmt.Errorf("%w: stage latency estimate is invalid", domain.ErrValidation)
	}
	config := allocator.Config
	minimum := estimate.Admission + config.PromptBuild.Min + estimate.MinGeneration + config.Guardrail.Min
	if total < minimum {
		return domain.TurnBudget{}, fmt.Errorf("%w: %s remains, minimum path needs %s", domain.ErrDeadlineInfeasible, total, minimum)
	}

	remaining := total - estimate.Admission
	prompt := config.PromptBuild.clamp(estimate.PromptBuild)
	guardrail := config.Guardrail.clamp(estimate.Guardrail)
	generation := estimate.Generation
	available := remaining - prompt - guardrail - generation
	if available < 0 {
		// Squeeze the required stages to their floors; the minimum-path check guarantees this fits.
		prompt, guardrail = config.PromptBuild.Min, config.Guardrail.Min
		generation = remaining - prompt - guardrail
		available = 0
	}
	embedding := grant(config.Embedding, estimate.Embedding, &available)
	retrieval := grant(config.Retrieval, estimate.Retrieval, &available)
	var rerank time.Duration
	if retrieval > 0 {
		rerank = grant(config.Rerank, estimate.Rerank, &available)
	}
	generation += available
	return domain.TurnBudget{
		Deadline: now.Add(total), Total: total, Generation: generation,
		Embedding: embedding, Retrieval: retrieval, Rerank: rerank,
		PromptBuild: prompt, Guardrail: guardrail,
	}, nil
}

// grant gives an optional stage its clamped estimate, or whatever fits above its floor, or nothing.
func grant(slice StageSlice, want time.Duration, available *time.Duration) time.Duration {
	if slice.Max <= 0 {
		return 0
	}
	granted := slice.clamp(want)
	if granted > *available {
		granted = *available
	}
	if granted < slice.Min || granted <= 0 {
		return 0
	}
	*available -= granted
	return granted
}

// ApplyBudget bounds one retrieval call to its slices: retrieval plus rerank, plus embedding when the
// index must embed the query itself. A zero retrieval slice means the stage was not granted.
func ApplyBudget(query domain.KnowledgeQuery, budget domain.TurnBudget) (domain.KnowledgeQuery, error) {
	if budget.Retrieval <= 0 {
		return query, fmt.Errorf("%w: no retrieval slice remains for request %s", domain.ErrDeadlineInfeasible, query.RequestID)
	}
	query.Budget = budget.Retrieval + budget.Rerank
	if query.Embedding == nil {
		query.Budget += budget.Embedding
	}
	return query, nil
}

// StaticStageLatencyModel returns one fixed table; zero fields take the README starting p95 targets.
type StaticStageLatencyModel struct {
	Table domain.StageLatencyEstimate
}

var _ contract.StageLatencyModel = (*StaticStageLatencyModel)(nil)

// DefaultStageLatencyEstimate is the README "starting latency budget" split by stage.
var DefaultStageLatencyEstimate = domain.StageLatencyEstimate{
	Admission:     40 * time.Millisecond,
	Embedding:     20 * time.Millisecond,
	Retrieval:     80 * time.Millisecond,
	Rerank:        20 * time.Millisecond,
	PromptBuild:   30 * time.Millisecond,
	Guardrail:     60 * time.Millisecond,
	MinGeneration: 600 * time.Millisecond,
	Generation:    900 * time.Millisecond,
}

// Estimate returns the configured table regardless of request shape.
func (model *StaticStageLatencyModel) Estimate(context.Context, domain.TurnRequest, domain.TenantRuntimePolicy) (domain.StageLatencyEstimate, error) {
	estimate := model.Table
	fill := func(value *time.Duration, fallback time.Duration) {
		if *value == 0 {
			*value = fallback
		}
	}
	defaults := DefaultStageLatencyEstimate
	fill(&estimate.Admission, defaults.Admission)
	fill(&estimate.Embedding, defaults.Embedding)
	fill(&estimate.Retrieval, defaults.Retrieval)
	fill(&estimate.Rerank, defaults.Rerank)
	fill(&estimate.PromptBuild, defaults.PromptBuild)
	fill(&estimate.Guardrail, defaults.Guardrail)
	fill(&estimate.MinGeneration, defaults.MinGeneration)
	fill(&estimate.Generation, defaults.Generation)
	return estimate, nil
}
