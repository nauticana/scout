package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/controlplane"
	"github.com/nauticana/scout/service/dataplane"
	agentruntime "github.com/nauticana/scout/service/runtime"
	"github.com/nauticana/scout/service/scope"
)

type draftReplayHarness struct {
	executor *DraftCaseExecutor
	draft    domain.AgentDefinition
	settings []domain.EffectiveResource
	example  domain.GoldenExample
	requests []domain.ModelRequest
	gets     []string
}

func newDraftReplayHarness(t *testing.T, script ...domain.ModelResult) *draftReplayHarness {
	t.Helper()
	harness := &draftReplayHarness{settings: draftSettings()}
	harness.draft = domain.AgentDefinition{
		AgentID: "writer-a", DefinitionDigest: draftDigest, SettingsDigest: scope.SettingsDigest(harness.settings),
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{}}},
		Tools:     []domain.ToolReference{{ToolID: "orders.get", Version: "3"}},
		ToolLoop:  &domain.ToolLoopConfig{},
	}
	normalized, err := normalizeScript(refundScript(), scriptBounds{})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := canonicalScript(normalized)
	var reader objectReaderFunc
	harness.example, reader = scriptedExample(t, content)
	registry := registryOf("orders.get")
	get := registry.GetFunc
	registry.GetFunc = func(ctx context.Context, tenantID int64, toolID, version string) (domain.ToolDefinition, error) {
		harness.gets = append(harness.gets, toolID+"@"+version)
		return get(ctx, tenantID, toolID, version)
	}
	registry.ListFunc = func(context.Context, int64, string, string) ([]domain.ToolDefinition, error) {
		t.Fatal("a draft replay must not list a release's tool bindings")
		return nil, nil
	}
	harness.executor = &DraftCaseExecutor{
		Compiler: controlplane.ToolLoopGraphCompiler{},
		Requests: func(definitions contract.AgentDefinitionReader, skills contract.SkillRegistry) contract.ToolLoopRequestBuilder {
			return &dataplane.ReleaseLoopRequestBuilder{Definitions: definitions, Skills: skills, Renderer: &agentruntime.PromptRenderer{}, MaxOutputTokens: 512}
		},
		Router: fake.ModelRouterFunc(func(context.Context, domain.ModelRequest) (domain.ModelSelection, error) {
			return domain.ModelSelection{Provider: "anthropic", Model: "claude"}, nil
		}),
		Models: &fake.ModelGateway{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
			harness.requests = append(harness.requests, request)
			if len(harness.requests) > len(script) {
				t.Fatalf("model asked %d times", len(harness.requests))
			}
			return script[len(harness.requests)-1], nil
		}},
		Registry: registry, Payloads: reader, Sandboxes: ScriptSandbox,
		Guardrails: &fake.GuardrailEnforcer{},
		Limits:     domain.ToolLoopLimits{MaxIterations: 3, MaxToolCalls: 4, MaxTokens: 10_000, MaxRepeatedCalls: 3, Deadline: time.Minute},
	}
	return harness
}

func TestDraftCaseExecutorAppliesFrozenOutputGuardrails(t *testing.T) {
	harness := newDraftReplayHarness(t, domain.ModelResult{Output: []byte("secret")}, domain.ModelResult{Output: []byte("secret")})
	harness.executor.Guardrails = &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, config domain.GuardrailConfig, _ domain.GuardrailSubject, chunk domain.ModelChunk) (domain.ModelChunk, error) {
		if config.RulesDigest == "" {
			t.Fatal("draft guardrail config was not resolved")
		}
		chunk.Payload = []byte("redacted")
		return chunk, nil
	}}
	evalCase, err := harness.executor.ExecuteDraft(context.Background(), 7, harness.draft, harness.settings, harness.example)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range evalCase.Trajectory {
		if strings.Contains(string(event.Payload), "secret") {
			t.Fatalf("raw answer remained in trajectory: %+v", evalCase.Trajectory)
		}
	}
	if string(evalCase.Output) != "redacted" {
		t.Fatalf("guarded draft case = %+v", evalCase)
	}
}

func TestDraftCaseExecutorReplaysAScriptOnTheFrozenDefinition(t *testing.T) {
	harness := newDraftReplayHarness(t, ordersCall(), domain.ModelResult{Output: []byte("order 1 found")}, domain.ModelResult{Output: []byte("order 2 too")})
	evalCase, err := harness.executor.ExecuteDraft(context.Background(), 7, harness.draft, harness.settings, harness.example)
	if err != nil {
		t.Fatal(err)
	}
	if string(evalCase.Output) != "order 2 too" || len(harness.requests) != 3 {
		t.Fatalf("case = %+v after %d model calls", evalCase, len(harness.requests))
	}
	if len(harness.gets) == 0 || harness.gets[0] != "orders.get@3" {
		t.Fatalf("tool versions read = %v, want the draft's bound version", harness.gets)
	}
	if tools := harness.requests[0].Tools; len(tools) != 1 || tools[0].ToolID != "orders.get" || tools[0].ToolVersion != "3" {
		t.Fatalf("model tools = %+v", tools)
	}
	if !strings.Contains(string(harness.requests[2].Prompt), "and the second order?") {
		t.Fatalf("turn 2 prompt = %s", harness.requests[2].Prompt)
	}
	var observed bool
	for _, event := range evalCase.Trajectory {
		observed = observed || event.Kind == domain.TrajectoryObservation && string(event.Payload) == `{"id":1}`
	}
	if !observed {
		t.Fatalf("the scripted tool output is not on the trajectory: %+v", evalCase.Trajectory)
	}
}

func TestDraftCaseExecutorRefusesWhatItCannotReplayFaithfully(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*draftReplayHarness, *int64)
		want   error
	}{
		"no tool loop":     {func(h *draftReplayHarness, _ *int64) { h.draft.ToolLoop = nil }, domain.ErrCapabilityUnsupported},
		"settings drift":   {func(h *draftReplayHarness, _ *int64) { h.settings = nil }, domain.ErrConflict},
		"another tenant":   {func(_ *draftReplayHarness, tenant *int64) { *tenant = 8 }, domain.ErrForbidden},
		"tampered script":  {func(h *draftReplayHarness, _ *int64) { h.example.Script.Digest = sha256Hex([]byte("x")) }, domain.ErrValidation},
		"undigested draft": {func(h *draftReplayHarness, _ *int64) { h.draft.DefinitionDigest = "" }, domain.ErrValidation},
		"unbound tool":     {func(h *draftReplayHarness, _ *int64) { h.draft.Tools[0].ToolID = "ghost" }, domain.ErrNotFound},
	} {
		harness := newDraftReplayHarness(t, domain.ModelResult{Output: []byte("x")})
		tenant := int64(7)
		test.mutate(harness, &tenant)
		_, err := harness.executor.ExecuteDraft(context.Background(), tenant, harness.draft, harness.settings, harness.example)
		if !errors.Is(err, test.want) {
			t.Fatalf("%s = %v, want %v", name, err, test.want)
		}
		if len(harness.requests) != 0 {
			t.Fatalf("%s: a refused replay called the model", name)
		}
	}
}

func TestDraftCaseExecutorAppliesTheFrozenPerTurnBudget(t *testing.T) {
	harness := newDraftReplayHarness(t, domain.ModelResult{Output: []byte("x"), Usage: domain.Usage{InputTokens: 50}})
	harness.settings = append(harness.settings, domain.EffectiveResource{ResourceKind: domain.ResourceBudget, ResourceID: "budget",
		Value: []byte(`{"tokens":10,"cost_minor_units":0,"period":"` + string(domain.BudgetPerTurn) + `"}`)})
	harness.draft.SettingsDigest = scope.SettingsDigest(harness.settings)
	evalCase, err := harness.executor.ExecuteDraft(context.Background(), 7, harness.draft, harness.settings, harness.example)
	if err != nil {
		t.Fatalf("a turn over its frozen budget is an outcome: %v", err)
	}
	if final := evalCase.Trajectory[len(evalCase.Trajectory)-1]; !final.Failed || evalCase.Output != nil || len(harness.requests) != 1 {
		t.Fatalf("final = %+v after %d model calls", final, len(harness.requests))
	}
	harness.settings[len(harness.settings)-1].Value = []byte(`{`)
	harness.draft.SettingsDigest = scope.SettingsDigest(harness.settings)
	if _, err := harness.executor.ExecuteDraft(context.Background(), 7, harness.draft, harness.settings, harness.example); err == nil {
		t.Fatal("a malformed frozen budget must fail the case")
	}
}
