package controlplane

import (
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestAllowedTransitionEnforcesTheStateMachine(t *testing.T) {
	legal := []struct{ from, to domain.AgentState }{
		{domain.AgentStateDraft, domain.AgentStateActive},
		{domain.AgentStateActive, domain.AgentStateSuspended},
		{domain.AgentStateActive, domain.AgentStateDraining},
		{domain.AgentStateSuspended, domain.AgentStateActive},
		{domain.AgentStateDraining, domain.AgentStateRetired},
	}
	for _, move := range legal {
		if !allowedTransition(move.from, move.to) {
			t.Fatalf("%s → %s must be allowed", move.from, move.to)
		}
	}
	illegal := []struct{ from, to domain.AgentState }{
		{domain.AgentStateDraft, domain.AgentStateSuspended},
		{domain.AgentStateRetired, domain.AgentStateActive},
		{domain.AgentStateDraining, domain.AgentStateActive},
		{domain.AgentStateActive, domain.AgentStateActive},
	}
	for _, move := range illegal {
		if allowedTransition(move.from, move.to) {
			t.Fatalf("%s → %s must be refused", move.from, move.to)
		}
	}
}

func TestMissingPackagesReportsDriftAgainstTheLatestType(t *testing.T) {
	pinned := []domain.CapabilityRef{{PackageID: "invoice", PackageVersion: "1"}}
	latest := []domain.CapabilityRef{
		{PackageID: "invoice", PackageVersion: "2"},
		{PackageID: "audit", PackageVersion: "1"},
	}
	missing := missingPackages(pinned, latest)
	if len(missing) != 2 {
		t.Fatalf("missing = %+v, want both the bumped and the new package", missing)
	}
}

func TestTypeVersionDigestCoversCapabilitiesAndAutonomy(t *testing.T) {
	base := domain.AgentTypeVersion{
		AgentTypeID: "invoice-analyst", TypeVersion: "1", Autonomy: domain.AutonomyDraft,
		Packages: []domain.CapabilityRef{{PackageID: "invoice", PackageVersion: "1", Required: true}},
	}
	bumped := base
	bumped.Packages = []domain.CapabilityRef{{PackageID: "invoice", PackageVersion: "2", Required: true}}
	raised := base
	raised.Autonomy = domain.AutonomyBounded

	if TypeVersionDigest(base) == TypeVersionDigest(bumped) {
		t.Fatal("a changed capability version must change the digest")
	}
	if TypeVersionDigest(base) == TypeVersionDigest(raised) {
		t.Fatal("a changed autonomy mode must change the digest")
	}
}
