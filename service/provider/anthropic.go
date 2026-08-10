package provider

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Anthropic invokes the Messages API. Text only: image and video are served by
// providers that implement contract.MediaProvider.
type Anthropic struct {
	APIKey                string
	Temperature           float64
	TemperatureConfigured bool
}

var _ contract.ModelProvider = (*Anthropic)(nil)

func (p *Anthropic) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if p.APIKey == "" {
		return domain.ModelResult{}, fmt.Errorf("%w: anthropic API key is not set", domain.ErrNotReady)
	}
	client := anthropic.NewClient(option.WithAPIKey(p.APIKey))
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:       anthropic.Model(selection.Model),
		MaxTokens:   maxOutputTokens(request),
		Temperature: anthropic.Float(temperature(p.Temperature, p.TemperatureConfigured)),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(string(request.Prompt))),
		},
	})
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("anthropic Messages.New: %w", err)
	}
	text := ""
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if text == "" {
		return domain.ModelResult{}, fmt.Errorf("anthropic: empty response content")
	}
	return domain.ModelResult{
		Output:       []byte(text),
		FinishReason: string(resp.StopReason),
		Usage: domain.Usage{
			InputTokens:  int64(resp.Usage.InputTokens),
			OutputTokens: int64(resp.Usage.OutputTokens),
		},
	}, nil
}

func (p *Anthropic) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return singleFrameStream(ctx, p, selection, request)
}
