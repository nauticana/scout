package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"

	"github.com/nauticana/scout/domain"
)

// toolRequest is a second model turn: two parallel calls were proposed and both
// observations are being returned, under a constrained terminal answer.
func toolRequest() domain.ModelRequest {
	calls := []domain.ModelToolCall{
		{CallID: "call-a", Name: "lookup", Arguments: []byte(`{"q":"x"}`)},
		{CallID: "call-b", Name: "lookup", Arguments: []byte(`{"q":"y"}`)},
	}
	return domain.ModelRequest{
		Prompt: []byte("find x and y"), MaxOutputTokens: 64,
		Tools: []domain.ModelTool{{
			Name: "lookup", Description: "look a term up", ToolID: "lookup", ToolVersion: "1",
			InputSchema: []byte(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`),
		}},
		Messages: []domain.ModelMessage{
			{Role: domain.ModelRoleAssistant, Text: []byte("checking"), ToolCalls: calls},
			{Role: domain.ModelRoleTool, Observations: []domain.ModelToolObservation{
				{CallID: "call-a", Name: "lookup", Output: []byte(`{"hits":1}`)},
				{CallID: "call-b", Name: "lookup", Output: []byte(`not found`), IsError: true},
			}},
		},
		Output: domain.OutputConstraint{Mode: domain.OutputModeJSONSchema, SchemaName: "answer", Schema: []byte(`{"type":"object","required":["answer"]}`)},
	}
}

func encoded(t *testing.T, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	return string(payload)
}

func requireAll(t *testing.T, payload string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(payload, fragment) {
			t.Fatalf("params lack %s\n%s", fragment, payload)
		}
	}
}

func requireParallelCalls(t *testing.T, result domain.ModelResult, err error, ids ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.FinishReason != domain.FinishReasonToolCalls || len(result.ToolCalls) != len(ids) {
		t.Fatalf("result = %+v", result)
	}
	for i, id := range ids {
		call := result.ToolCalls[i]
		if call.CallID != id || call.Name != "lookup" || !json.Valid(call.Arguments) {
			t.Fatalf("call[%d] = %+v, want id %q", i, call, id)
		}
	}
}

func TestAnthropicRoundTripsParallelToolCallsAndConstrainsOutput(t *testing.T) {
	params, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, toolRequest())
	if err != nil {
		t.Fatalf("messageParams: %v", err)
	}
	requireAll(t, encoded(t, params),
		`"input_schema":{`, `"required":["q"]`, `"type":"tool_use"`, `"id":"call-a"`, `"id":"call-b"`,
		`"tool_use_id":"call-b"`, `"is_error":true`, `"output_config":{"format":{`, `"type":"json_schema"`)

	var message anthropic.Message
	if err := json.Unmarshal([]byte(`{"content":[
		{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}},
		{"type":"tool_use","id":"toolu_2","name":"lookup","input":{"q":"y"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`), &message); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	result, err := anthropicResult(&message)
	requireParallelCalls(t, result, err, "toolu_1", "toolu_2")
	if result.Usage.InputTokens != 5 || result.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestOpenAIRoundTripsParallelToolCallsAndConstrainsOutput(t *testing.T) {
	params, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, toolRequest())
	if err != nil {
		t.Fatalf("completionParams: %v", err)
	}
	requireAll(t, encoded(t, params),
		`"tools":[{"function":{`, `"tool_calls":[{"id":"call-a"`, `"tool_call_id":"call-a"`, `"tool_call_id":"call-b"`,
		`"response_format":{"json_schema":{`, `"name":"answer"`, `"strict":false`)

	var completion openai.ChatCompletion
	if err := json.Unmarshal([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},
		{"id":"c2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"y\"}"}}]}}],
		"usage":{"prompt_tokens":5,"completion_tokens":7}}`), &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	result, err := openAIResult(&completion)
	requireParallelCalls(t, result, err, "c1", "c2")
}

func TestGoogleRoundTripsParallelToolCallsAndConstrainsOutput(t *testing.T) {
	contents, config, err := (&Google{}).contentParams(toolRequest())
	if err != nil {
		t.Fatalf("contentParams: %v", err)
	}
	requireAll(t, encoded(t, contents), `"functionCall":{"id":"call-a"`, `"functionResponse":{`, `"id":"call-b"`, `"error":"not found"`, `"output":{"hits":1}`)
	requireAll(t, encoded(t, config), `"parametersJsonSchema":{`, `"responseMimeType":"application/json"`, `"responseJsonSchema":{`)

	response := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		FinishReason: genai.FinishReasonStop,
		Content: &genai.Content{Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "g1", Name: "lookup", Args: map[string]any{"q": "x"}}},
			{FunctionCall: &genai.FunctionCall{Name: "lookup", Args: map[string]any{"q": "y"}}},
		}},
	}}}
	result, err := googleResult(response, toolRequest())
	requireParallelCalls(t, result, err, "g1", "call_2_2")
}

func TestGoogleSynthesizesCallIDsUniqueAcrossConversationTurns(t *testing.T) {
	response := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{Name: "lookup", Args: map[string]any{"q": "x"}}},
	}}}}}
	first, err := googleResult(response, domain.ModelRequest{})
	if err != nil {
		t.Fatalf("first result: %v", err)
	}
	request := domain.ModelRequest{Messages: []domain.ModelMessage{
		{Role: domain.ModelRoleAssistant, ToolCalls: first.ToolCalls},
		{Role: domain.ModelRoleTool, Observations: []domain.ModelToolObservation{{CallID: first.ToolCalls[0].CallID, Name: "lookup"}}},
	}}
	second, err := googleResult(response, request)
	if err != nil {
		t.Fatalf("second result: %v", err)
	}
	if first.ToolCalls[0].CallID == second.ToolCalls[0].CallID {
		t.Fatalf("synthesized call id was reused across turns: %q", first.ToolCalls[0].CallID)
	}
}

func TestAdaptersRefuseAnOutputModeTheyCannotEnforce(t *testing.T) {
	request := toolRequest()
	request.Output.Mode = "grammar"
	_, anthropicErr := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, request)
	_, openAIErr := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, request)
	_, _, googleErr := (&Google{}).contentParams(request)
	for name, err := range map[string]error{"anthropic": anthropicErr, "openai": openAIErr, "google": googleErr} {
		if !errors.Is(err, domain.ErrCapabilityUnsupported) {
			t.Fatalf("%s: want ErrCapabilityUnsupported, got %v", name, err)
		}
	}
}

func TestSingleFrameStreamCarriesToolCalls(t *testing.T) {
	stub := &stubProvider{result: domain.ModelResult{
		ToolCalls: []domain.ModelToolCall{{CallID: "a", Name: "lookup", Arguments: []byte(`{}`)}}, FinishReason: domain.FinishReasonToolCalls,
	}}
	stream, err := stub.Stream(t.Context(), domain.ModelSelection{Model: "m"}, domain.ModelRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunk, err := stream.Receive(t.Context())
	if err != nil || len(chunk.ToolCalls) != 1 || chunk.FinishReason != domain.FinishReasonToolCalls {
		t.Fatalf("chunk = %+v, %v", chunk, err)
	}
}

func TestOpenAIUsesStrictModeOnlyForSchemasItAccepts(t *testing.T) {
	request := toolRequest()
	request.Output.Schema = []byte(`{"type":"object","additionalProperties":false,"required":["answer"],"properties":{"answer":{"type":"string"}}}`)
	params, err := (&OpenAI{}).completionParams(domain.ModelSelection{Model: "m"}, request)
	if err != nil {
		t.Fatalf("completionParams: %v", err)
	}
	requireAll(t, encoded(t, params), `"strict":true`)
}

// Anthropic rejects numeric and length bounds in structured output; the gateway
// still validates them, so the adapter sends the schema without them.
func TestAnthropicDropsConstraintKeywordsItsStructuredOutputRejects(t *testing.T) {
	request := toolRequest()
	request.Output.Schema = []byte(`{"type":"object","required":["title"],"properties":{"title":{"type":"string","maxLength":60},"score":{"type":"integer","minimum":0,"maximum":10},"tags":{"anyOf":[{"type":"array","minItems":2,"items":{"type":"string","minLength":2}}]}}}`)
	params, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, request)
	if err != nil {
		t.Fatalf("messageParams: %v", err)
	}
	payload := encoded(t, params)
	requireAll(t, payload, `"title":{"type":"string"}`, `"score":{"type":"integer"}`, `"required":["title"]`)
	for _, keyword := range []string{"maxLength", "minimum", "maximum", "minItems", "minLength"} {
		if strings.Contains(payload, keyword) {
			t.Fatalf("%s must not reach Anthropic: %s", keyword, payload)
		}
	}
}

// Property names are data, not keywords: a field called "minimum" must survive
// the projection, and literal values under enum or default are never rewritten.
func TestSchemaProjectionLeavesPropertyNamesAndLiteralValuesAlone(t *testing.T) {
	schema, err := schemaObject([]byte(`{"type":"object","required":["minimum","range"],"properties":{
		"minimum":{"type":"number","minimum":0},
		"range":{"type":"object","enum":[{"maxLength":5}],"default":{"maximum":9}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	payload := encoded(t, projectSchema(schema, anthropicUnsupportedKeywords))
	requireAll(t, payload, `"minimum":{"type":"number"}`, `"enum":[{"maxLength":5}]`, `"default":{"maximum":9}`)
}
