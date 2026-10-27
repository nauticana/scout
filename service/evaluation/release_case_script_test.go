package evaluation

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func registryOf(toolIDs ...string) *fake.ToolRegistry {
	return &fake.ToolRegistry{
		ListFunc: func(context.Context, int64, string, string) ([]domain.ToolDefinition, error) {
			tools := make([]domain.ToolDefinition, 0, len(toolIDs))
			for _, toolID := range toolIDs {
				tools = append(tools, domain.ToolDefinition{ToolID: toolID, Version: "1", InputSchema: []byte(`{"type":"object"}`)})
			}
			return tools, nil
		},
		GetFunc: func(_ context.Context, _ int64, toolID, version string) (domain.ToolDefinition, error) {
			if slices.Contains(toolIDs, toolID) {
				return domain.ToolDefinition{ToolID: toolID, Version: version, InputSchema: []byte(`{"type":"object"}`)}, nil
			}
			return domain.ToolDefinition{}, domain.ErrNotFound
		},
	}
}

// scriptedReplay primes the replay harness with a two-turn script answered by ScriptSandbox.
func scriptedReplay(t *testing.T, harness *replayHarness) (domain.GoldenExample, *[]domain.StepInput) {
	t.Helper()
	script, err := normalizeScript(refundScript(), scriptBounds{})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := canonicalScript(script)
	example, reader := scriptedExample(t, content)
	harness.executor.Payloads = reader
	var steps []domain.StepInput
	harness.executor.Requests = requestBuilderFunc(func(_ context.Context, input domain.StepInput) (domain.ModelRequest, error) {
		steps = append(steps, input)
		return domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 7}, Prompt: input.Input, MaxOutputTokens: 1024}, nil
	})
	harness.executor.Sandboxes = func(ctx context.Context, sandboxed domain.GoldenExample, script domain.CaseScript) (contract.ToolSandbox, error) {
		if sandboxed.ExampleID != example.ExampleID || len(script.Tools) != 2 {
			t.Fatalf("sandbox built for %q from %+v", sandboxed.ExampleID, script)
		}
		return ScriptSandbox(ctx, sandboxed, script)
	}
	return example, &steps
}

func ordersCall() domain.ModelResult {
	return domain.ModelResult{ToolCalls: []domain.ModelToolCall{{CallID: nextCallID(), Name: "orders_get", Arguments: []byte(`{"id":1}`)}},
		FinishReason: domain.FinishReasonToolCalls, Usage: domain.Usage{InputTokens: 5}}
}

func TestReleaseCaseExecutorReplaysAScriptTurnByTurn(t *testing.T) {
	harness := newReplayHarness(t, ordersCall(), domain.ModelResult{Output: []byte("order 1 found"), Usage: domain.Usage{InputTokens: 7}},
		domain.ModelResult{Output: []byte("order 2 too"), Usage: domain.Usage{InputTokens: 11}})
	harness.executor.Registry = registryOf("orders.get")
	example, steps := scriptedReplay(t, harness)
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), example, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(evalCase.Output) != "order 2 too" || evalCase.Usage.InputTokens != 23 {
		t.Fatalf("case = %+v", evalCase)
	}
	if len(*steps) != 2 {
		t.Fatalf("steps = %d", len(*steps))
	}
	first, second := (*steps)[0], (*steps)[1]
	if string(first.Input) != replayInput || string(second.Input) != "and the second order?" {
		t.Fatalf("turn inputs = %q, %q", first.Input, second.Input)
	}
	conversation := "evaluation/v2/alt-text"
	if first.RequestID != conversation || second.RequestID != conversation+"/turn/2" ||
		first.Snapshot.ConversationID != conversation || second.Snapshot.ConversationID != conversation {
		t.Fatalf("request ids %q, %q", first.RequestID, second.RequestID)
	}
	if first.Snapshot.LatestTurnNo != 0 || first.Snapshot.State != nil || second.Snapshot.LatestTurnNo != 1 || string(second.Snapshot.State) != "order 1 found" {
		t.Fatalf("snapshots = %+v and %+v", first.Snapshot, second.Snapshot)
	}
	var boundary *domain.TrajectoryEvent
	completed := 0
	for i, event := range evalCase.Trajectory {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event %d has sequence %d", i, event.Sequence)
		}
		if event.Kind == domain.TrajectoryState && event.Name == ReplayTurn {
			boundary = &evalCase.Trajectory[i]
		}
		if event.Name == ReplayCompleted {
			completed++
		}
	}
	if boundary == nil || string(boundary.Payload) != `{"turn":2}` || completed != 2 || evalCase.Trajectory[len(evalCase.Trajectory)-1].Name != ReplayCompleted {
		t.Fatalf("trajectory = %+v", evalCase.Trajectory)
	}
}

func TestReleaseCaseExecutorEndsAScriptAtAFailedTurn(t *testing.T) {
	harness := newReplayHarness(t, ordersCall(), ordersCall(), ordersCall())
	harness.executor.Registry = registryOf("orders.get")
	harness.executor.Limits.MaxIterations = 2
	harness.executor.Limits.MaxRepeatedCalls = 10
	example, steps := scriptedReplay(t, harness)
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), example, nil)
	if err != nil {
		t.Fatalf("a turn the agent fails is an outcome: %v", err)
	}
	final := evalCase.Trajectory[len(evalCase.Trajectory)-1]
	if len(*steps) != 1 || final.Name != "execution_limit" || !final.Failed || evalCase.Output != nil {
		t.Fatalf("turn 2 must not run after turn 1 failed: steps %d, final %+v, output %q", len(*steps), final, evalCase.Output)
	}
	for _, event := range evalCase.Trajectory {
		if event.Name == ReplayTurn {
			t.Fatal("no boundary is recorded for a turn that never started")
		}
	}
}

func TestReleaseCaseExecutorDeliversAScriptedToolFailure(t *testing.T) {
	harness := newReplayHarness(t,
		domain.ModelResult{ToolCalls: []domain.ModelToolCall{{CallID: "c1", Name: "refund_issue", Arguments: []byte(`{}`)}}, FinishReason: domain.FinishReasonToolCalls},
		domain.ModelResult{Output: []byte("refund failed")}, domain.ModelResult{Output: []byte("ok")})
	harness.executor.Registry = registryOf("refund.issue")
	example, _ := scriptedReplay(t, harness)
	evalCase, err := harness.executor.Execute(context.Background(), replaySubject(), example, nil)
	if err != nil {
		t.Fatal(err)
	}
	var observed *domain.TrajectoryEvent
	for i := range evalCase.Trajectory {
		if evalCase.Trajectory[i].Kind == domain.TrajectoryObservation {
			observed = &evalCase.Trajectory[i]
		}
	}
	if observed == nil || !observed.Failed || string(observed.Payload) != `{"error":"contract_failed"}` {
		t.Fatalf("observation = %+v", observed)
	}
}

func TestReleaseCaseExecutorRefusesAScriptItCannotTrust(t *testing.T) {
	harness := newReplayHarness(t, domain.ModelResult{Output: []byte("x")})
	example, _ := scriptedReplay(t, harness)
	tampered := example
	tampered.Script.Digest = sha256Hex([]byte("other"))
	if _, err := harness.executor.Execute(context.Background(), replaySubject(), tampered, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tampered script = %v", err)
	}
	if _, err := harness.executor.Execute(context.Background(), replaySubject(), example, []domain.KnowledgeMatch{{DocumentID: "d"}}); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("preserved retrieval for several turns = %v", err)
	}
	if len(harness.requests) != 0 {
		t.Fatal("a refused replay called the model")
	}
}
