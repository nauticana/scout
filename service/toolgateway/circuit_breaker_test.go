package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func testBreaker(t *testing.T, config CircuitBreakerConfig, now *time.Time) *CircuitBreaker {
	t.Helper()
	config.Now = func() time.Time { return *now }
	breaker, err := NewCircuitBreaker(config)
	if err != nil {
		t.Fatal(err)
	}
	return breaker
}

func breakerCall(toolID string) domain.ToolCall {
	return domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, RequestID: "r1", ToolID: toolID, ToolVersion: "v1"}
}

func TestCircuitBreakerStateMachine(t *testing.T) {
	clock := time.Unix(0, 0)
	breaker := testBreaker(t, CircuitBreakerConfig{FailureThreshold: 2, Window: time.Minute, OpenDuration: 10 * time.Second, MaxEntries: 8}, &clock)
	ctx := context.Background()

	steps := []struct {
		name      string
		advance   time.Duration
		skipAllow bool
		allowErr  bool
		outcome   *bool
		wantState string
	}{
		{name: "first failure", outcome: ptr(false), wantState: "closed"},
		{name: "threshold opens", outcome: ptr(false), wantState: "open"},
		{name: "open rejects", allowErr: true, wantState: "open"},
		{name: "probe admitted after open duration", advance: 10 * time.Second, wantState: "half-open"},
		{name: "second concurrent probe rejected", allowErr: true, wantState: "half-open"},
		{name: "probe fails reopens", skipAllow: true, outcome: ptr(false), wantState: "open"},
		{name: "probe admitted again", advance: 10 * time.Second, wantState: "half-open"},
		{name: "probe succeeds closes", skipAllow: true, outcome: ptr(true), wantState: "closed"},
		{name: "closed admits", wantState: "closed"},
	}
	for _, step := range steps {
		clock = clock.Add(step.advance)
		if !step.skipAllow {
			err := breaker.Allow(ctx, 7, "search")
			if step.allowErr {
				if !errors.Is(err, domain.ErrCircuitOpen) {
					t.Fatalf("%s: allow error = %v", step.name, err)
				}
			} else if err != nil {
				t.Fatalf("%s: allow error = %v", step.name, err)
			}
		}
		if step.outcome != nil {
			var recordErr error
			if *step.outcome {
				recordErr = breaker.RecordSuccess(ctx, 7, "search")
			} else {
				recordErr = breaker.RecordFailure(ctx, 7, "search")
			}
			if recordErr != nil {
				t.Fatalf("%s: record error = %v", step.name, recordErr)
			}
		}
		if state, _ := breaker.State(7, "search"); state != step.wantState {
			t.Fatalf("%s: state = %s, want %s", step.name, state, step.wantState)
		}
	}
}

func ptr(value bool) *bool { return &value }

func TestCircuitBreakerFencesStaleProbe(t *testing.T) {
	clock := time.Unix(0, 0)
	breaker := testBreaker(t, CircuitBreakerConfig{FailureThreshold: 1, Window: time.Minute, OpenDuration: time.Second, MaxEntries: 4}, &clock)
	ctx := context.Background()
	call, definition := breakerCall("search"), domain.ToolDefinition{Endpoint: "https://tool.example.com/x"}

	generation, err := breaker.Admit(ctx, call, definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := breaker.Settle(ctx, call, definition, generation, false); err != nil {
		t.Fatal(err)
	}
	if state, _ := breaker.State(7, "search"); state != "open" {
		t.Fatalf("state = %s", state)
	}
	clock = clock.Add(time.Second)
	probe, err := breaker.Admit(ctx, call, definition)
	if err != nil || probe == generation {
		t.Fatalf("probe generation = %d (admit generation %d), error = %v", probe, generation, err)
	}
	// The outcome of the pre-open call arrives late and must not close the newer generation.
	if err := breaker.Settle(ctx, call, definition, generation, true); err != nil {
		t.Fatal(err)
	}
	if state, _ := breaker.State(7, "search"); state != "half-open" {
		t.Fatalf("stale success changed state to %s", state)
	}
	if err := breaker.Settle(ctx, call, definition, probe, true); err != nil {
		t.Fatal(err)
	}
	if state, _ := breaker.State(7, "search"); state != "closed" {
		t.Fatalf("state = %s after probe success", state)
	}
}

func TestCircuitBreakerWindowExpiryAndMinSamples(t *testing.T) {
	clock := time.Unix(0, 0)
	breaker := testBreaker(t, CircuitBreakerConfig{FailureThreshold: 2, MinSamples: 4, Window: 10 * time.Second, OpenDuration: time.Second, MaxEntries: 4}, &clock)
	ctx := context.Background()
	for range 3 {
		if err := breaker.Allow(ctx, 7, "search"); err != nil {
			t.Fatal(err)
		}
		if err := breaker.RecordFailure(ctx, 7, "search"); err != nil {
			t.Fatal(err)
		}
	}
	if state, _ := breaker.State(7, "search"); state != "closed" {
		t.Fatalf("min samples not honored: %s", state)
	}
	clock = clock.Add(11 * time.Second)
	if err := breaker.Allow(ctx, 7, "search"); err != nil {
		t.Fatal(err)
	}
	if err := breaker.RecordFailure(ctx, 7, "search"); err != nil {
		t.Fatal(err)
	}
	if state, _ := breaker.State(7, "search"); state != "closed" {
		t.Fatalf("window did not reset: %s", state)
	}
}

func TestCircuitBreakerSharedDestinationHealthAndCardinality(t *testing.T) {
	clock := time.Unix(0, 0)
	breaker := testBreaker(t, CircuitBreakerConfig{FailureThreshold: 1, Window: time.Minute, OpenDuration: time.Minute, MaxEntries: 2, SharedDestinationHealth: true, MaxDestinations: 2}, &clock)
	ctx := context.Background()
	definition := domain.ToolDefinition{Endpoint: "https://Shared.example.com/a"}
	first := breakerCall("tool-a")
	generation, err := breaker.Admit(ctx, first, definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := breaker.Settle(ctx, first, definition, generation, false); err != nil {
		t.Fatal(err)
	}
	other := domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 9}, RequestID: "r2", ToolID: "tool-b", ToolVersion: "v1"}
	if _, err := breaker.Admit(ctx, other, domain.ToolDefinition{Endpoint: "https://shared.example.com/b"}); !errors.Is(err, domain.ErrCircuitOpen) {
		t.Fatalf("shared destination health not applied: %v", err)
	}
	if _, err := breaker.Admit(ctx, other, domain.ToolDefinition{Endpoint: "https://other.example.com/b"}); err != nil {
		t.Fatalf("unrelated destination: %v", err)
	}
	if _, err := breaker.Admit(ctx, breakerCall("tool-c"), domain.ToolDefinition{Endpoint: "not a url"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid endpoint: %v", err)
	}
	for i := range 4 {
		call := breakerCall(fmt.Sprintf("tool-%d", i))
		if _, err := breaker.Admit(ctx, call, domain.ToolDefinition{Endpoint: "https://other.example.com/b"}); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	if breaker.entries.Len() > 2 {
		t.Fatalf("tenant×tool entries = %d, want bounded by 2", breaker.entries.Len())
	}
}

func TestCircuitBreakerValidatesConfigAndKeys(t *testing.T) {
	cases := map[string]CircuitBreakerConfig{
		"zero threshold":        {Window: time.Second, OpenDuration: time.Second, MaxEntries: 1},
		"zero window":           {FailureThreshold: 1, OpenDuration: time.Second, MaxEntries: 1},
		"zero open duration":    {FailureThreshold: 1, Window: time.Second, MaxEntries: 1},
		"zero entries":          {FailureThreshold: 1, Window: time.Second, OpenDuration: time.Second},
		"min samples too small": {FailureThreshold: 3, MinSamples: 2, Window: time.Second, OpenDuration: time.Second, MaxEntries: 1},
		"destinations unset":    {FailureThreshold: 1, Window: time.Second, OpenDuration: time.Second, MaxEntries: 1, SharedDestinationHealth: true},
	}
	for name, config := range cases {
		if _, err := NewCircuitBreaker(config); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	clock := time.Unix(0, 0)
	breaker := testBreaker(t, DefaultCircuitBreakerConfig(), &clock)
	if err := breaker.Allow(context.Background(), 0, "search"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tenant validation: %v", err)
	}
	if err := breaker.Allow(context.Background(), 7, "  "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tool validation: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := breaker.Allow(canceled, 7, "search"); !errors.Is(err, context.Canceled) {
		t.Fatalf("context: %v", err)
	}
}

func TestDefaultFailureClassifier(t *testing.T) {
	cases := []struct {
		name       string
		classifier DefaultFailureClassifier
		err        error
		want       bool
	}{
		{name: "success", want: false},
		{name: "transport", err: errors.New("connection reset"), want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "cancellation", err: fmt.Errorf("aborted: %w", context.Canceled), want: false},
		{name: "validation", err: fmt.Errorf("%w: bad args", domain.ErrValidation), want: false},
		{name: "authorization", err: fmt.Errorf("%w: not allowed", domain.ErrForbidden), want: false},
		{name: "rate limited", err: fmt.Errorf("%w: slow down", domain.ErrRateLimited), want: false},
		{name: "invalid output ignored", err: fmt.Errorf("%w: schema", ErrInvalidToolOutput), want: false},
		{name: "invalid output counted", classifier: DefaultFailureClassifier{CountValidationFailures: true}, err: fmt.Errorf("%w: schema", ErrInvalidToolOutput), want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.classifier.CountsAsDependencyFailure(context.Background(), breakerCall("search"), domain.ToolResult{}, testCase.err); got != testCase.want {
				t.Fatalf("counts = %v, want %v", got, testCase.want)
			}
		})
	}
}
