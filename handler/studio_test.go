package handler

import (
	"strings"
	"testing"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

func TestStudioDraftLayerMapping(t *testing.T) {
	request := api.AgentDraft{
		AgentName: "writer-a", AgentType: "writer", DisplayName: "Writer", Enabled: true,
		Models: api.AgentModelSelection{TextModel: "model-a"}, ExpectedAgentRevision: 3, ExpectedTypeDefaultsRevision: 5,
		Languages: []api.AgentLanguageDraft{{LanguageCode: "en-US", PromptSections: []api.AgentPromptSection{{
			PromptHeaderID: 4, Layers: []api.AgentPromptLayer{
				{ScopeID: "global", ScopeKind: "platform", MergeMode: "replace", Instruction: "base"},
				{ScopeID: "t:writer", ScopeKind: "agent_type", MergeMode: "append", Sealed: true, Editable: true, Instruction: "tenant"},
				{ScopeID: "a:writer-a", ScopeKind: "agent", MergeMode: "replace", Instruction: "agent"},
			},
		}}}},
	}

	draft := domainDraft(request)
	if draft.AgentID != "writer-a" || !draft.Active || draft.Models.Text.ProviderID != "" {
		t.Fatalf("unexpected domain draft: %+v", draft)
	}
	layers := draft.Languages[0].Sections[0].Layers
	if len(layers) != 3 || layers[1].Instruction != "tenant" || !layers[1].Sealed || layers[2].MergeMode != domain.MergeReplace {
		t.Fatalf("prompt mapping lost its layers: %+v", layers)
	}
	// Editable is the server's statement; a client cannot grant it to itself.
	if layers[1].Editable {
		t.Fatalf("editable must not be read from the request: %+v", layers[1])
	}
	response := apiDraft(draft)
	if response.Models.TextModel != "model-a" || response.Languages[0].PromptSections[0].Layers[2].Instruction != "agent" {
		t.Fatalf("unexpected response: %+v", response)
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
