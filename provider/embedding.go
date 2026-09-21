package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"google.golang.org/genai"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

var (
	_ contract.EmbeddingProvider = (*OpenAI)(nil)
	_ contract.EmbeddingProvider = (*Google)(nil)
)

// checkEmbeddingRequest validates what every vendor requires of a batch.
func checkEmbeddingRequest(request domain.EmbeddingRequest) error {
	if len(request.Inputs) == 0 {
		return fmt.Errorf("%w: at least one input is required", domain.ErrValidation)
	}
	if request.Dimensions < 0 {
		return fmt.Errorf("%w: dimensions cannot be negative", domain.ErrValidation)
	}
	for index, input := range request.Inputs {
		if len(input) == 0 {
			return fmt.Errorf("%w: input %d is empty", domain.ErrValidation, index)
		}
	}
	return nil
}

// attributeEmbeddingUsage spreads one batch's input tokens over its inputs in
// proportion to their length, so a caller that sums per-item usage reproduces
// the call's total exactly. Vendors report the batch, never the item.
func attributeEmbeddingUsage(inputs [][]byte, inputTokens int64) []domain.Usage {
	usage := make([]domain.Usage, len(inputs))
	var totalBytes int64
	for _, input := range inputs {
		totalBytes += int64(len(input))
	}
	if inputTokens <= 0 || totalBytes <= 0 {
		return usage
	}
	var assigned int64
	for index, input := range inputs {
		share := inputTokens * int64(len(input)) / totalBytes
		usage[index].InputTokens = share
		assigned += share
	}
	usage[0].InputTokens += inputTokens - assigned
	return usage
}

// Embed calls the OpenAI embeddings endpoint for the whole batch.
func (p *OpenAI) Embed(ctx context.Context, selection domain.ModelSelection, request domain.EmbeddingRequest) ([]domain.Embedding, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("%w: openai API key is not set", domain.ErrNotReady)
	}
	if err := checkEmbeddingRequest(request); err != nil {
		return nil, err
	}
	inputs := make([]string, 0, len(request.Inputs))
	for _, input := range request.Inputs {
		inputs = append(inputs, string(input))
	}
	params := openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(selection.Model),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
	}
	if request.Dimensions > 0 {
		params.Dimensions = openai.Int(int64(request.Dimensions))
	}
	client := openai.NewClient(option.WithAPIKey(p.APIKey))
	resp, err := client.Embeddings.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai Embeddings: %w", err)
	}
	return openAIEmbeddings(resp, request.Inputs)
}

// openAIEmbeddings places each vector at its own input's position.
func openAIEmbeddings(resp *openai.CreateEmbeddingResponse, inputs [][]byte) ([]domain.Embedding, error) {
	if len(resp.Data) != len(inputs) {
		return nil, fmt.Errorf("%w: openai returned %d vectors for %d inputs", domain.ErrInvalidModelOutput, len(resp.Data), len(inputs))
	}
	usage := attributeEmbeddingUsage(inputs, resp.Usage.PromptTokens)
	embeddings := make([]domain.Embedding, len(resp.Data))
	for _, vector := range resp.Data {
		if vector.Index < 0 || vector.Index >= int64(len(embeddings)) {
			return nil, fmt.Errorf("%w: openai returned vector index %d", domain.ErrInvalidModelOutput, vector.Index)
		}
		values := make([]float32, len(vector.Embedding))
		for position, value := range vector.Embedding {
			values[position] = float32(value)
		}
		embeddings[vector.Index] = domain.Embedding{Values: values, Usage: usage[vector.Index]}
	}
	for index, embedding := range embeddings {
		if len(embedding.Values) == 0 {
			return nil, fmt.Errorf("%w: openai returned no vector for input %d", domain.ErrInvalidModelOutput, index)
		}
	}
	return embeddings, nil
}

// Embed calls Gemini's embedding endpoint for the whole batch. The vendor
// reports no token count, so the vectors carry none and the caller's own
// estimate settles the call.
func (p *Google) Embed(ctx context.Context, selection domain.ModelSelection, request domain.EmbeddingRequest) ([]domain.Embedding, error) {
	if err := checkEmbeddingRequest(request); err != nil {
		return nil, err
	}
	client, err := p.newClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	contents := make([]*genai.Content, 0, len(request.Inputs))
	for _, input := range request.Inputs {
		contents = append(contents, genai.NewContentFromText(string(input), genai.RoleUser))
	}
	var config *genai.EmbedContentConfig
	if request.Dimensions > 0 {
		config = &genai.EmbedContentConfig{OutputDimensionality: genai.Ptr(int32(request.Dimensions))}
	}
	resp, err := client.Models.EmbedContent(ctx, selection.Model, contents, config)
	if err != nil {
		return nil, fmt.Errorf("genai EmbedContent: %w", err)
	}
	return googleEmbeddings(resp, request.Inputs)
}

// googleEmbeddings keeps the vendor's own input order.
func googleEmbeddings(resp *genai.EmbedContentResponse, inputs [][]byte) ([]domain.Embedding, error) {
	if len(resp.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("%w: gemini returned %d vectors for %d inputs", domain.ErrInvalidModelOutput, len(resp.Embeddings), len(inputs))
	}
	embeddings := make([]domain.Embedding, len(resp.Embeddings))
	for index, vector := range resp.Embeddings {
		if vector == nil || len(vector.Values) == 0 {
			return nil, fmt.Errorf("%w: gemini returned no vector for input %d", domain.ErrInvalidModelOutput, index)
		}
		embeddings[index] = domain.Embedding{Values: append([]float32(nil), vector.Values...)}
	}
	return embeddings, nil
}

// BuildEmbedder constructs the embedding adapter for one model reference.
// Anthropic publishes no embedding endpoint, so it has no adapter.
func (factory *Factory) BuildEmbedder(ctx context.Context, reference domain.ModelReference) (contract.EmbeddingProvider, error) {
	if factory == nil {
		return nil, fmt.Errorf("provider factory is required")
	}
	reference.ProviderID = strings.TrimSpace(reference.ProviderID)
	reference.ModelID = strings.TrimSpace(reference.ModelID)
	if reference.ProviderID == "" || reference.ModelID == "" {
		return nil, fmt.Errorf("%w: provider and model are required", domain.ErrValidation)
	}
	switch reference.ProviderID {
	case OpenAIProviderID:
		apiKey, err := factory.apiKey(ctx, OpenAIProviderID)
		if err != nil {
			return nil, err
		}
		return &OpenAI{APIKey: apiKey}, nil
	case GoogleProviderID:
		return factory.googleAdapter(ctx, nil)
	default:
		return nil, fmt.Errorf("%w: %s publishes no embedding endpoint", domain.ErrCapabilityUnsupported, reference.ProviderID)
	}
}

var _ contract.EmbeddingProviderFactory = (*Factory)(nil)
