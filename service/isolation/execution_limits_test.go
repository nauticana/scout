package isolation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestExecutionLimitsComeFromTheFrozenRelease(t *testing.T) {
	limits := &ReleaseLimits{
		Releases: releaseWith(domain.ResourceLimits, `{"turns_per_minute":30,"max_steps":6,"max_tool_calls":4,"max_duration_seconds":90}`),
	}
	got, err := limits.ExecutionLimits(context.Background(), boundAgent())
	if err != nil {
		t.Fatal(err)
	}
	if got != (domain.ExecutionLimits{TurnsPerMinute: 30, MaxSteps: 6, MaxToolCalls: 4, MaxDuration: 90 * time.Second}) {
		t.Fatalf("limits = %+v", got)
	}
}

func TestExecutionLimitsLimitNothingWhenTheReleaseBindsNone(t *testing.T) {
	limits := &ReleaseLimits{Releases: releaseWith(domain.ResourceBudget, `{"tokens":10,"cost_minor_units":0,"period":"turn"}`)}
	got, err := limits.ExecutionLimits(context.Background(), boundAgent())
	if err != nil || got != (domain.ExecutionLimits{}) {
		t.Fatalf("limits = %+v, error = %v", got, err)
	}
}

func TestExecutionLimitsRefuseAMalformedOrOutOfRangeRelease(t *testing.T) {
	for name, value := range map[string]string{
		"negative":  `{"max_steps":-1}`,
		"text":      `{"max_steps":"six"}`,
		"too large": fmt.Sprintf(`{"max_duration_seconds":%d}`, int64(domain.MaxExecutionLimit)+1),
	} {
		limits := &ReleaseLimits{Releases: releaseWith(domain.ResourceLimits, value)}
		if _, err := limits.ExecutionLimits(context.Background(), boundAgent()); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
}

func TestExecutionLimitsFailClosedWithoutAFrozenRelease(t *testing.T) {
	limits := &ReleaseLimits{Releases: &fake.EffectiveReleaseRepository{}}
	if _, err := limits.ExecutionLimits(context.Background(), boundAgent()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want a missing release to refuse", err)
	}
	unpinned := boundAgent()
	unpinned.Release = ""
	if _, err := (&ReleaseLimits{Releases: stubReleases{}}).ExecutionLimits(context.Background(), unpinned); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want an unpinned principal refused", err)
	}
}
