package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/openai/openai-go"
	"google.golang.org/genai"

	"github.com/nauticana/scout/domain"
)

func TestOpenAIEmbeddingsFollowTheirInputOrderAndCarryTheirShareOfTheUsage(t *testing.T) {
	var resp openai.CreateEmbeddingResponse
	if err := json.Unmarshal([]byte(`{"object":"list","model":"text-embedding-3-small","data":[
		{"object":"embedding","index":1,"embedding":[0.5,0.5]},
		{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
		"usage":{"prompt_tokens":9,"total_tokens":9}}`), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	inputs := [][]byte{[]byte("aa"), []byte("bbbb")}
	embeddings, err := openAIEmbeddings(&resp, inputs)
	if err != nil {
		t.Fatalf("openAIEmbeddings: %v", err)
	}
	if embeddings[0].Values[0] != 0.1 || embeddings[1].Values[0] != 0.5 {
		t.Fatalf("a vector must land on its own input, got %+v", embeddings)
	}
	if total := embeddings[0].Usage.InputTokens + embeddings[1].Usage.InputTokens; total != 9 {
		t.Fatalf("the attributed usage must sum to the call's own, got %d", total)
	}
	if embeddings[1].Usage.InputTokens <= embeddings[0].Usage.InputTokens {
		t.Fatalf("the longer input carries the larger share, got %+v", embeddings)
	}
	if _, err := openAIEmbeddings(&resp, inputs[:1]); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("a vector count that does not match the inputs = %v", err)
	}
}

func TestGoogleEmbeddingsRefuseAnEmptyVector(t *testing.T) {
	resp := &genai.EmbedContentResponse{Embeddings: []*genai.ContentEmbedding{{Values: []float32{0.3}}, {}}}
	if _, err := googleEmbeddings(resp, [][]byte{[]byte("a"), []byte("b")}); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("err = %v", err)
	}
	resp.Embeddings[1] = &genai.ContentEmbedding{Values: []float32{0.4}}
	embeddings, err := googleEmbeddings(resp, [][]byte{[]byte("a"), []byte("b")})
	if err != nil || len(embeddings) != 2 || embeddings[1].Values[0] != 0.4 {
		t.Fatalf("embeddings = %+v, %v", embeddings, err)
	}
}

func TestEmbeddingRequestsAreValidatedBeforeAnyVendorCall(t *testing.T) {
	for name, request := range map[string]domain.EmbeddingRequest{
		"no inputs":   {},
		"empty input": {Inputs: [][]byte{[]byte("a"), {}}},
		"negative":    {Inputs: [][]byte{[]byte("a")}, Dimensions: -1},
	} {
		if err := checkEmbeddingRequest(request); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestFactoryBuildsNoEmbedderForAVendorWithoutTheEndpoint(t *testing.T) {
	factory := NewFactory(nil, FactoryConfig{})
	_, err := factory.BuildEmbedder(context.Background(), domain.ModelReference{ProviderID: AnthropicProviderID, ModelID: "opus"})
	if !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
