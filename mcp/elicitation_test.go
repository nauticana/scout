package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/nauticana/scout/domain"
)

// confirmingBackend asks the user to confirm before completing, and records
// the answer it is called with.
type confirmingBackend struct {
	ask  domain.MCPElicitation
	call domain.MCPToolCall
}

func (backend *confirmingBackend) Catalog(context.Context) ([]domain.MCPToolDefinition, error) {
	return []domain.MCPToolDefinition{{Name: "publish", Description: "publish things"}}, nil
}

func (backend *confirmingBackend) ListTools(ctx context.Context, _ domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	return backend.Catalog(ctx)
}

func (backend *confirmingBackend) ExecuteTool(_ context.Context, call domain.MCPToolCall) (domain.MCPToolResult, error) {
	backend.call = call
	if answer, ok := call.Elicited["confirm"]; ok {
		return domain.MCPToolResult{Data: map[string]any{"action": string(answer.Action)}}, nil
	}
	return domain.MCPToolResult{Elicit: map[string]domain.MCPElicitation{"confirm": backend.ask}, State: "draft-9"}, nil
}

var confirmForm = domain.MCPElicitation{Message: "Publish?", Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)}

func modernParams(capabilities map[string]any, extra map[string]any) map[string]any {
	params := map[string]any{
		"name": "publish",
		"_meta": map[string]any{
			mcpgo.MetaKeyProtocolVersion:    mcpgo.ProtocolVersion20260728,
			mcpgo.MetaKeyClientCapabilities: capabilities,
		},
	}
	for key, value := range extra {
		params[key] = value
	}
	return params
}

func TestToolElicitsAndReceivesTheAnswer(t *testing.T) {
	backend := &confirmingBackend{ask: confirmForm}
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	if err := srv.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	capabilities := map[string]any{"elicitation": map[string]any{}}

	asked := call(t, srv, remoteContext("read"), "tools/call", modernParams(capabilities, nil))
	if !strings.Contains(asked, `"input_required"`) || !strings.Contains(asked, `"confirm"`) || !strings.Contains(asked, `"draft-9"`) {
		t.Fatalf("elicitation result = %s", asked)
	}

	answered := call(t, srv, remoteContext("read"), "tools/call", modernParams(capabilities, map[string]any{
		"inputResponses": map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"ok": true}}},
		"requestState":   "draft-9",
	}))
	if !strings.Contains(answered, `\"action\": \"accept\"`) {
		t.Fatalf("answered result = %s", answered)
	}
	got := backend.call.Elicited["confirm"]
	if got.Action != domain.MCPElicitationAccept || got.Content["ok"] != true || backend.call.State != "draft-9" {
		t.Fatalf("backend call = %+v", backend.call)
	}
}

func TestToolElicitationRequiresTheClientCapability(t *testing.T) {
	for name, test := range map[string]struct {
		ask          domain.MCPElicitation
		capabilities map[string]any
	}{
		"no elicitation": {confirmForm, map[string]any{}},
		"form only for url": {
			domain.MCPElicitation{Message: "Sign in", URL: "https://example.com/consent", ElicitationID: "e-1"},
			map[string]any{"elicitation": map[string]any{"form": map[string]any{}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
			if err := srv.RegisterToolBackend(context.Background(), &confirmingBackend{ask: test.ask}); err != nil {
				t.Fatalf("register: %v", err)
			}
			refused := call(t, srv, remoteContext("read"), "tools/call", modernParams(test.capabilities, nil))
			if !strings.Contains(refused, `"isError":true`) || !strings.Contains(refused, "capability unsupported") {
				t.Fatalf("result = %s", refused)
			}
		})
	}
}

func TestElicitationParamsValidation(t *testing.T) {
	caller := domain.MCPCaller{ElicitForm: true, ElicitURL: true}
	for name, elicitation := range map[string]domain.MCPElicitation{
		"no message":        {Schema: confirmForm.Schema},
		"schema and url":    {Message: "m", Schema: confirmForm.Schema, URL: "https://example.com"},
		"schema not object": {Message: "m", Schema: json.RawMessage(`[1]`)},
		"relative url":      {Message: "m", URL: "/consent", ElicitationID: "e"},
		"url without id":    {Message: "m", URL: "https://example.com/consent"},
		"javascript url":    {Message: "m", URL: "javascript:alert(1)", ElicitationID: "e"},
	} {
		if _, err := elicitationParams("id", elicitation, caller); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	params, err := elicitationParams("id", domain.MCPElicitation{Message: "m", URL: "https://example.com/consent", ElicitationID: "e"}, caller)
	if err != nil || params.Mode != mcpgo.ElicitationModeURL || params.Validate() != nil {
		t.Fatalf("url params = %+v, err = %v", params, err)
	}
}

func TestClientElicitationModes(t *testing.T) {
	modern := func(elicitation *mcpgo.ElicitationCapability) context.Context {
		return server.WithRequestProtocolInfo(context.Background(), &server.RequestProtocolInfo{
			Modern: true, ClientCapabilities: &mcpgo.ClientCapabilities{Elicitation: elicitation},
		})
	}
	for name, test := range map[string]struct {
		ctx       context.Context
		form, url bool
	}{
		"no session":   {context.Background(), false, false},
		"none":         {modern(nil), false, false},
		"empty":        {modern(&mcpgo.ElicitationCapability{}), true, false},
		"url only":     {modern(&mcpgo.ElicitationCapability{URL: &struct{}{}}), false, true},
		"form and url": {modern(&mcpgo.ElicitationCapability{Form: &struct{}{}, URL: &struct{}{}}), true, true},
	} {
		if form, url := clientElicitation(test.ctx); form != test.form || url != test.url {
			t.Errorf("%s: form, url = %v, %v; want %v, %v", name, form, url, test.form, test.url)
		}
	}
}

func TestToolRefusesMalformedElicitationAnswers(t *testing.T) {
	backend := &confirmingBackend{ask: confirmForm}
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	if err := srv.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	capabilities := map[string]any{"elicitation": map[string]any{}}
	for name, answer := range map[string]any{
		"unknown action":   map[string]any{"action": "approve"},
		"not an object":    "yes",
		"scalar content":   map[string]any{"action": "accept", "content": "yes"},
		"declined content": map[string]any{"action": "decline", "content": map[string]any{"ok": false}},
	} {
		refused := call(t, srv, remoteContext("read"), "tools/call", modernParams(capabilities, map[string]any{
			"inputResponses": map[string]any{"confirm": answer},
		}))
		if !strings.Contains(refused, `"isError":true`) || backend.call.Name != "" {
			t.Errorf("%s: result = %s, call = %+v", name, refused, backend.call)
		}
	}
}

type acceptingClient struct{ asked mcpgo.ElicitationParams }

func (client *acceptingClient) Elicit(_ context.Context, request mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	client.asked = request.Params
	return &mcpgo.ElicitationResult{ElicitationResponse: mcpgo.ElicitationResponse{
		Action: mcpgo.ElicitationResponseActionAccept, Content: map[string]any{"ok": true},
	}}, nil
}

// A client on an earlier protocol is asked through elicitation/create and the
// tool is called again with its answer.
func TestToolElicitsFromAnEarlierProtocolClient(t *testing.T) {
	backend := &confirmingBackend{ask: confirmForm}
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	if err := srv.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	client := &acceptingClient{}
	session := server.NewInProcessSessionWithHandlers("s-1", nil, client, nil)
	session.Initialize()
	session.SetClientCapabilities(mcpgo.ClientCapabilities{Elicitation: &mcpgo.ElicitationCapability{}})
	ctx := srv.MCPServer().WithContext(remoteContext("read"), session)

	answered := call(t, srv, ctx, "tools/call", map[string]any{"name": "publish"})
	if !strings.Contains(answered, `\"action\": \"accept\"`) || client.asked.Message != "Publish?" {
		t.Fatalf("result = %s, asked = %+v", answered, client.asked)
	}
	if backend.call.State != "draft-9" || backend.call.Elicited["confirm"].Content["ok"] != true {
		t.Fatalf("backend call = %+v", backend.call)
	}
}
