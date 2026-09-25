package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type definitionReaderStub struct {
	definition domain.AgentDefinition
	asked      string
}

func (stub *definitionReaderStub) Get(_ context.Context, _ int64, agentID, version string) (domain.AgentDefinition, error) {
	stub.asked = agentID + "@" + version
	return stub.definition, nil
}

type taskRenderer struct{}

func (taskRenderer) Render(agentID string, sections []domain.CompiledPromptSection, task domain.AgentTask) string {
	return agentID + "|" + sections[0].Instruction + "|" + task.Task + "|" + task.Context
}

func TestReleaseLoopRequestBuilderRendersThePinnedReleaseAndTheTurnInput(t *testing.T) {
	reader := &definitionReaderStub{definition: domain.AgentDefinition{
		AgentID: "writer", Version: "v3",
		Models:    domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "anthropic", ModelID: "sonnet"}},
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{{Instruction: "be brief"}}}},
	}}
	builder := &ReleaseLoopRequestBuilder{Definitions: reader, Renderer: taskRenderer{}, MaxOutputTokens: 500}
	input := loopInput()
	input.Input = []byte("audit the homepage")
	input.Snapshot.ConversationID = "conversation-9"
	request, err := builder.Build(context.Background(), input, domain.ToolLoopConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if reader.asked != "writer@v3" || string(request.Prompt) != "writer|be brief|audit the homepage|" {
		t.Fatalf("asked %s, prompt %q", reader.asked, request.Prompt)
	}
	if request.Model.ModelID != "sonnet" || request.TenantContext.TenantID != 7 || request.ConversationID != "conversation-9" || request.MaxOutputTokens != 500 {
		t.Fatalf("request = %+v", request)
	}
	builder.Task = func(_ context.Context, input domain.StepInput) (domain.AgentTask, string, error) {
		return domain.AgentTask{Task: string(input.Input), Context: "brand: acme"}, "en-US", nil
	}
	if request, err = builder.Build(context.Background(), input, domain.ToolLoopConfig{}); err != nil || !strings.HasSuffix(string(request.Prompt), "|brand: acme") {
		t.Fatalf("product context hook: %q, %v", request.Prompt, err)
	}
	builder.Task, builder.LanguageCode = nil, "tr-TR"
	if _, err = builder.Build(context.Background(), input, domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrNoPrompts) {
		t.Fatalf("want ErrNoPrompts for an uncompiled language, got %v", err)
	}
	input.Input = nil
	builder.LanguageCode = ""
	if _, err = builder.Build(context.Background(), input, domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation without turn input, got %v", err)
	}
}

type skillListStub struct {
	contract.SkillRegistry
	skills []domain.SkillDefinition
}

func (stub skillListStub) List(context.Context, int64, string, string) ([]domain.SkillDefinition, error) {
	return stub.skills, nil
}

func TestReleaseLoopRequestBuilderIndexesTheReleasesSkills(t *testing.T) {
	reader := &definitionReaderStub{definition: domain.AgentDefinition{
		AgentID: "writer", Version: "v3", Skills: []domain.SkillReference{{SkillID: "audit", Version: "1"}},
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{{Instruction: "be brief"}}}},
	}}
	builder := &ReleaseLoopRequestBuilder{Definitions: reader, Renderer: taskRenderer{}, MaxOutputTokens: 500}
	input := loopInput()
	input.Input = []byte("audit the homepage")
	if _, err := builder.Build(context.Background(), input, domain.ToolLoopConfig{}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a release with skills and no registry: want ErrNotReady, got %v", err)
	}
	builder.Skills = skillListStub{skills: []domain.SkillDefinition{{SkillID: "audit", Summary: "A page needs an audit."}}}
	request, err := builder.Build(context.Background(), input, domain.ToolLoopConfig{})
	if err != nil || !strings.HasSuffix(string(request.Prompt), "use_skill before starting):\n- audit: A page needs an audit.") {
		t.Fatalf("prompt %q, %v", request.Prompt, err)
	}
}

func TestLoopBudgetEstimatorQuotesTheLoopCeilings(t *testing.T) {
	limits := domain.ToolLoopLimits{MaxTokens: 9000, MaxToolCalls: 5, MaxCostMinorUnits: 40, Deadline: time.Minute}
	quote, err := LoopBudgetEstimator{Limits: limits, Currency: "USD"}.Estimate(context.Background(), domain.TurnRequest{})
	if err != nil || quote.InputTokens != 9000 || quote.CostMinorUnits != 40 || quote.Currency != "USD" {
		t.Fatalf("quote = %+v, %v", quote, err)
	}
	if _, err := (LoopBudgetEstimator{Limits: limits}).Estimate(context.Background(), domain.TurnRequest{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation for a cost ceiling without a currency, got %v", err)
	}
}

func TestPinnedPrincipalFollowsTheConversationsAgentVersion(t *testing.T) {
	turn := domain.TurnRequest{AgentID: "writer", Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "writer", Release: "v9-deployed-later"}}
	if got := pinnedPrincipal(turn, "v3").Release; got != "v3" {
		t.Fatalf("tools must come from the release the graph came from, got %q", got)
	}
	queued := domain.TurnRequest{AgentID: "writer", TenantContext: domain.TenantContext{TenantID: 7}}
	if got := pinnedPrincipal(queued, "v3"); got.Kind != domain.PrincipalAgent || got.ID != "writer" || got.TenantID != 7 || got.Release != "v3" {
		t.Fatalf("a queue-delivered turn must act as its agent at the pinned version, got %+v", got)
	}
	turn.Principal.ID = "delegate"
	if got := pinnedPrincipal(turn, "v3").Release; got != "v9-deployed-later" {
		t.Fatalf("another agent's release is not this conversation's to pin, got %q", got)
	}
}
