package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// retryEverything retries any failed attempt, as a product's policy may.
type retryEverything struct{}

func (retryEverything) NextDelay(context.Context, domain.ToolCall, domain.ToolResult, error, int) (time.Duration, bool) {
	return 0, true
}

func readOnlyRegistry(calls *[]string) *fake.ToolRegistry {
	return &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		*calls = append(*calls, "registry")
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", ReadOnly: true}, nil
	}}
}

// The gateway says what became of a failed call: refused before it left is not sent; a sent
// mutating call whose failure proves no effect is no effect; any other failure of a sent mutating
// call is unknown, never a plain error; a read-only call's failure is left as it is.
func TestGovernedGatewayClassifiesWhatBecameOfAFailedCall(t *testing.T) {
	transportErr := func(err error, retryable bool) fake.ToolTransportFunc {
		return func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
			return domain.ToolResult{Retryable: retryable}, err
		}
	}
	var calls []string
	refused := governedGateway(&calls, transportErr(nil, false))
	refused.Egress = fake.ToolEgressPolicyFunc(func(context.Context, int64, string) error { return fmt.Errorf("%w: destination", domain.ErrForbidden) })
	if _, err := refused.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrNotSent) || !errors.Is(err, domain.ErrForbidden) ||
		errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("a call refused before it left = %v", err)
	}
	for name, tc := range map[string]struct {
		transport fake.ToolTransportFunc
		want      error
		not       error
	}{
		"reset after send":    {transportErr(errors.New("connection reset"), false), domain.ErrEffectUnknown, domain.ErrNoEffect},
		"refused by the tool": {transportErr(fmt.Errorf("%w: sku unknown", domain.ErrNoEffect), false), domain.ErrNoEffect, domain.ErrEffectUnknown},
		"retryable exhausted": {transportErr(errors.New("503"), true), domain.ErrNoEffect, domain.ErrEffectUnknown},
	} {
		gateway := governedGateway(&calls, tc.transport)
		if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, tc.want) || errors.Is(err, tc.not) {
			t.Errorf("%s = %v, want %v and not %v", name, err, tc.want, tc.not)
		}
	}
	// An uncertain mutation is not retried unless the composition declares provider deduplication.
	attempt := 0
	uncertain := governedGateway(&calls, func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		attempt++
		if attempt == 1 {
			return domain.ToolResult{}, errors.New("timeout")
		}
		return domain.ToolResult{Retryable: true}, fmt.Errorf("%w: busy", domain.ErrNoEffect)
	})
	uncertain.Retry = retryEverything{}
	uncertain.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", MaxAttempts: 2}, nil
	}}
	if _, err := uncertain.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) || attempt != 1 {
		t.Fatalf("an unproven earlier attempt = %v after %d attempts, want unknown", err, attempt)
	}
	attempt = 0
	uncertain.Outcomes = map[string]domain.ToolOutcomeTraits{"search": {DedupesByKey: true}}
	if _, err := uncertain.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) || attempt != 2 {
		t.Fatalf("a deduplicated retry = %v after %d attempts, want two attempts and unknown", err, attempt)
	}
	invalid := governedGateway(&calls, transportErr(nil, false))
	invalid.Validator = fake.ToolResultValidatorFunc(func(context.Context, domain.ToolDefinition, domain.ToolResult) error { return errors.New("schema") })
	if _, err := invalid.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) || !errors.Is(err, ErrInvalidToolOutput) {
		t.Fatalf("invalid output of a sent mutation = %v, want unknown", err)
	}
	readOnly := governedGateway(&calls, transportErr(errors.New("connection reset"), false))
	readOnly.Registry = readOnlyRegistry(&calls)
	if _, err := readOnly.Invoke(context.Background(), validToolCall()); err == nil || errors.Is(err, domain.ErrEffectUnknown) || errors.Is(err, domain.ErrNoEffect) {
		t.Fatalf("a read-only failure = %v, want it unclassified", err)
	}
}

// A call that took effect but whose output cannot be returned, blocked by a guardrail or followed
// by a failed audit, is withheld, not failed.
func TestGovernedGatewayWithholdsTheOutputOfACallThatTookEffect(t *testing.T) {
	var calls []string
	ok := fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte("written"), Usage: domain.Usage{ToolCalls: 1}}, nil
	})
	blocked := guardedGateway(t, &calls, &fake.GuardrailEnforcer{AfterToolFunc: func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject, domain.ToolResult) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("%w: secret in output", domain.ErrForbidden)
	}}, ok)
	result, err := blocked.Invoke(context.Background(), validToolCall())
	if !errors.Is(err, domain.ErrOutputWithheld) || result.Output != nil || result.Usage.ToolCalls != 1 {
		t.Fatalf("a blocked output = %+v, %v", result, err)
	}
	audited := governedGateway(&calls, ok)
	audited.Audit = &fake.AuditSink{RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
		if record.Category == domain.DecisionCategoryToolInvoke {
			return errors.New("audit store down")
		}
		return nil
	}}
	if _, err := audited.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrOutputWithheld) {
		t.Fatalf("a failed audit after the effect = %v", err)
	}
}

// Decision 15: only a tool the composition declares lookup-capable may accept a call; its
// acceptance passes without output validation, and any other is invalid output of a sent call.
func TestGovernedGatewayAdmitsAnAcceptanceOnlyFromADeclaredLookupTool(t *testing.T) {
	var calls []string
	accepting := fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte("ignored"), Acceptance: &domain.ToolAcceptance{Handle: "job-7", LookupWithin: time.Hour}}, nil
	})
	undeclared := governedGateway(&calls, accepting)
	if _, err := undeclared.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) || !errors.Is(err, ErrInvalidToolOutput) {
		t.Fatalf("an undeclared acceptance = %v", err)
	}
	declared := governedGateway(&calls, accepting)
	declared.Outcomes = map[string]domain.ToolOutcomeTraits{"search": {Lookup: true}}
	declared.Lookup = lookupFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition) (domain.CallReport, error) {
		return domain.CallReport{Status: domain.CallUnknown}, nil
	})
	calls = nil
	result, err := declared.Invoke(context.Background(), validToolCall())
	if err != nil || result.Acceptance == nil || result.Acceptance.Handle != "job-7" || result.Output != nil {
		t.Fatalf("a declared acceptance = %+v, %v", result, err)
	}
	for _, call := range calls {
		if call == "validate" {
			t.Fatal("an acceptance carries no output to validate")
		}
	}
	malformed := governedGateway(&calls, func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{Acceptance: &domain.ToolAcceptance{Handle: "job", LookupWithin: 0}}, nil
	})
	malformed.Outcomes, malformed.Lookup = declared.Outcomes, declared.Lookup
	if _, err := malformed.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("a malformed acceptance = %v", err)
	}
	unbacked := governedGateway(&calls, accepting)
	unbacked.Outcomes = declared.Outcomes
	if _, err := unbacked.Invoke(context.Background(), validToolCall()); err == nil || !errors.Is(err, domain.ErrNotSent) {
		t.Fatalf("a lookup declaration without a lookup must refuse the composition: %v", err)
	}
}

type lookupFunc func(context.Context, domain.ToolCall, domain.ToolDefinition) (domain.CallReport, error)

func (function lookupFunc) Lookup(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (domain.CallReport, error) {
	return function(ctx, call, definition)
}

// Reconcile never sends: a VerifyEffect tool's effect answers first, then the declared lookup; a
// completed answer passes output validation and guardrails like a sent call's.
func TestGovernedGatewayReconcilesWithoutSending(t *testing.T) {
	ctx := context.Background()
	landed := domain.EffectObservation{Status: domain.EffectSatisfied, Evidence: effectEvidence, Output: []byte("written")}
	absent := domain.EffectObservation{Status: domain.EffectViolated, Reason: "no order", Evidence: effectEvidence}
	gateway, mutations := verifyingGateway(t, nil, landed, absent, errors.New("timed out"))
	report, err := gateway.Reconcile(ctx, validToolCall())
	if err != nil || report.Status != domain.CallCompleted || report.Resolution != domain.ResolvedByEffect || string(report.Result.Output) != "written" {
		t.Fatalf("a landed effect = %+v, %v", report, err)
	}
	if report, err = gateway.Reconcile(ctx, validToolCall()); err != nil || report.Status != domain.CallFailed || report.Resolution != domain.ResolvedByEffect {
		t.Fatalf("an absent effect = %+v, %v", report, err)
	}
	if report, err = gateway.Reconcile(ctx, validToolCall()); err != nil || report.Status != domain.CallUnknown {
		t.Fatalf("an unobservable effect without a lookup = %+v, %v", report, err)
	}
	if *mutations != 0 {
		t.Fatalf("reconciliation sent the call %d times", *mutations)
	}

	var calls []string
	looked := governedGateway(&calls, func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		t.Fatal("reconciliation must never send")
		return domain.ToolResult{}, nil
	})
	answer := domain.CallReport{Status: domain.CallCompleted, Result: domain.ToolResult{Output: []byte(`{"order":"o-1"}`)}}
	looked.Lookup = lookupFunc(func(_ context.Context, call domain.ToolCall, _ domain.ToolDefinition) (domain.CallReport, error) {
		if call.IdempotencyKey != "loop:k" {
			t.Fatalf("the lookup keyed by %q", call.IdempotencyKey)
		}
		return answer, nil
	})
	keyed := validToolCall()
	keyed.IdempotencyKey = "loop:k"
	if report, err = looked.Reconcile(ctx, keyed); err != nil || report.Status != domain.CallUnknown {
		t.Fatalf("a tool without a lookup declaration = %+v, %v", report, err)
	}
	looked.Outcomes = map[string]domain.ToolOutcomeTraits{"search": {Lookup: true, DedupesByKey: true}}
	if traits := looked.Traits("search"); !traits.Lookup || !traits.DedupesByKey || looked.Traits("other") != (domain.ToolOutcomeTraits{}) {
		t.Fatalf("traits = %+v", traits)
	}
	if report, err = looked.Reconcile(ctx, keyed); err != nil || report.Status != domain.CallCompleted || report.Resolution != domain.ResolvedByLookup {
		t.Fatalf("a lookup answer = %+v, %v", report, err)
	}
	looked.Validator = fake.ToolResultValidatorFunc(func(context.Context, domain.ToolDefinition, domain.ToolResult) error { return errors.New("schema") })
	if report, err = looked.Reconcile(ctx, keyed); err != nil || report.Status != domain.CallUnknown {
		t.Fatalf("an invalid lookup answer = %+v, %v", report, err)
	}
	guarded := guardedGateway(t, &calls, &fake.GuardrailEnforcer{AfterToolFunc: func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject, domain.ToolResult) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("%w: secret", domain.ErrForbidden)
	}}, nil)
	guarded.Outcomes, guarded.Lookup = looked.Outcomes, lookupFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition) (domain.CallReport, error) {
		return answer, nil
	})
	if report, err = guarded.Reconcile(ctx, keyed); err != nil || report.Status != domain.CallCompleted || !report.OutputWithheld || report.Result.Output != nil {
		t.Fatalf("a blocked lookup answer = %+v, %v", report, err)
	}
	failed := governedGateway(&calls, nil)
	failed.Outcomes = looked.Outcomes
	failed.Lookup = lookupFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition) (domain.CallReport, error) {
		return domain.CallReport{Status: domain.CallFailed}, nil
	})
	if report, err = failed.Reconcile(ctx, keyed); err != nil || report.Status != domain.CallFailed || report.Resolution != domain.ResolvedByLookup {
		t.Fatalf("a failed lookup answer = %+v, %v", report, err)
	}
}
