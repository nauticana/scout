package runtime

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PricedAgent decorates an executor with catalog pricing, so a task surface can
// quote work before running it and bill the exact usage afterwards.
type PricedAgent struct {
	contract.AgentExecutor
	Pricer contract.ModelPricer
}

var _ contract.PricedAgent = (*PricedAgent)(nil)

// ModelReference reports the provider/model the wrapped executor is bound to,
// for metric labels and provenance. Executors that carry no reference yield a
// zero value.
func (agent *PricedAgent) ModelReference() domain.ModelReference {
	type carrier interface{ ModelReference() domain.ModelReference }
	if agent == nil || agent.AgentExecutor == nil {
		return domain.ModelReference{}
	}
	if ref, ok := agent.AgentExecutor.(carrier); ok {
		return ref.ModelReference()
	}
	return domain.ModelReference{}
}

func (agent *PricedAgent) GenerateText(ctx context.Context, task domain.AgentTask) (string, int64, int64, error) {
	if agent == nil || agent.AgentExecutor == nil {
		return "", -1, -1, fmt.Errorf("%w: agent executor is required", domain.ErrValidation)
	}
	result, err := agent.Generate(ctx, task)
	if err != nil {
		return "", -1, -1, err
	}
	return string(result.Output), result.Usage.InputTokens, result.Usage.OutputTokens, nil
}

func (agent *PricedAgent) Cost(ctx context.Context, usage domain.ModelUsage) (int64, string, error) {
	if agent == nil || agent.AgentExecutor == nil || agent.Pricer == nil {
		return 0, "", fmt.Errorf("%w: agent executor and pricer are required", domain.ErrValidation)
	}
	return agent.Pricer.Cost(ctx, agent.ModelReference(), usage)
}
