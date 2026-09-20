package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Anthropic invokes the Messages API with tool use and native structured
// output. Text only: image and video are served by providers that implement
// contract.MediaProvider.
type Anthropic struct {
	APIKey string
	// Temperature is sent only when set; nil leaves sampling to the model, which is
	// the only form models without sampling support accept.
	Temperature *float64
}

var _ contract.ModelProvider = (*Anthropic)(nil)

func (p *Anthropic) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if p.APIKey == "" {
		return domain.ModelResult{}, fmt.Errorf("%w: anthropic API key is not set", domain.ErrNotReady)
	}
	params, err := p.messageParams(selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	client := anthropic.NewClient(option.WithAPIKey(p.APIKey))
	resp, err := client.Messages.New(ctx, params)
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("anthropic Messages.New: %w", err)
	}
	return anthropicResult(resp)
}

func (p *Anthropic) messageParams(selection domain.ModelSelection, request domain.ModelRequest) (anthropic.MessageNewParams, error) {
	if err := checkOutputMode(AnthropicProviderID, request.Output); err != nil {
		return anthropic.MessageNewParams{}, err
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(selection.Model),
		MaxTokens: maxOutputTokens(request),
	}
	if p.Temperature != nil {
		params.Temperature = anthropic.Float(*p.Temperature)
	}
	for _, message := range conversation(request) {
		params.Messages = append(params.Messages, anthropicMessage(message))
	}
	for _, tool := range request.Tools {
		schema, err := schemaObject(tool.InputSchema)
		if err != nil {
			return anthropic.MessageNewParams{}, fmt.Errorf("tool %q: %w", tool.Name, err)
		}
		input := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
		for key, value := range schema {
			switch key {
			case "type":
			case "properties":
				input.Properties = value
			default:
				input.ExtraFields[key] = value
			}
		}
		param := anthropic.ToolParam{Name: tool.Name, InputSchema: input}
		if tool.Description != "" {
			param.Description = anthropic.String(tool.Description)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &param})
	}
	if request.Output.Mode == domain.OutputModeJSONSchema {
		schema, err := schemaObject(request.Output.Schema)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.OutputConfig = anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{
			Schema: projectSchema(schema, anthropicUnsupportedKeywords),
		}}
	}
	return params, nil
}

func anthropicMessage(message domain.ModelMessage) anthropic.MessageParam {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(message.ToolCalls)+len(message.Observations))
	if len(message.Text) > 0 {
		blocks = append(blocks, anthropic.NewTextBlock(string(message.Text)))
	}
	for _, call := range message.ToolCalls {
		blocks = append(blocks, anthropic.NewToolUseBlock(call.CallID, json.RawMessage(call.Arguments), call.Name))
	}
	for _, observation := range message.Observations {
		blocks = append(blocks, anthropic.NewToolResultBlock(observation.CallID, string(observation.Output), observation.IsError))
	}
	if message.Role == domain.ModelRoleAssistant {
		return anthropic.NewAssistantMessage(blocks...)
	}
	return anthropic.NewUserMessage(blocks...)
}

func anthropicResult(resp *anthropic.Message) (domain.ModelResult, error) {
	text := ""
	var calls []domain.ModelToolCall
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "tool_use":
			calls = append(calls, domain.ModelToolCall{CallID: block.ID, Name: block.Name, Arguments: []byte(block.Input)})
		}
	}
	if err := emptyResult(AnthropicProviderID, text, calls); err != nil {
		return domain.ModelResult{}, err
	}
	return domain.ModelResult{
		Output:       []byte(text),
		ToolCalls:    calls,
		FinishReason: finishReason(string(resp.StopReason), calls),
		Usage: domain.Usage{
			InputTokens:  int64(resp.Usage.InputTokens),
			OutputTokens: int64(resp.Usage.OutputTokens),
		},
	}, nil
}

func (p *Anthropic) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return singleFrameStream(ctx, p, selection, request)
}
