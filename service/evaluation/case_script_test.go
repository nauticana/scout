package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func refundScript() domain.CaseScript {
	return domain.CaseScript{
		Turns: []domain.CaseTurn{{Input: "and the second order?"}},
		Tools: []domain.CaseTool{{ToolID: "refund.issue", IsError: true}, {ToolID: "orders.get", Output: json.RawMessage(`{ "id" : 1 }`)}},
	}
}

func TestNormalizeScriptCanonicalizesAndBoundsAScript(t *testing.T) {
	script, err := normalizeScript(refundScript(), scriptBounds{})
	if err != nil {
		t.Fatal(err)
	}
	if script.Tools[0].ToolID != "orders.get" || string(script.Tools[0].Output) != `{"id":1}` || !script.Tools[1].IsError || script.Tools[1].Output != nil {
		t.Fatalf("canonical tools = %+v", script.Tools)
	}
	for name, test := range map[string]struct {
		mutate func(*domain.CaseScript)
		bounds scriptBounds
	}{
		"too many turns":         {func(s *domain.CaseScript) { s.Turns = append(s.Turns, domain.CaseTurn{Input: "x"}) }, scriptBounds{maxTurns: 2}},
		"too many tools":         {func(*domain.CaseScript) {}, scriptBounds{maxTools: 1}},
		"oversized output":       {func(*domain.CaseScript) {}, scriptBounds{maxToolOutputBytes: 4}},
		"oversized turn":         {func(*domain.CaseScript) {}, scriptBounds{maxInputBytes: 4}},
		"blank turn":             {func(s *domain.CaseScript) { s.Turns[0].Input = "  " }, scriptBounds{}},
		"nul in turn":            {func(s *domain.CaseScript) { s.Turns[0].Input = "a\x00b" }, scriptBounds{}},
		"output that is no JSON": {func(s *domain.CaseScript) { s.Tools[1].Output = json.RawMessage(`{not json`) }, scriptBounds{}},
		"missing output":         {func(s *domain.CaseScript) { s.Tools[1].Output = nil }, scriptBounds{}},
		"failure with output":    {func(s *domain.CaseScript) { s.Tools[0].Output = json.RawMessage(`{}`) }, scriptBounds{}},
		"repeated tool": {func(s *domain.CaseScript) {
			s.Tools[0] = domain.CaseTool{ToolID: "orders.get", Output: json.RawMessage(`1`)}
		}, scriptBounds{}},
		"padded tool id": {func(s *domain.CaseScript) { s.Tools[0].ToolID = " refund.issue" }, scriptBounds{}},
		"long tool id":   {func(s *domain.CaseScript) { s.Tools[0].ToolID = strings.Repeat("t", 81) }, scriptBounds{}},
	} {
		script := refundScript()
		test.mutate(&script)
		if _, err := normalizeScript(script, test.bounds); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}

func scriptedExample(t *testing.T, content []byte) (domain.GoldenExample, objectReaderFunc) {
	t.Helper()
	example := replayExample()
	example.Script = domain.ObjectRef{URI: "object://golden/script", Digest: sha256Hex(content)}
	return example, func(_ context.Context, ref domain.ObjectRef) ([]byte, error) {
		if ref.URI == example.Script.URI {
			return content, nil
		}
		return []byte(replayInput), nil
	}
}

func TestReadScriptChecksTheDigestAndDecodesStrictly(t *testing.T) {
	ctx := context.Background()
	normalized, err := normalizeScript(refundScript(), scriptBounds{})
	if err != nil {
		t.Fatal(err)
	}
	content, err := canonicalScript(normalized)
	if err != nil {
		t.Fatal(err)
	}
	example, reader := scriptedExample(t, content)
	script, err := readScript(ctx, reader, example)
	if err != nil || len(script.Turns) != 1 || len(script.Tools) != 2 {
		t.Fatalf("script = %+v, %v", script, err)
	}
	if script, err := readScript(ctx, nil, replayExample()); err != nil || script.Scripted() {
		t.Fatalf("an unscripted example reads as the zero script without a reader: %+v, %v", script, err)
	}
	tampered := example
	tampered.Script.Digest = sha256Hex([]byte("other"))
	if _, err := readScript(ctx, reader, tampered); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tampered script = %v", err)
	}
	for name, body := range map[string]string{
		"unknown field":  `{"turns":[],"extra":1}`,
		"trailing JSON":  `{} {}`,
		"blank turn":     `{"turns":[{"input":" "}]}`,
		"repeated tool":  `{"tools":[{"tool_id":"a","output":1},{"tool_id":"a","output":2}]}`,
		"failure output": `{"tools":[{"tool_id":"a","output":1,"is_error":true}]}`,
	} {
		bad, badReader := scriptedExample(t, []byte(body))
		if _, err := readScript(ctx, badReader, bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
	halfRef := example
	halfRef.Script.URI = ""
	if _, err := readScript(ctx, reader, halfRef); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a digest without a URI = %v", err)
	}
}

func TestScriptSandboxAnswersOnlyScriptedTools(t *testing.T) {
	ctx := context.Background()
	normalized, _ := normalizeScript(refundScript(), scriptBounds{})
	sandbox, err := ScriptSandbox(ctx, replayExample(), normalized)
	if err != nil {
		t.Fatal(err)
	}
	call := func(toolID string) (domain.ToolResult, error) {
		return sandbox.Invoke(ctx, domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, ToolID: toolID})
	}
	if result, err := call("orders.get"); err != nil || string(result.Output) != `{"id":1}` {
		t.Fatalf("scripted tool = %s, %v", result.Output, err)
	}
	if _, err := call("refund.issue"); !errors.Is(err, ErrSandboxToolFailed) || !errors.Is(err, domain.ErrContractFailed) {
		t.Fatalf("scripted failure = %v", err)
	}
	if _, err := call("payments.charge"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unscripted tool must not answer: %v", err)
	}
	empty, _ := ScriptSandbox(ctx, replayExample(), domain.CaseScript{})
	if _, err := empty.Invoke(ctx, domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, ToolID: "orders.get"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unscripted example's sandbox answers nothing: %v", err)
	}
	if len(sandbox.Recorded()) != 3 {
		t.Fatalf("recorded = %+v", sandbox.Recorded())
	}
}

func TestTrajectoryDocumentRoundTrips(t *testing.T) {
	events := []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToolCall, Name: "orders.get", Payload: []byte(`{"id":1}`), Offset: 1500000000, Usage: domain.Usage{InputTokens: 3, CostMinorUnits: 2, Currency: "EUR"}},
		{Sequence: 2, Kind: domain.TrajectoryState, Name: "answer", Payload: []byte("plain text"), Failed: true, Recovered: true},
		{Sequence: 3, Kind: domain.TrajectoryState, Name: ReplayCompleted},
	}
	content, err := encodeTrajectory(events)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeTrajectory(content)
	if err != nil || len(decoded) != 3 {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
	if string(decoded[0].Payload) != `{"id":1}` || decoded[0].Offset.Milliseconds() != 1500 || decoded[0].Usage.CostMinorUnits != 2 ||
		string(decoded[1].Payload) != `"plain text"` || !decoded[1].Failed || !decoded[1].Recovered || decoded[2].Payload != nil {
		t.Fatalf("decoded = %+v", decoded)
	}
	if _, err := decodeTrajectory([]byte(`{"events":[],"x":1}`)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown field = %v", err)
	}
	for _, malformed := range [][]byte{
		[]byte(`{"events":[{"sequence":2,"kind":"state","offset_ms":0}]}`),
		[]byte(`{"events":[{"sequence":1,"kind":"unknown","offset_ms":0}]}`),
		[]byte(`{"events":[{"sequence":1,"kind":"state","offset_ms":-1}]}`),
		[]byte(`{"events":[{"sequence":1,"kind":"state","offset_ms":0,"usage":{"search_queries":-1}}]}`),
	} {
		if _, err := decodeTrajectory(malformed); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("malformed trajectory %s = %v", malformed, err)
		}
	}
}
