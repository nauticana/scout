package isolation

import (
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestFrozenExecutionLimitsReadUnstoredSettings(t *testing.T) {
	got, err := FrozenExecutionLimits([]domain.EffectiveResource{
		{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(`{"tokens":10,"cost_minor_units":0,"period":"turn"}`)},
		{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_steps":2,"max_tool_calls":3,"max_duration_seconds":40}`)},
	})
	if err != nil || got != (domain.ExecutionLimits{MaxSteps: 2, MaxToolCalls: 3, MaxDuration: 40 * time.Second}) {
		t.Fatalf("limits = %+v, error = %v", got, err)
	}
	if got, err := FrozenExecutionLimits(nil); err != nil || got != (domain.ExecutionLimits{}) {
		t.Fatalf("settings binding no limits = %+v, %v; want nothing limited", got, err)
	}
	malformed := []domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_tool_calls":-2}`)}}
	if _, err := FrozenExecutionLimits(malformed); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want malformed limits refused", err)
	}
}
