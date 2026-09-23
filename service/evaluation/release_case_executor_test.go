package evaluation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type objectReaderFunc func(context.Context, domain.ObjectRef) ([]byte, error)

func (function objectReaderFunc) Read(ctx context.Context, ref domain.ObjectRef) ([]byte, error) {
	return function(ctx, ref)
}

type requestBuilderFunc func(context.Context, domain.StepInput) (domain.ModelRequest, error)

func (function requestBuilderFunc) Build(ctx context.Context, input domain.StepInput, _ domain.ToolLoopConfig) (domain.ModelRequest, error) {
	return function(ctx, input)
}

type guardrailFunc struct {
	contract.GuardrailEnforcer
	after func(domain.ModelChunk) (domain.ModelChunk, error)
}

func (guardrail guardrailFunc) AfterModelChunk(_ context.Context, _ domain.GuardrailConfig, _ domain.GuardrailSubject, chunk domain.ModelChunk) (domain.ModelChunk, error) {
	return guardrail.after(chunk)
}

type guardrailConfigs struct {
	contract.GuardrailConfigRepository
}

func (guardrailConfigs) Get(context.Context, int64, string, string) (domain.GuardrailConfig, error) {
	return domain.GuardrailConfig{}, nil
}

const replayInput = "which products lack alt text?"

type replayHarness struct {
	executor *ReleaseCaseExecutor
	sandbox  *RecordingSandbox
	script   []domain.ModelResult
	requests []domain.ModelRequest
}

func newReplayHarness(t *testing.T, script ...domain.ModelResult) *replayHarness {
	harness := &replayHarness{script: script, sandbox: &RecordingSandbox{Strict: true, Responses: map[string]domain.ToolResult{
		"catalog.search": {Output: []byte(`{"products":["p1"]}`), Usage: domain.Usage{ToolCalls: 1}},
	}}}
	harness.executor = &ReleaseCaseExecutor{
		Graphs: fake.DefinitionResolverFunc(func(_ context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error) {
			if tenantID != 7 || agentID != "wingmate" {
				t.Fatalf("graph resolved for %d %s@%s", tenantID, agentID, version)
			}
			return domain.ExecutionGraph{AgentID: agentID, Version: version, EntryStepID: "answer",
				Steps: []domain.ExecutionStep{{ExecutionStepID: 3, StepID: "answer", Kind: domain.StepKindToolLoop}}}, nil
		}),
		Requests: requestBuilderFunc(func(_ context.Context, input domain.StepInput) (domain.ModelRequest, error) {
			if input.Principal.Release != "v2" || string(input.Input) != replayInput {
				t.Fatalf("step input = %+v", input)
			}
			return domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 7}, Prompt: input.Input, MaxOutputTokens: 1024}, nil
		}),
		Router: fake.ModelRouterFunc(func(context.Context, domain.ModelRequest) (domain.ModelSelection, error) {
			return domain.ModelSelection{Provider: "anthropic", Model: "claude", ModelVersion: "claude-2026"}, nil
		}),
		Models: &fake.ModelGateway{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
			harness.requests = append(harness.requests, request)
			if len(harness.requests) > len(harness.script) {
				t.Fatalf("model asked %d times", len(harness.requests))
			}
			return harness.script[len(harness.requests)-1], nil
		}},
		Registry: &fake.ToolRegistry{ListFunc: func(context.Context, int64, string, string) ([]domain.ToolDefinition, error) {
			return []domain.ToolDefinition{{ToolID: "catalog.search", Version: "1", InputSchema: []byte(`{"type":"object"}`)}}, nil
		}},
		Payloads: objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return []byte(replayInput), nil }),
		Sandboxes: func(context.Context, domain.GoldenExample) (contract.ToolSandbox, error) {
			return harness.sandbox, nil
		},
		Limits: domain.ToolLoopLimits{MaxIterations: 3, MaxToolCalls: 4, MaxTokens: 10_000, MaxRepeatedCalls: 3, Deadline: time.Minute},
	}
	return harness
}

func replaySubject() domain.EvaluationSubject {
	return domain.EvaluationSubject{AgentID: "wingmate", AgentVersion: "v2", Decoding: []byte(`{"max_tokens":256,"temperature":0}`)}
}

func replayExample() domain.GoldenExample {
	return domain.GoldenExample{TenantID: 7, ExampleID: "alt-text", Payload: domain.ObjectRef{URI: "object://golden/alt-text", Digest: sha256Hex([]byte(replayInput))}}
}

func searchCall() domain.ModelResult {
	return domain.ModelResult{
		ToolCalls:    []domain.ModelToolCall{{CallID: "c1", Name: "catalog_search", Arguments: []byte(`{"missing":"alt"}`)}},
		FinishReason: domain.FinishReasonToolCalls, Usage: domain.Usage{InputTokens: 10, OutputTokens: 4},
	}
}

func TestReleaseCaseExecutorReplaysTheReleaseThroughTheSandbox(t *testing.T) {
	harness := newReplayHarness(t, searchCall(), domain.ModelResult{Output: []byte("p1 lacks alt text"), Usage: domain.Usage{InputTokens: 20, OutputTokens: 6}})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(evalCase.Output) != "p1 lacks alt text" || evalCase.Usage.InputTokens != 30 || evalCase.Usage.ToolCalls != 1 {
		t.Fatalf("case = %+v", evalCase)
	}
	if request := harness.requests[0]; request.MaxOutputTokens != 256 || request.Temperature == nil || *request.Temperature != 0 {
		t.Fatalf("decoding was not applied: max %d temperature %v", request.MaxOutputTokens, request.Temperature)
	}
	if recorded := harness.sandbox.Recorded(); len(recorded) != 1 || recorded[0].ToolID != "catalog.search" {
		t.Fatalf("sandbox calls = %+v", recorded)
	}
	kinds := []domain.TrajectoryEventKind{domain.TrajectoryToolCall, domain.TrajectoryObservation, domain.TrajectoryState, domain.TrajectoryState}
	if len(evalCase.Trajectory) != len(kinds) {
		t.Fatalf("trajectory = %+v", evalCase.Trajectory)
	}
	for i, event := range evalCase.Trajectory {
		if event.Kind != kinds[i] || event.Sequence != int64(i+1) {
			t.Fatalf("event %d = %+v", i, event)
		}
	}
	if call := evalCase.Trajectory[0]; call.Name != "catalog.search" || string(call.Payload) != `{"missing":"alt"}` {
		t.Fatalf("tool call = %+v", call)
	}
	if answer := evalCase.Trajectory[2]; answer.Name != "answer" || string(answer.Payload) != "p1 lacks alt text" {
		t.Fatalf("answer state = %+v", answer)
	}
	if final := evalCase.Trajectory[3]; final.Name != ReplayCompleted {
		t.Fatalf("final state = %+v", final)
	}
}

func TestReleaseCaseExecutorScoresAnAgentFailureInsteadOfFailingTheRun(t *testing.T) {
	harness := newReplayHarness(t, searchCall(), searchCall(), searchCall())
	harness.executor.Limits.MaxRepeatedCalls = 10
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatalf("an agent that exhausts its iterations is an outcome: %v", err)
	}
	final := evalCase.Trajectory[len(evalCase.Trajectory)-1]
	if final.Kind != domain.TrajectoryState || final.Name != "execution_limit" || !final.Failed {
		t.Fatalf("final state = %+v", final)
	}
	if calls := len(harness.sandbox.Recorded()); calls != 3 || len(evalCase.Trajectory) != 7 || evalCase.Usage.InputTokens != 30 {
		t.Fatalf("the calls made before the failure are kept: calls %d trajectory %d usage %+v", calls, len(evalCase.Trajectory), evalCase.Usage)
	}
}

func TestReleaseCaseExecutorRecordsABlockedOutput(t *testing.T) {
	harness := newReplayHarness(t, domain.ModelResult{Output: []byte("unsafe")})
	harness.executor.Guardrails = guardrailFunc{after: func(domain.ModelChunk) (domain.ModelChunk, error) {
		return domain.ModelChunk{}, domain.ErrForbidden
	}}
	harness.executor.GuardrailConfigs = guardrailConfigs{}
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil || evalCase.Output != nil || evalCase.Trajectory[len(evalCase.Trajectory)-1].Name != ReplayBlocked {
		t.Fatalf("blocked = %+v, %v", evalCase, err)
	}
}

func TestReleaseCaseExecutorRefusesAReplayItCannotMakeFaithful(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*replayHarness, *domain.EvaluationSubject, *domain.GoldenExample, *[]domain.KnowledgeMatch)
		want   error
	}{
		{"tampered payload", func(_ *replayHarness, _ *domain.EvaluationSubject, example *domain.GoldenExample, _ *[]domain.KnowledgeMatch) {
			example.Payload.Digest = sha256Hex([]byte("other"))
		}, domain.ErrValidation},
		{"unappliable decoding", func(_ *replayHarness, subject *domain.EvaluationSubject, _ *domain.GoldenExample, _ *[]domain.KnowledgeMatch) {
			subject.Decoding = []byte(`{"top_p":0.9}`)
		}, domain.ErrCapabilityUnsupported},
		{"trailing decoding", func(_ *replayHarness, subject *domain.EvaluationSubject, _ *domain.GoldenExample, _ *[]domain.KnowledgeMatch) {
			subject.Decoding = []byte(`{"max_tokens":32} {}`)
		}, domain.ErrCapabilityUnsupported},
		{"another model version", func(_ *replayHarness, subject *domain.EvaluationSubject, _ *domain.GoldenExample, _ *[]domain.KnowledgeMatch) {
			subject.Versions.Model = "claude-2025"
		}, domain.ErrConflict},
		{"another agent version", func(_ *replayHarness, subject *domain.EvaluationSubject, _ *domain.GoldenExample, _ *[]domain.KnowledgeMatch) {
			subject.Versions.Agent = "v1"
		}, domain.ErrConflict},
		{"preserved retrieval without a retriever", func(_ *replayHarness, _ *domain.EvaluationSubject, _ *domain.GoldenExample, preserved *[]domain.KnowledgeMatch) {
			*preserved = []domain.KnowledgeMatch{{DocumentID: "d"}}
		}, domain.ErrCapabilityUnsupported},
		{"a step kind the replay does not run", func(harness *replayHarness, _ *domain.EvaluationSubject, _ *domain.GoldenExample, _ *[]domain.KnowledgeMatch) {
			harness.executor.Graphs = fake.DefinitionResolverFunc(func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
				return domain.ExecutionGraph{EntryStepID: "a", Steps: []domain.ExecutionStep{{ExecutionStepID: 1, StepID: "a", Kind: "delegate"}}}, nil
			})
		}, domain.ErrCapabilityUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newReplayHarness(t, searchCall(), domain.ModelResult{Output: []byte("done")})
			subject, example := replaySubject(), replayExample()
			var preserved []domain.KnowledgeMatch
			tc.mutate(harness, &subject, &example, &preserved)
			if _, err := harness.executor.Execute(context.Background(), subject, example, preserved); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReleaseCaseExecutorScoresGraphStepExhaustion(t *testing.T) {
	harness := newReplayHarness(t, domain.ModelResult{Output: []byte("again")})
	harness.executor.Graphs = fake.DefinitionResolverFunc(func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
		return domain.ExecutionGraph{AgentID: "wingmate", Version: "v2", EntryStepID: "answer", Steps: []domain.ExecutionStep{
			{ExecutionStepID: 3, StepID: "answer", Kind: domain.StepKindToolLoop, Configuration: []byte(`{"next_step_id":"answer"}`)},
		}}, nil
	})
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil {
		t.Fatalf("graph exhaustion is a scored outcome: %v", err)
	}
	final := evalCase.Trajectory[len(evalCase.Trajectory)-1]
	if final.Name != "execution_limit" || !final.Failed {
		t.Fatalf("final state = %+v", final)
	}
}
