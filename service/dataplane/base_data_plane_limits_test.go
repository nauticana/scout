package dataplane

import (
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/isolation"
)

// The runtime's step and duration limits come from the same reader as the gateway's
// operating mode, so the two cannot disagree about a release.
func TestBaseDataPlaneGivesTheRuntimeItsPrincipalLimits(t *testing.T) {
	injected := &fake.PrincipalLimits{}
	for name, limits := range map[string]contract.PrincipalLimits{"injected": injected, "default": nil} {
		plane := composablePlane()
		plane.PrincipalLimits = limits
		if err := plane.Compose(); err != nil {
			t.Fatalf("%s: Compose: %v", name, err)
		}
		t.Cleanup(func() { _ = plane.Close() })
		if limits == nil {
			if _, ok := plane.PrincipalLimits.(*isolation.ReleaseLimits); !ok {
				t.Fatalf("default limits = %T, want the frozen release reader", plane.PrincipalLimits)
			}
		} else if plane.PrincipalLimits != limits {
			t.Fatalf("%s: the injected limits were replaced", name)
		}
		if runtime := plane.TurnRuntime.(*TurnRuntime); runtime.Limits != plane.PrincipalLimits {
			t.Fatalf("%s: runtime reads %v, want %v", name, runtime.Limits, plane.PrincipalLimits)
		}
	}
}
