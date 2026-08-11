package runtime

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

type catalogFake struct{ models []domain.StudioModel }

func (c catalogFake) List(context.Context, int64) ([]domain.StudioModel, error) {
	return c.models, nil
}

func (c catalogFake) Validate(context.Context, int64, domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
	return nil, nil
}

func deployedOn(alias string, provider, model string) domain.DeployedAgent {
	reference := domain.ModelReference{ProviderID: provider, ModelID: model}
	return domain.DeployedAgent{
		AliasID: alias, AgentID: alias, Active: true, Enabled: true, Version: "3",
		Definition: &domain.AgentDefinition{Models: domain.AgentModelSelection{Text: &reference}},
	}
}

func TestReadinessNarrowsReadyAgentsOnly(t *testing.T) {
	live := domain.ModelReference{ProviderID: "anthropic", ModelID: "opus"}
	withdrawn := domain.ModelReference{ProviderID: "openai", ModelID: "gone"}
	uncredentialed := domain.ModelReference{ProviderID: "google", ModelID: "gemini"}
	resolver := &ReadinessResolver{
		Catalog: catalogFake{models: []domain.StudioModel{
			{Reference: live, Active: true},
			{Reference: withdrawn, Active: false},
			{Reference: uncredentialed, Active: true},
		}},
		CredentialMissing: func(_ context.Context, providerID string) bool { return providerID == "google" },
	}

	disabled := deployedOn("SD", "anthropic", "opus")
	disabled.Enabled = false

	states, err := resolver.Resolve(context.Background(), 3, map[string]domain.DeployedAgent{
		"CP": deployedOn("CP", "anthropic", "opus"),
		"BL": deployedOn("BL", "openai", "gone"),
		"RR": deployedOn("RR", "google", "gemini"),
		"SD": disabled,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for alias, want := range map[string]domain.AgentReadiness{
		"CP": domain.AgentReady,
		"BL": domain.AgentMissingModel,
		"RR": domain.AgentError,
		"SD": domain.AgentDisabled,
	} {
		if states[alias].Readiness != want {
			t.Errorf("%s = %s, want %s (%s)", alias, states[alias].Readiness, want, states[alias].Reason)
		}
	}
	if states["RR"].Version != "3" {
		t.Errorf("version not carried through: %+v", states["RR"])
	}
	// A model the catalog does not list at all is unavailable, not silently ready.
	unknown, err := resolver.Resolve(context.Background(), 3,
		map[string]domain.DeployedAgent{"QR": deployedOn("QR", "openai", "never-seeded")})
	if err != nil || unknown["QR"].Readiness != domain.AgentMissingModel {
		t.Fatalf("unlisted model = %+v err = %v", unknown["QR"], err)
	}
}

func TestReadinessRequiresCatalog(t *testing.T) {
	resolver := &ReadinessResolver{}
	if _, err := resolver.Resolve(context.Background(), 3, nil); err == nil {
		t.Fatal("a resolver with no catalog must fail loudly")
	}
}
