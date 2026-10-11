package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

const alertsPolicy = `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_silence_minutes":120}`

// Health alerts are operational: Studio shows and edits them, and no release freezes them.
func TestHealthAlertsAreNotFrozenIntoTheRelease(t *testing.T) {
	plain, _ := publishService()
	without, err := plain.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	alerted, _ := publishService()
	alerted.Scopes = studioScopes([]domain.ScopedBinding{{TenantID: 8, ScopeID: "t:writer", ResourceKind: domain.ResourceHealthAlerts,
		ResourceID: string(domain.ResourceHealthAlerts), MergeMode: domain.MergeReplace, Value: []byte(alertsPolicy)}})
	with, err := alerted.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if with.Definition.DefinitionDigest != without.Definition.DefinitionDigest || len(with.Settings.Resources) != 0 {
		t.Fatalf("alerts frozen: settings %+v", with.Settings.Resources)
	}
	settings, err := alerted.Settings(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || len(settings.Effective) != 1 || settings.Effective[0].ResourceKind != domain.ResourceHealthAlerts {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	scopes := domain.PromptScopes{AgentScopeID: "a:writer-a", TypeScopeID: "t:writer"}
	if _, err = settingBinding(humanActor(8, 9), scopes, domain.AgentSettingChange{AgentID: "writer-a", ScopeID: "a:writer-a",
		ResourceKind: domain.ResourceHealthAlerts, ResourceID: "alerts", Value: []byte(alertsPolicy)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a second alerts resource id = %v", err)
	}
	if _, err = settingBinding(humanActor(8, 9), scopes, domain.AgentSettingChange{AgentID: "writer-a", ScopeID: "a:writer-a",
		ResourceKind: domain.ResourceHealthAlerts, ResourceID: string(domain.ResourceHealthAlerts), Value: []byte(`{"max_failure_rate_bp":-1}`)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a malformed policy = %v", err)
	}
}

func TestAgentPublisherFreezesNoHealthAlerts(t *testing.T) {
	var stored domain.EffectiveRelease
	publisher := &AgentPublisher{
		Compiler: fake.AgentCompilerFunc(func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error) { return validGraph(), nil }),
		Store:    fake.AgentPublicationStoreFunc(func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error { return nil }),
		Effective: fake.EffectiveConfigCompilerFunc(func(_ context.Context, request domain.CompileRequest) (domain.EffectiveRelease, error) {
			return domain.EffectiveRelease{TenantID: request.TenantID, AgentID: request.AgentID, AgentVersion: request.AgentVersion,
				Resources: []domain.EffectiveResource{{ResourceKind: domain.ResourceHealthAlerts, ResourceID: "health_alerts", Value: []byte(alertsPolicy)},
					{ResourceKind: domain.ResourceTool, ResourceID: "tool", Value: []byte(`["search"]`)}}}, nil
		}),
		Releases: effectiveReleaseSink(func(release domain.EffectiveRelease) error { stored = release; return nil }),
	}
	if err := publisher.Publish(context.Background(), 7, validDefinition()); err != nil {
		t.Fatal(err)
	}
	if len(stored.Resources) != 1 || stored.Resources[0].ResourceKind != domain.ResourceTool || stored.Digest != scope.Digest(stored) {
		t.Fatalf("stored = %+v", stored)
	}
}
