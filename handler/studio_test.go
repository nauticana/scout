package handler

import (
	"strings"
	"testing"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

func TestStudioDraftCompatibilityMapping(t *testing.T) {
	text := "tenant"
	override := "agent"
	request := api.AgentDraft{
		AgentName: "writer-a", AgentType: "writer", DisplayName: "Writer", Enabled: true,
		Models: api.AgentModelSelection{TextModel: "model-a"}, ExpectedAgentRevision: 3, ExpectedTypeDefaultsRevision: 5,
		Languages: []api.AgentLanguageDraft{{LanguageCode: "en-US", PromptSections: []api.AgentPromptSection{{
			PromptHeaderID: 4, BusinessText: "base", DefaultText: &text, OverrideText: &override, Overwrite: true,
		}}}},
	}

	draft := domainDraft(request)
	if draft.AgentID != "writer-a" || !draft.Active || draft.Models.Text.ProviderID != "" {
		t.Fatalf("unexpected domain draft: %+v", draft)
	}
	section := draft.Languages[0].Sections[0]
	if section.TenantDefault.Instruction != "tenant" || !section.AgentOverride.Overwrite {
		t.Fatalf("prompt mapping lost inheritance data: %+v", section)
	}
	response := apiDraft(draft)
	if response.Models.TextModel != "model-a" || *response.Languages[0].PromptSections[0].OverrideText != "agent" {
		t.Fatalf("unexpected compatibility response: %+v", response)
	}
}

func TestStudioValidationErrorMapping(t *testing.T) {
	_, err := mapStudioError(nil, &controlplane.StudioValidationError{Fields: []domain.AgentFieldError{{Field: "display_name", Message: "required"}}})
	if err == nil || !strings.Contains(err.Error(), "display_name") {
		t.Fatalf("field detail was not serialized: %v", err)
	}
}

func TestStudioRoutesCoverCompatibilityContract(t *testing.T) {
	routes := (&StudioHandler{}).Routes()
	paths := []string{
		api.StudioAgentsPath, api.StudioAgentPath, api.StudioDraftPath, api.StudioEnabledPath,
		api.StudioTestPath, api.StudioPublishPath, api.StudioRestorePath, api.StudioResetPath,
		api.StudioSetDefaultPath, api.StudioHistoryPath, api.StudioAuditPath, api.StudioSectionsPath,
		api.StudioModelsPath,
	}
	for _, path := range paths {
		if routes[path] == nil {
			t.Fatalf("missing Studio route %s", path)
		}
	}
	if len(routes) != len(paths) {
		t.Fatalf("routes contain undocumented paths: %v", routes)
	}
}
