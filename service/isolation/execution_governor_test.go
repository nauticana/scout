package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func validTurnRequest() domain.TurnRequest {
	return domain.TurnRequest{
		TenantContext:  domain.TenantContext{TenantID: 7},
		RequestID:      "request",
		ConversationID: "conversation",
		AgentID:        "agent",
	}
}

func validRuntimePolicy() domain.TenantRuntimePolicy {
	return domain.TenantRuntimePolicy{
		MaxSteps:          2,
		MaxTokens:         100,
		MaxCostMinorUnits: 50,
		CostCurrency:      "USD",
		TurnTimeout:       time.Minute,
	}
}

func executionGovernor(now *time.Time, observed *[]string, recorded *[]domain.Usage) *ExecutionGovernor {
	return &ExecutionGovernor{
		Loops: &fake.LoopDetector{
			ObserveFunc: func(_ context.Context, _ int64, _ string, fingerprint string) error {
				*observed = append(*observed, fingerprint)
				return nil
			},
			ResetFunc: func(context.Context, int64, string) error { return nil },
		},
		Costs: &fake.CostCircuitBreaker{
			AllowFunc: func(context.Context, int64, string, int64) error { return nil },
			RecordFunc: func(_ context.Context, _ int64, _ string, usage domain.Usage) error {
				*recorded = append(*recorded, usage)
				return nil
			},
		},
		Now: func() time.Time { return *now },
	}
}

func TestExecutionGovernorAccountsStepsAndTerminalDelta(t *testing.T) {
	now := time.Now()
	var observed []string
	var recorded []domain.Usage
	governor := executionGovernor(&now, &observed, &recorded)
	permit, err := governor.Start(context.Background(), validTurnRequest(), validRuntimePolicy())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := permit.BeforeStep(context.Background(), domain.ExecutionStep{StepID: "one", Kind: "model"}); err != nil {
		t.Fatalf("BeforeStep: %v", err)
	}
	stepUsage := domain.Usage{InputTokens: 2, CostMinorUnits: 3, Currency: "USD"}
	if err := permit.AfterStep(context.Background(), domain.StepResult{Fingerprint: "fingerprint", Usage: stepUsage}); err != nil {
		t.Fatalf("AfterStep: %v", err)
	}
	terminal := domain.Usage{InputTokens: 2, OutputTokens: 4, CostMinorUnits: 5, Currency: "USD"}
	if err := permit.Close(context.Background(), terminal); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(observed) != 1 || observed[0] != "fingerprint" || len(recorded) != 2 {
		t.Fatalf("observed = %v, recorded = %+v", observed, recorded)
	}
	if recorded[1].OutputTokens != 4 || recorded[1].CostMinorUnits != 2 {
		t.Fatalf("terminal delta = %+v", recorded[1])
	}
}

func TestExecutionGovernorEnforcesStepLimit(t *testing.T) {
	now := time.Now()
	var observed []string
	var recorded []domain.Usage
	policy := validRuntimePolicy()
	policy.MaxSteps = 1
	permit, _ := executionGovernor(&now, &observed, &recorded).Start(context.Background(), validTurnRequest(), policy)
	if err := permit.BeforeStep(context.Background(), domain.ExecutionStep{StepID: "one", Kind: "model"}); err != nil {
		t.Fatalf("BeforeStep: %v", err)
	}
	if err := permit.AfterStep(context.Background(), domain.StepResult{}); err != nil {
		t.Fatalf("AfterStep: %v", err)
	}
	if err := permit.BeforeStep(context.Background(), domain.ExecutionStep{StepID: "two", Kind: "model"}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v", err)
	}
}

func TestExecutionGovernorAccountsBeforeReturningLimitErrors(t *testing.T) {
	now := time.Now()
	var observed []string
	var recorded []domain.Usage
	policy := validRuntimePolicy()
	policy.MaxTokens = 3
	permit, _ := executionGovernor(&now, &observed, &recorded).Start(context.Background(), validTurnRequest(), policy)
	_ = permit.BeforeStep(context.Background(), domain.ExecutionStep{StepID: "one", Kind: "model"})
	usage := domain.Usage{InputTokens: 2, OutputTokens: 2}
	err := permit.AfterStep(context.Background(), domain.StepResult{Usage: usage})
	if !errors.Is(err, domain.ErrExecutionLimit) || len(recorded) != 1 {
		t.Fatalf("error = %v, recorded = %+v", err, recorded)
	}
}

func TestExecutionGovernorPropagatesLoopDetection(t *testing.T) {
	now := time.Now()
	want := domain.ErrLoopDetected
	governor := &ExecutionGovernor{
		Loops: &fake.LoopDetector{
			ObserveFunc: func(context.Context, int64, string, string) error { return want },
			ResetFunc:   func(context.Context, int64, string) error { return nil },
		},
		Costs: &fake.CostCircuitBreaker{
			AllowFunc:  func(context.Context, int64, string, int64) error { return nil },
			RecordFunc: func(context.Context, int64, string, domain.Usage) error { return nil },
		},
		Now: func() time.Time { return now },
	}
	permit, _ := governor.Start(context.Background(), validTurnRequest(), validRuntimePolicy())
	_ = permit.BeforeStep(context.Background(), domain.ExecutionStep{StepID: "one", Kind: "model"})
	if err := permit.AfterStep(context.Background(), domain.StepResult{Fingerprint: "same"}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func TestExecutionGovernorEnforcesTimeout(t *testing.T) {
	now := time.Now()
	var observed []string
	var recorded []domain.Usage
	policy := validRuntimePolicy()
	policy.TurnTimeout = time.Second
	permit, _ := executionGovernor(&now, &observed, &recorded).Start(context.Background(), validTurnRequest(), policy)
	now = now.Add(time.Second)
	if err := permit.BeforeStep(context.Background(), domain.ExecutionStep{StepID: "one", Kind: "model"}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v", err)
	}
}
