package domain

import "testing"

func TestAutonomyModeRank(t *testing.T) {
	ordered := []AutonomyMode{AutonomyHumanOnly, AutonomyAdvise, AutonomyDraft, AutonomyExecuteWithApproval, AutonomyBounded}
	for i, mode := range ordered {
		if got := mode.Rank(); got != i+1 {
			t.Fatalf("%s ranks %d, want %d", mode, got, i+1)
		}
	}
	for _, mode := range []AutonomyMode{"", "autonomous", "DRAFT"} {
		if got := mode.Rank(); got != 0 {
			t.Fatalf("%q ranks %d, want 0", mode, got)
		}
	}
}
