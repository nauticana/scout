package provider

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// OpenAI invokes Chat Completions for text and the Images API for image
// generation. Video generation is not wired.
type OpenAI struct {
	APIKey      string
	Temperature float64
}

var (
	_ contract.ModelProvider = (*OpenAI)(nil)
	_ contract.MediaProvider = (*OpenAI)(nil)
)

func (p *OpenAI) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if p.APIKey == "" {
		return domain.ModelResult{}, fmt.Errorf("%w: openai API key is not set", domain.ErrNotReady)
	}
	client := openai.NewClient(option.WithAPIKey(p.APIKey))
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:       selection.Model,
		Messages:    []openai.ChatCompletionMessageParamUnion{openai.UserMessage(string(request.Prompt))},
		Temperature: openai.Float(temperature(p.Temperature)),
		MaxTokens:   openai.Int(maxOutputTokens(request)),
	})
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("openai ChatCompletion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return domain.ModelResult{}, fmt.Errorf("openai: no choices returned")
	}
	text := resp.Choices[0].Message.Content
	if text == "" {
		return domain.ModelResult{}, fmt.Errorf("openai: empty response content")
	}
	return domain.ModelResult{
		Output:       []byte(text),
		FinishReason: resp.Choices[0].FinishReason,
		Usage: domain.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}, nil
}

func (p *OpenAI) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return singleFrameStream(ctx, p, selection, request)
}

func (p *OpenAI) GenerateImage(ctx context.Context, model string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("%w: openai API key is not set", domain.ErrNotReady)
	}
	if model == "" {
		return nil, fmt.Errorf("%w: no image model configured", domain.ErrValidation)
	}
	client := openai.NewClient(option.WithAPIKey(p.APIKey))
	resp, err := client.Images.Generate(ctx, openai.ImageGenerateParams{
		Model:          openai.ImageModel(model),
		Prompt:         mediaPrompt(request.Prompt, request.StyleHint),
		N:              openai.Int(int64(atLeastOne(request.Count))),
		Size:           openai.ImageGenerateParamsSize1024x1024,
		ResponseFormat: openai.ImageGenerateParamsResponseFormatB64JSON,
	})
	if err != nil {
		return nil, fmt.Errorf("openai Images.Generate: %w", err)
	}
	results := make([]domain.GeneratedMedia, 0, len(resp.Data))
	for _, image := range resp.Data {
		if image.B64JSON == "" {
			continue
		}
		data, decodeErr := base64.StdEncoding.DecodeString(image.B64JSON)
		if decodeErr != nil {
			return nil, fmt.Errorf("openai image base64 decode: %w", decodeErr)
		}
		results = append(results, domain.GeneratedMedia{Data: data, MimeType: "image/png"})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("openai: no images generated")
	}
	return results, nil
}

func (p *OpenAI) GenerateVideo(context.Context, string, domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	return nil, fmt.Errorf("video generation is not supported by the openai adapter")
}
