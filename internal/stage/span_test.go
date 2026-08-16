package stage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestErrorClassUsesSentinelIdentity(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"wrapped sentinel", fmt.Errorf("rate limited: %w", domain.ErrRateLimited), "rate_limited"},
		{"context canceled", context.Canceled, "canceled"},
		{"turn canceled", At(domain.StageModel, domain.ErrTurnCanceled), "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"text is not a class", errors.New("rate limited"), ErrorClassInternal},
		{"joined", errors.Join(errors.New("publish down"), domain.ErrBudgetExceeded), "budget_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorClass(tc.err); got != tc.want {
				t.Fatalf("ErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSpanEnd(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	versions := domain.ComponentVersions{Model: "m1", Release: "r1"}
	cases := []struct {
		name        string
		outcome     domain.ObservationOutcome
		err         error
		wantOutcome domain.ObservationOutcome
		wantStage   domain.TurnStage
		wantClass   string
	}{
		{"ok derived", "", nil, domain.OutcomeOK, domain.StageModel, ""},
		{"error derived", "", errors.New("boom"), domain.OutcomeError, domain.StageModel, ErrorClassInternal},
		{"canceled derived", "", context.Canceled, domain.OutcomeCanceled, domain.StageModel, "canceled"},
		{"explicit degraded", domain.OutcomeDegraded, nil, domain.OutcomeDegraded, domain.StageModel, ""},
		{"stage error re-attributes", "", At(domain.StageGuardrail, domain.ErrForbidden), domain.OutcomeError, domain.StageGuardrail, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			span := Begin(start, domain.StageModel, "pump", versions)
			span.Observation.TenantID = 7
			got := span.End(start.Add(250*time.Millisecond), tc.outcome, domain.Usage{OutputTokens: 3}, tc.err)
			if got.Outcome != tc.wantOutcome || got.Stage != tc.wantStage || got.ErrorClass != tc.wantClass {
				t.Fatalf("observation = %+v", got)
			}
			if got.Duration != 250*time.Millisecond || got.Usage.OutputTokens != 3 || got.TenantID != 7 || got.Versions != versions || got.Component != "pump" || !got.StartedAt.Equal(start) {
				t.Fatalf("observation = %+v", got)
			}
		})
	}
}
