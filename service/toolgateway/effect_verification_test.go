package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var effectEvidence = []domain.ObjectRef{{URI: "memory://observed/1", Digest: "d1"}}

// verifyingGateway scripts one observation per Observe call, before and after the mutation.
func verifyingGateway(t *testing.T, transportErrs []error, observations ...any) (*GovernedGateway, *int) {
	t.Helper()
	var calls []string
	mutations := 0
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		mutations++
		if mutations <= len(transportErrs) && transportErrs[mutations-1] != nil {
			return domain.ToolResult{Retryable: true}, transportErrs[mutations-1]
		}
		return domain.ToolResult{Output: []byte("written"), Usage: domain.Usage{ToolCalls: 1}}, nil
	}))
	gateway.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", VerifyEffect: true}, nil
	}}
	gateway.Effects = fake.ToolEffectVerifierFunc(func(_ context.Context, _ domain.ToolCall, _ domain.ToolDefinition, result *domain.ToolResult) (domain.EffectObservation, error) {
		if len(observations) == 0 {
			t.Fatal("unexpected observation")
		}
		next := observations[0]
		observations = observations[1:]
		if err, failed := next.(error); failed {
			return domain.EffectObservation{}, err
		}
		return next.(domain.EffectObservation), nil
	})
	return gateway, &mutations
}

func TestVerifiedEffectCompletesOnlyWhenObserved(t *testing.T) {
	absent := domain.EffectObservation{Status: domain.EffectViolated}
	held := domain.EffectObservation{Status: domain.EffectSatisfied, Evidence: effectEvidence}
	stale := domain.EffectObservation{Status: domain.EffectViolated, Reason: "old value", Evidence: effectEvidence}

	gateway, mutations := verifyingGateway(t, nil, absent, held)
	result, err := gateway.Invoke(context.Background(), validToolCall())
	if err != nil || string(result.Output) != "written" || result.Effect == nil || result.Effect.Status != domain.EffectSatisfied || result.Effect.Reconciled || result.Effect.ObservedAt.IsZero() {
		t.Fatalf("accepted and observed must complete: %+v, %v", result, err)
	}

	gateway, mutations = verifyingGateway(t, nil, absent, stale)
	result, err = gateway.Invoke(context.Background(), validToolCall())
	if !errors.Is(err, domain.ErrEffectViolated) || *mutations != 1 || result.Output != nil || result.Usage.ToolCalls != 1 || result.Effect.Status != domain.EffectViolated {
		t.Fatalf("accepted but unchanged must fail once, keeping usage: %+v, %v, mutations %d", result, err, *mutations)
	}

	for name, unobservable := range map[string]any{
		"observer failed": errors.New("read timed out"),
		"no evidence":     domain.EffectObservation{Status: domain.EffectSatisfied},
		"unrecognized":    domain.EffectObservation{Status: "maybe", Evidence: effectEvidence},
	} {
		gateway, mutations = verifyingGateway(t, nil, absent, unobservable)
		result, err = gateway.Invoke(context.Background(), validToolCall())
		if !errors.Is(err, domain.ErrEffectUnknown) || *mutations != 1 || result.Output != nil || result.Effect.Status != domain.EffectUnknown {
			t.Fatalf("%s: an unobservable effect is unknown, never success: %+v, %v", name, result, err)
		}
	}
}

func TestVerifiedEffectReconcilesBeforeMutating(t *testing.T) {
	landed := domain.EffectObservation{Status: domain.EffectSatisfied, Evidence: effectEvidence, Output: []byte("written")}
	gateway, mutations := verifyingGateway(t, nil, landed)
	result, err := gateway.Invoke(context.Background(), validToolCall())
	if err != nil || *mutations != 0 || string(result.Output) != "written" || !result.Effect.Reconciled {
		t.Fatalf("an effect that already holds must not be repeated: %+v, %v, mutations %d", result, err, *mutations)
	}

	// A transport failure leaves the outcome unknown; the retry observes it landed instead of sending it again.
	gateway, mutations = verifyingGateway(t, []error{errors.New("connection reset")}, domain.EffectObservation{Status: domain.EffectViolated}, landed)
	result, err = gateway.Invoke(context.Background(), validToolCall())
	if err != nil || *mutations != 1 || !result.Effect.Reconciled {
		t.Fatalf("retry must reconcile from observation: %+v, %v, mutations %d", result, err, *mutations)
	}

	gateway, mutations = verifyingGateway(t, nil, errors.New("read timed out"))
	if _, err = gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) || *mutations != 0 {
		t.Fatalf("a blind mutation must be refused: %v, mutations %d", err, *mutations)
	}

	gateway, mutations = verifyingGateway(t, nil)
	gateway.Effects = nil
	if _, err = gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrNotReady) || *mutations != 0 {
		t.Fatalf("a tool requiring verification is refused without a verifier: %v", err)
	}
}

func TestProvenAbsentEffectIsResentOnlyWhenTheContractDeclaresItSafe(t *testing.T) {
	absent := domain.EffectObservation{Status: domain.EffectViolated}
	stale := domain.EffectObservation{Status: domain.EffectViolated, Reason: "old value", Evidence: effectEvidence}
	held := domain.EffectObservation{Status: domain.EffectSatisfied, Evidence: effectEvidence}
	resendable := &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", VerifyEffect: true, RetryWhenEffectAbsent: true}, nil
	}}

	gateway, mutations := verifyingGateway(t, nil, absent, stale, absent, held)
	gateway.Registry = resendable
	result, err := gateway.Invoke(context.Background(), validToolCall())
	if err != nil || *mutations != 2 || result.Effect.Status != domain.EffectSatisfied {
		t.Fatalf("an absent effect must be sent again: %+v, %v, mutations %d", result, err, *mutations)
	}

	// The attempt ceiling still binds, and the last observation is what the caller gets.
	gateway, mutations = verifyingGateway(t, nil, absent, stale, absent, stale)
	gateway.Registry = resendable
	result, err = gateway.Invoke(context.Background(), validToolCall())
	if !errors.Is(err, domain.ErrEffectViolated) || *mutations != 2 || result.Effect.Status != domain.EffectViolated {
		t.Fatalf("retries are bounded: %+v, %v, mutations %d", result, err, *mutations)
	}

	// An unknown effect is never resent, whatever the contract says.
	gateway, mutations = verifyingGateway(t, nil, absent, errors.New("read timed out"))
	gateway.Registry = resendable
	if _, err = gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrEffectUnknown) || *mutations != 1 {
		t.Fatalf("an unknown effect must not be resent: %v, mutations %d", err, *mutations)
	}
}
