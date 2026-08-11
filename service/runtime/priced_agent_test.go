package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

type pricerFake struct {
	cost int64
	err  error
	ref  domain.ModelReference
}

func (p *pricerFake) Cost(_ context.Context, reference domain.ModelReference, usage domain.ModelUsage) (int64, string, error) {
	p.ref = reference
	return p.cost, "CRD", p.err
}

type executorFake struct {
	output string
	in     int64
	out    int64
	ref    domain.ModelReference
}

func (e *executorFake) Generate(context.Context, domain.AgentTask) (domain.ModelResult, error) {
	return domain.ModelResult{Output: []byte(e.output),
		Usage: domain.Usage{InputTokens: e.in, OutputTokens: e.out}}, nil
}

func (e *executorFake) GenerateImage(context.Context, string, domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	return nil, nil
}

func (e *executorFake) GenerateVideo(context.Context, string, domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	return nil, nil
}

func (e *executorFake) AgentID() string                                { return "writer" }
func (e *executorFake) ModelReference() domain.ModelReference          { return e.ref }
func (e *executorFake) PromptSections() []domain.CompiledPromptSection { return nil }

func TestPricedAgentPricesItsOwnModel(t *testing.T) {
	reference := domain.ModelReference{ProviderID: "anthropic", ModelID: "claude-opus-5"}
	pricer := &pricerFake{cost: 220000}
	agent := &PricedAgent{AgentExecutor: &executorFake{output: "post", in: 1000, out: 2000, ref: reference}, Pricer: pricer}

	text, in, out, err := agent.GenerateText(context.Background(), domain.AgentTask{Task: "write"})
	if err != nil || text != "post" || in != 1000 || out != 2000 {
		t.Fatalf("GenerateText = %q %d %d err=%v", text, in, out, err)
	}
	cost, err := agent.Cost(context.Background(), domain.ModelUsage{InputTokens: in, OutputTokens: out})
	if err != nil || cost != 220000 {
		t.Fatalf("Cost = %d err = %v", cost, err)
	}
	if pricer.ref != reference {
		t.Fatalf("priced %+v, want the agent's own model", pricer.ref)
	}
}

func TestPricedAgentRequiresPricer(t *testing.T) {
	agent := &PricedAgent{AgentExecutor: &executorFake{}}
	if _, err := agent.Cost(context.Background(), domain.ModelUsage{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing pricer error = %v", err)
	}
}
