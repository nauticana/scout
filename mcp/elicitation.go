package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/nauticana/scout/domain"
)

// clientElicitation reports the elicitation modes the client declared: per
// request from protocol 2026-07-28, otherwise at initialization. An empty
// elicitation capability means form mode.
func clientElicitation(ctx context.Context) (form, url bool) {
	var capabilities *mcpgo.ClientCapabilities
	if info := server.RequestProtocolInfoFromContext(ctx); info != nil && info.Modern {
		capabilities = info.ClientCapabilities
	} else if session, ok := server.ClientSessionFromContext(ctx).(server.SessionWithClientInfo); ok {
		declared := session.GetClientCapabilities()
		capabilities = &declared
	}
	if capabilities == nil || capabilities.Elicitation == nil {
		return false, false
	}
	elicitation := capabilities.Elicitation
	return elicitation.Form != nil || elicitation.URL == nil, elicitation.URL != nil
}

// elicitationResult asks the client for the result's elicitations. mcp-go
// returns the request to a 2026-07-28 client and performs elicitation/create
// for an earlier one, calling the tool again with the answers either way.
func elicitationResult(result domain.MCPToolResult, caller domain.MCPCaller) (*mcpgo.CallToolResult, error) {
	builder := server.NewInputRequestBuilder(result.State)
	for id, elicitation := range result.Elicit {
		params, err := elicitationParams(id, elicitation, caller)
		if err != nil {
			return nil, err
		}
		builder.Elicit(id, params)
	}
	return builder.ToolResult(), nil
}

func elicitationParams(id string, elicitation domain.MCPElicitation, caller domain.MCPCaller) (mcpgo.ElicitationParams, error) {
	if id == "" || elicitation.Message == "" {
		return mcpgo.ElicitationParams{}, fmt.Errorf("%w: elicitation %q needs an id and a message", domain.ErrValidation, id)
	}
	if len(elicitation.Schema) > 0 {
		if elicitation.URL != "" {
			return mcpgo.ElicitationParams{}, fmt.Errorf("%w: elicitation %q sets both a schema and a url", domain.ErrValidation, id)
		}
		if !caller.ElicitForm {
			return mcpgo.ElicitationParams{}, fmt.Errorf("%w: client does not accept form elicitation", domain.ErrCapabilityUnsupported)
		}
		var schema map[string]any
		if err := json.Unmarshal(elicitation.Schema, &schema); err != nil || schema == nil {
			return mcpgo.ElicitationParams{}, fmt.Errorf("%w: elicitation %q schema is not a JSON object", domain.ErrValidation, id)
		}
		return mcpgo.ElicitationParams{Mode: mcpgo.ElicitationModeForm, Message: elicitation.Message, RequestedSchema: schema}, nil
	}
	target, err := url.Parse(elicitation.URL)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" || elicitation.ElicitationID == "" {
		return mcpgo.ElicitationParams{}, fmt.Errorf("%w: elicitation %q needs a schema, or an absolute http(s) url and an elicitation id", domain.ErrValidation, id)
	}
	if !caller.ElicitURL {
		return mcpgo.ElicitationParams{}, fmt.Errorf("%w: client does not accept url elicitation", domain.ErrCapabilityUnsupported)
	}
	return mcpgo.ElicitationParams{
		Mode: mcpgo.ElicitationModeURL, Message: elicitation.Message,
		URL: elicitation.URL, ElicitationID: elicitation.ElicitationID,
	}, nil
}

// elicitedFrom decodes the client's elicitation answers and refuses any that
// is not one.
func elicitedFrom(responses mcpgo.InputResponses) (map[string]domain.MCPElicitationResult, error) {
	if len(responses) == 0 {
		return nil, nil
	}
	elicited := make(map[string]domain.MCPElicitationResult, len(responses))
	for id := range responses {
		response := server.ElicitationResponse(responses, id)
		if response == nil {
			return nil, fmt.Errorf("%w: input response %q is not an elicitation result", domain.ErrValidation, id)
		}
		action := domain.MCPElicitationAction(response.Action)
		switch action {
		case domain.MCPElicitationAccept, domain.MCPElicitationDecline, domain.MCPElicitationCancel:
		default:
			return nil, fmt.Errorf("%w: input response %q has unknown action %q", domain.ErrValidation, id, response.Action)
		}
		content, ok := response.Content.(map[string]any)
		if response.Content != nil && !ok {
			return nil, fmt.Errorf("%w: input response %q content is not a JSON object", domain.ErrValidation, id)
		}
		if action != domain.MCPElicitationAccept && response.Content != nil {
			return nil, fmt.Errorf("%w: input response %q includes content for action %q", domain.ErrValidation, id, action)
		}
		elicited[id] = domain.MCPElicitationResult{Action: action, Content: content}
	}
	return elicited, nil
}
