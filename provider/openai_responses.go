package provider

import (
	"context"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"

	"github.com/nauticana/scout/domain"
)

// respond answers a grounded call through the Responses API with the vendor's
// web_search tool; citations arrive as url_citation annotations of the answer.
func (p *OpenAI) respond(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	client, err := p.client()
	if err != nil {
		return domain.ModelResult{}, err
	}
	params, err := p.responseParams(selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	resp, err := client.Responses.New(ctx, params)
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("%s Responses: %w", p.id(), err)
	}
	return responseResult(p.id(), resp)
}

func (p *OpenAI) responseParams(selection domain.ModelSelection, request domain.ModelRequest) (responses.ResponseNewParams, error) {
	if err := checkOutputMode(p.id(), request.Output); err != nil {
		return responses.ResponseNewParams{}, err
	}
	if err := checkSearchBound(p.id(), request.Search); err != nil {
		return responses.ResponseNewParams{}, err
	}
	if location := request.Search.Location; location != nil {
		if err := searchLocationSupport(p.id(), *location); err != nil {
			return responses.ResponseNewParams{}, err
		}
	}
	params := responses.ResponseNewParams{
		Model:           shared.ResponsesModel(selection.Model),
		MaxOutputTokens: openai.Int(maxOutputTokens(request)),
		Tools:           []responses.ToolUnionParam{{OfWebSearchPreview: &responses.WebSearchToolParam{Type: "web_search"}}},
	}
	if temperature := sampling(p.Temperature, request); temperature != nil {
		params.Temperature = openai.Float(*temperature)
	}
	if len(request.Instructions) > 0 {
		params.Instructions = openai.String(string(request.Instructions))
	}
	for _, message := range conversation(request) {
		params.Input.OfInputItemList = append(params.Input.OfInputItemList, responseInputItems(message)...)
	}
	for _, tool := range request.Tools {
		schema, err := schemaObject(tool.InputSchema)
		if err != nil {
			return responses.ResponseNewParams{}, fmt.Errorf("tool %q: %w", tool.Name, err)
		}
		function := responses.ToolParamOfFunction(tool.Name, schema, false)
		if tool.Description != "" {
			function.OfFunction.Description = openai.String(tool.Description)
		}
		params.Tools = append(params.Tools, function)
	}
	if request.Output.Mode == domain.OutputModeJSONSchema {
		schema, err := schemaObject(request.Output.Schema)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Text.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name: schemaName(request.Output), Schema: schema, Strict: openai.Bool(strictCompatible(schema)),
		}
	}
	return params, nil
}

// responseInputItems maps one message onto Responses input items; tool calls
// and their outputs are items of their own.
func responseInputItems(message domain.ModelMessage) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam
	switch message.Role {
	case domain.ModelRoleAssistant:
		if len(message.Text) > 0 {
			items = append(items, responses.ResponseInputItemParamOfMessage(string(message.Text), responses.EasyInputMessageRoleAssistant))
		}
		for _, call := range message.ToolCalls {
			items = append(items, responses.ResponseInputItemParamOfFunctionCall(string(call.Arguments), call.CallID, call.Name))
		}
	case domain.ModelRoleTool:
		for _, observation := range message.Observations {
			items = append(items, responses.ResponseInputItemParamOfFunctionCallOutput(observation.CallID, string(observation.Output)))
		}
	default:
		items = append(items, responses.ResponseInputItemParamOfMessage(string(message.Text), responses.EasyInputMessageRoleUser))
	}
	return items
}

func responseResult(provider string, resp *responses.Response) (domain.ModelResult, error) {
	text := resp.OutputText()
	answer := []rune(text)
	var calls []domain.ModelToolCall
	var sources citations
	var searches int64
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call":
			calls = append(calls, domain.ModelToolCall{CallID: item.CallID, Name: item.Name, Arguments: []byte(item.Arguments)})
		case "web_search_call":
			searches++
		case "message":
			for _, part := range item.Content {
				for _, annotation := range part.Annotations {
					if annotation.Type != "url_citation" {
						continue
					}
					snippet := ""
					if annotation.StartIndex >= 0 && annotation.EndIndex > annotation.StartIndex && annotation.EndIndex <= int64(len(answer)) {
						snippet = string(answer[annotation.StartIndex:annotation.EndIndex])
					}
					sources.add(annotation.URL, urlHost(annotation.URL), annotation.Title, snippet)
				}
			}
		}
	}
	if err := emptyResult(provider, text, calls); err != nil {
		return domain.ModelResult{}, err
	}
	if err := compatibleCitations(resp.RawJSON(), &sources); err != nil {
		return domain.ModelResult{}, fmt.Errorf("%w: %s sources: %w", domain.ErrInvalidModelOutput, provider, err)
	}
	native := "stop"
	if resp.Status == responses.ResponseStatusIncomplete {
		native = resp.IncompleteDetails.Reason
	}
	return domain.ModelResult{
		Output:       []byte(text),
		ToolCalls:    calls,
		Citations:    sources.list,
		FinishReason: finishReason(native, calls),
		Usage:        domain.Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, SearchQueries: searches},
	}, nil
}
