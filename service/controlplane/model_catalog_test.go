package controlplane

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

func catalogFake(rows map[string][][]any) *ModelCatalog {
	return &ModelCatalog{qs: &studioQueryFake{rows: rows, args: map[string][]any{}}}
}

func TestListJoinsCapabilitiesAndRates(t *testing.T) {
	catalog := catalogFake(map[string][][]any{
		qCatalogModels: {
			{"anthropic", "claude-opus-5", "Claude Opus 5", int64(1000000), int64(128000), true},
			{"google", "gemini-3.1-flash-image", "Gemini 3.1 Flash (Image)", int64(2048), int64(1024), true},
		},
		qCatalogCapabilities: {
			{"anthropic", "claude-opus-5", "text"},
			{"google", "gemini-3.1-flash-image", "image"},
			{"google", "gemini-3.1-flash-image", "text"},
		},
		qCatalogPrices: {
			{"anthropic", "claude-opus-5", "CRD", int64(20000000), int64(100000000), int64(0), int64(0)},
			{"google", "gemini-3.1-flash-image", "CRD", int64(0), int64(0), int64(120000), int64(0)},
		},
	})
	models, err := catalog.List(context.Background(), 8)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models", len(models))
	}
	if models[0].DisplayName != "Claude Opus 5" || models[0].ContextTokenLimit != 1000000 {
		t.Fatalf("model = %+v", models[0])
	}
	if len(models[0].Rates) != 2 || models[0].Rates[0].UsageCategory != RateInputPerMillion {
		t.Fatalf("rates = %+v", models[0].Rates)
	}
	// Zero-amount rates are omitted rather than reported as free.
	if len(models[1].Rates) != 1 || models[1].Rates[0].UsageCategory != RateImage {
		t.Fatalf("media rates = %+v", models[1].Rates)
	}
	if len(models[1].Capabilities) != 2 {
		t.Fatalf("a model may serve several modalities, got %v", models[1].Capabilities)
	}
}

// A model is valid for a slot only if it declares that modality.
func TestValidateChecksCapability(t *testing.T) {
	catalog := catalogFake(map[string][][]any{
		qCatalogModel: {{true, "text"}},
	})
	fields, err := catalog.Validate(context.Background(), 8, domain.AgentModelSelection{
		Text:  &domain.ModelReference{ProviderID: "anthropic", ModelID: "claude-opus-5"},
		Image: &domain.ModelReference{ProviderID: "anthropic", ModelID: "claude-opus-5"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(fields) != 1 || fields[0].Field != "models.image" {
		t.Fatalf("a text-only model must be rejected for the image slot, got %+v", fields)
	}
}

func TestValidateRejectsUnknownAndWithdrawnModels(t *testing.T) {
	unknown := catalogFake(map[string][][]any{})
	fields, _ := unknown.Validate(context.Background(), 8, domain.AgentModelSelection{
		Text: &domain.ModelReference{ProviderID: "anthropic", ModelID: "ghost"},
	})
	if len(fields) != 1 || fields[0].Message != "unknown model ghost" {
		t.Fatalf("unknown model = %+v", fields)
	}

	withdrawn := catalogFake(map[string][][]any{qCatalogModel: {{false, "text"}}})
	fields, _ = withdrawn.Validate(context.Background(), 8, domain.AgentModelSelection{
		Text: &domain.ModelReference{ProviderID: "anthropic", ModelID: "retired"},
	})
	if len(fields) != 1 || fields[0].Message != "model retired is not active" {
		t.Fatalf("withdrawn model = %+v", fields)
	}
}

func TestValidateSkipsUnsetSlots(t *testing.T) {
	catalog := catalogFake(map[string][][]any{})
	fields, err := catalog.Validate(context.Background(), 8, domain.AgentModelSelection{})
	if err != nil || len(fields) != 0 {
		t.Fatalf("fields = %+v err = %v", fields, err)
	}
}
