package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// otherModelCatalog offers provider-b/model-b and refuses provider-b/retired as withdrawn.
type otherModelCatalog struct{}

func (otherModelCatalog) List(context.Context, int64) ([]domain.StudioModel, error) {
	return []domain.StudioModel{
		{Reference: domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}},
		{Reference: domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}},
		{Reference: domain.ModelReference{ProviderID: "provider-b", ModelID: "retired"}},
	}, nil
}

func (otherModelCatalog) Validate(_ context.Context, _ int64, selection domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
	if selection.Text != nil && selection.Text.ModelID == "retired" {
		return []domain.AgentFieldError{{Field: "models.text", Message: "model retired is not active"}}, nil
	}
	return nil, nil
}

// A draft frozen on another model is the same draft with only its text model replaced, held
// to the catalog like the draft's own model, and digested apart from it.
func TestFreezeDraftOnAnotherModelReplacesOnlyTheTextModel(t *testing.T) {
	ctx := context.Background()
	service := testDraftService(&studioQueryFake{rows: draftRows(), args: map[string][]any{}}, fake.DraftTester{})
	service.Catalog = otherModelCatalog{}
	own, err := service.FreezeDraft(ctx, humanActor(8, 9), "writer-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.FreezeDraft(ctx, humanActor(8, 9), "writer-a", &domain.ModelReference{ModelID: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	if text := other.Definition.Models.Text; text == nil || *text != (domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}) {
		t.Fatalf("text model = %+v, want provider-b/model-b", text)
	}
	if text := own.Definition.Models.Text; text == nil || text.ModelID != "model-a" {
		t.Fatalf("own text model = %+v", text)
	}
	if other.Definition.DefinitionDigest == own.Definition.DefinitionDigest || other.Definition.SettingsDigest != own.Definition.SettingsDigest {
		t.Fatalf("digests: own %q/%q, other %q/%q", own.Definition.DefinitionDigest, own.Definition.SettingsDigest,
			other.Definition.DefinitionDigest, other.Definition.SettingsDigest)
	}
	for name, model := range map[string]*domain.ModelReference{
		"unknown":   {ModelID: "missing"},
		"withdrawn": {ModelID: "retired"},
		"blank":     {ModelID: " "},
	} {
		if _, err := service.FreezeDraft(ctx, humanActor(8, 9), "writer-a", model); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s model = %v, want validation", name, err)
		}
	}
}

// The agent's model setting bounds a model a draft is frozen on as it bounds the draft's own.
func TestFreezeDraftOnAModelOutsideTheModelSettingIsRefused(t *testing.T) {
	ctx := context.Background()
	service := testDraftService(&studioQueryFake{rows: draftRows(), args: map[string][]any{}}, fake.DraftTester{})
	service.Catalog = otherModelCatalog{}
	service.Scopes = studioScopes([]domain.ScopedBinding{{ScopeID: "t:writer", ResourceKind: domain.ResourceModel,
		ResourceID: "model", ResourceVersion: "v1", MergeMode: domain.MergeReplace, Value: []byte(`["provider-a/model-a"]`)}})
	if _, err := service.FreezeDraft(ctx, humanActor(8, 9), "writer-a", nil); err != nil {
		t.Fatalf("the draft's own model within the setting = %v", err)
	}
	if _, err := service.FreezeDraft(ctx, humanActor(8, 9), "writer-a", &domain.ModelReference{ModelID: "model-b"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a model outside the setting = %v, want validation", err)
	}
}
