package provider

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// OpenAI invokes Chat Completions for text and the Images API for image
// generation. Video generation is not wired.
type OpenAI struct {
	APIKey string
	// Temperature is sent only when set; nil leaves sampling to the model.
	Temperature *float64
}

var (
	_ contract.ModelProvider = (*OpenAI)(nil)
	_ contract.MediaProvider = (*OpenAI)(nil)
)

func (p *OpenAI) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if p.APIKey == "" {
		return domain.ModelResult{}, fmt.Errorf("%w: openai API key is not set", domain.ErrNotReady)
	}
	params, err := p.completionParams(selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	client := openai.NewClient(option.WithAPIKey(p.APIKey))
	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("openai ChatCompletion: %w", err)
	}
	return openAIResult(resp)
}

func (p *OpenAI) completionParams(selection domain.ModelSelection, request domain.ModelRequest) (openai.ChatCompletionNewParams, error) {
	if err := checkOutputMode(OpenAIProviderID, request.Output); err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	params := openai.ChatCompletionNewParams{
		Model:     selection.Model,
		MaxTokens: openai.Int(maxOutputTokens(request)),
	}
	if p.Temperature != nil {
		params.Temperature = openai.Float(*p.Temperature)
	}
	for _, message := range conversation(request) {
		params.Messages = append(params.Messages, openAIMessages(message)...)
	}
	for _, tool := range request.Tools {
		schema, err := schemaObject(tool.InputSchema)
		if err != nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("tool %q: %w", tool.Name, err)
		}
		function := shared.FunctionDefinitionParam{Name: tool.Name, Parameters: shared.FunctionParameters(schema)}
		if tool.Description != "" {
			function.Description = openai.String(tool.Description)
		}
		params.Tools = append(params.Tools, openai.ChatCompletionToolParam{Function: function})
	}
	if request.Output.Mode == domain.OutputModeJSONSchema {
		schema, err := schemaObject(request.Output.Schema)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name: schemaName(request.Output), Schema: schema, Strict: openai.Bool(strictCompatible(schema)),
			}},
		}
	}
	return params, nil
}

// openAIMessages maps one message; a tool message fans out because Chat
// Completions answers each call id in its own message.
func openAIMessages(message domain.ModelMessage) []openai.ChatCompletionMessageParamUnion {
	switch message.Role {
	case domain.ModelRoleAssistant:
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if len(message.Text) > 0 {
			assistant.Content.OfString = openai.String(string(message.Text))
		}
		for _, call := range message.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallParam{
				ID:       call.CallID,
				Function: openai.ChatCompletionMessageToolCallFunctionParam{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		return []openai.ChatCompletionMessageParamUnion{{OfAssistant: &assistant}}
	case domain.ModelRoleTool:
		messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(message.Observations))
		for _, observation := range message.Observations {
			messages = append(messages, openai.ToolMessage(string(observation.Output), observation.CallID))
		}
		return messages
	}
	return []openai.ChatCompletionMessageParamUnion{openai.UserMessage(string(message.Text))}
}

func openAIResult(resp *openai.ChatCompletion) (domain.ModelResult, error) {
	if len(resp.Choices) == 0 {
		return domain.ModelResult{}, fmt.Errorf("openai: no choices returned")
	}
	choice := resp.Choices[0]
	var calls []domain.ModelToolCall
	for _, call := range choice.Message.ToolCalls {
		calls = append(calls, domain.ModelToolCall{CallID: call.ID, Name: call.Function.Name, Arguments: []byte(call.Function.Arguments)})
	}
	if err := emptyResult(OpenAIProviderID, choice.Message.Content, calls); err != nil {
		return domain.ModelResult{}, err
	}
	return domain.ModelResult{
		Output:       []byte(choice.Message.Content),
		ToolCalls:    calls,
		FinishReason: finishReason(choice.FinishReason, calls),
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
