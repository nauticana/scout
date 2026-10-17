package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go"

	"github.com/nauticana/scout/domain"
)

func TestFactoryBuildsCompatibleVendorsWithoutMedia(t *testing.T) {
	secrets := &secretProviderStub{values: map[string]string{"perplexity_api_key": "p", "xai_api_key": "x"}}
	for providerID, baseURL := range map[string]string{PerplexityProviderID: "https://api.perplexity.ai", XAIProviderID: "https://api.x.ai/v1"} {
		model, media, err := NewFactory(secrets, FactoryConfig{}).Build(context.Background(), domain.ModelReference{ProviderID: providerID, ModelID: "m"})
		if err != nil {
			t.Fatalf("%s: %v", providerID, err)
		}
		adapter, ok := model.(*OpenAI)
		if !ok || media != nil || adapter.ProviderID != providerID || adapter.BaseURL != baseURL || adapter.APIKey == "" {
			t.Fatalf("%s adapters = (%+v, %v)", providerID, model, media)
		}
	}
	if compatibleVendors[PerplexityProviderID].APIKey != "" {
		t.Fatal("building a vendor must not write its key into the shared table")
	}
}

// Perplexity returns its sources beside the message; they become citations in
// the order its [n] markers number them, with search_results' titles and snippets.
func TestPerplexityAnswersWithItsSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"sonar","created":1,
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"a ranks first [1][2]"}}],
			"citations":["https://a.example/p","https://b.example/q"],
			"search_results":[{"title":"B","url":"https://b.example/q","snippet":"b is second"},{"title":"A","url":"https://a.example/p","snippet":"a is first"}],
			"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`))
	}))
	defer server.Close()
	adapter := &OpenAI{APIKey: "key", ProviderID: PerplexityProviderID, BaseURL: server.URL}
	result, err := adapter.Generate(context.Background(), domain.ModelSelection{Model: "sonar"}, groundedRequest(0))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	requireCitation(t, result.Citations, 0, "https://a.example/p", "a.example", "A", "a is first")
	requireCitation(t, result.Citations, 1, "https://b.example/q", "b.example", "B", "b is second")
	if len(result.Citations) != 2 || result.Usage.SearchQueries != 1 || result.Usage.OutputTokens != 7 {
		t.Fatalf("citations = %+v usage = %+v", result.Citations, result.Usage)
	}
}

func TestCompatibleVendorsRefuseGroundingTheyCannotHonour(t *testing.T) {
	xai := compatibleVendors[XAIProviderID]
	for _, adapter := range []*OpenAI{&xai, {ProviderID: XAIProviderID}} {
		if _, err := adapter.completionParams(domain.ModelSelection{Model: "grok"}, groundedRequest(0)); !errors.Is(err, domain.ErrCapabilityUnsupported) {
			t.Fatalf("xai grounded call = %v, want refusal", err)
		}
		if _, err := adapter.completionParams(domain.ModelSelection{Model: "grok"}, domain.ModelRequest{Prompt: []byte("hi")}); err != nil {
			t.Fatalf("xai ungrounded call: %v", err)
		}
	}
	if SupportsSearchLocation(PerplexityProviderID, domain.SearchLocation{Country: "US"}) {
		t.Fatal("perplexity takes no OpenAI-shaped location")
	}
}

func TestListModelsReadsTheVendorCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.Error(w, r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-4","object":"model","created":1,"owned_by":"xai"},{"id":"grok-3","object":"model","created":1,"owned_by":"xai"}]}`))
	}))
	defer server.Close()
	ids, err := (&OpenAI{APIKey: "key", ProviderID: XAIProviderID, BaseURL: server.URL}).listModels(context.Background())
	if err != nil || len(ids) != 2 || ids[0] != "grok-4" || ids[1] != "grok-3" {
		t.Fatalf("ids = %v, %v", ids, err)
	}

	factory := NewFactory(&secretProviderStub{values: map[string]string{"perplexity_api_key": "p"}}, FactoryConfig{})
	for name, test := range map[string]struct {
		providerID string
		want       error
	}{
		"blank":           {" ", domain.ErrValidation},
		"unknown":         {"other", domain.ErrNotFound},
		"no model list":   {PerplexityProviderID, domain.ErrCapabilityUnsupported},
		"no credential":   {AnthropicProviderID, domain.ErrNotReady},
		"no vertex scope": {GoogleProviderID, domain.ErrValidation},
	} {
		if _, err := factory.ListModels(context.Background(), test.providerID); !errors.Is(err, test.want) {
			t.Fatalf("%s: %v, want %v", name, err, test.want)
		}
	}
}

func TestMalformedVendorSourcesAreInvalidOutput(t *testing.T) {
	var completion openai.ChatCompletion
	if err := json.Unmarshal([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],
		"citations":[{"url":"https://a.example/p"}]}`), &completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := openAIResult(PerplexityProviderID, &completion, groundedRequest(0)); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("malformed sources = %v", err)
	}
	if result, err := openAIResult(OpenAIProviderID, &completion, groundedRequest(0)); err != nil || len(result.Citations) != 0 {
		t.Fatalf("openai reads only its own annotations: %+v, %v", result.Citations, err)
	}
}
