package provider

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/ssestream"
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
	return openAIResult(resp, request)
}

func (p *OpenAI) completionParams(selection domain.ModelSelection, request domain.ModelRequest) (openai.ChatCompletionNewParams, error) {
	if err := checkOutputMode(OpenAIProviderID, request.Output); err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	params := openai.ChatCompletionNewParams{
		Model:     selection.Model,
		MaxTokens: openai.Int(maxOutputTokens(request)),
	}
	if temperature := sampling(p.Temperature, request); temperature != nil {
		params.Temperature = openai.Float(*temperature)
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
	if request.Search != nil {
		if err := checkSearchBound(OpenAIProviderID, request.Search); err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		// An all-zero option object is omitted from the request body, which would
		// send the call ungrounded; "medium" is the vendor default.
		params.WebSearchOptions = openai.ChatCompletionNewParamsWebSearchOptions{SearchContextSize: "medium"}
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

func openAIResult(resp *openai.ChatCompletion, request domain.ModelRequest) (domain.ModelResult, error) {
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
	usage := domain.Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	if request.Search != nil {
		// Chat Completions bills the grounded call, not each search it ran.
		usage.SearchQueries = 1
	}
	return domain.ModelResult{
		Output:       []byte(choice.Message.Content),
		ToolCalls:    calls,
		Citations:    openAICitations(choice.Message),
		FinishReason: finishReason(choice.FinishReason, calls),
		Usage:        usage,
	}, nil
}

// openAICitations maps URL annotations; the cited span of the answer is the
// only snippet Chat Completions reports.
func openAICitations(message openai.ChatCompletionMessage) []domain.Citation {
	var sources citations
	answer := []rune(message.Content)
	for _, annotation := range message.Annotations {
		if annotation.Type != "url_citation" {
			continue
		}
		cited := annotation.URLCitation
		snippet := ""
		if cited.StartIndex >= 0 && cited.EndIndex > cited.StartIndex && cited.EndIndex <= int64(len(answer)) {
			snippet = string(answer[cited.StartIndex:cited.EndIndex])
		}
		sources.add(cited.URL, cited.Title, snippet)
	}
	return sources.list
}

func (p *OpenAI) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("%w: openai API key is not set", domain.ErrNotReady)
	}
	if request.Search != nil {
		// Chat Completions reports URL annotations only on the complete message,
		// so a streamed grounded answer would arrive without its sources.
		return singleFrameStream(ctx, p, selection, request)
	}
	params, err := p.completionParams(selection, request)
	if err != nil {
		return nil, err
	}
	// Without this the streamed call reports no tokens and would settle as free.
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	client := openai.NewClient(option.WithAPIKey(p.APIKey))
	return openAIStream(client.Chat.Completions.NewStreaming(ctx, params), request), nil
}

// openAIStream emits each content delta as it arrives and accumulates the same
// completion a unary call returns, so both paths report the same terminal frame.
func openAIStream(stream *ssestream.Stream[openai.ChatCompletionChunk], request domain.ModelRequest) contract.ModelStream {
	accumulated := openai.ChatCompletionAccumulator{}
	terminal := false
	return &eventStream{
		shutdown: stream.Close,
		advance: func() (domain.ModelChunk, bool, error) {
			for stream.Next() {
				chunk := stream.Current()
				if !accumulated.AddChunk(chunk) {
					return domain.ModelChunk{}, false, fmt.Errorf("openai stream: frames belong to different completions")
				}
				if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
					return domain.ModelChunk{Payload: []byte(chunk.Choices[0].Delta.Content)}, true, nil
				}
			}
			if err := stream.Err(); err != nil {
				return domain.ModelChunk{}, false, fmt.Errorf("openai stream: %w", err)
			}
			if terminal {
				return domain.ModelChunk{}, false, nil
			}
			terminal = true
			result, err := openAIResult(&accumulated.ChatCompletion, request)
			if err != nil {
				return domain.ModelChunk{}, false, err
			}
			return terminalFrame(result), true, nil
		},
	}
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
