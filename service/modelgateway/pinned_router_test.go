package modelgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestPinnedModelRouterRoutesOnlyToTheGrantedPinnedModel(t *testing.T) {
	router := &PinnedModelRouter{Catalog: &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Generation: 4, Candidates: []domain.ModelCandidate{
		{Provider: "anthropic", Model: "sonnet", RouteID: "anthropic/sonnet", Capabilities: []string{domain.CapabilityTools}},
		{Provider: "openai", Model: "gpt", RouteID: "openai/gpt", Capabilities: []string{domain.CapabilityTools, domain.CapabilityStructuredOutput}},
	}}}}
	request := validModelRequest()
	request.Tools = []domain.ModelTool{lookupTool}
	request.Model = domain.ModelReference{ProviderID: "anthropic", ModelID: "sonnet"}
	selection, err := router.Select(context.Background(), request)
	if err != nil || selection.RouteID != "anthropic/sonnet" || selection.RoutingGeneration != 4 {
		t.Fatalf("selection = %+v, %v", selection, err)
	}
	request.Output = domain.OutputConstraint{Mode: domain.OutputModeJSONSchema, Schema: []byte(`{"type":"object"}`)}
	if _, err := router.Select(context.Background(), request); !errors.Is(err, domain.ErrNoRoute) {
		t.Fatalf("the pinned model lacks structured output, so there is no route; got %v", err)
	}
	request.Model = domain.ModelReference{ProviderID: "google", ModelID: "gemini"}
	if _, err := router.Select(context.Background(), request); !errors.Is(err, domain.ErrNoRoute) {
		t.Fatalf("an ungranted model must not route, got %v", err)
	}
	request.Model = domain.ModelReference{}
	if _, err := router.Select(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation without a pinned model, got %v", err)
	}
}
