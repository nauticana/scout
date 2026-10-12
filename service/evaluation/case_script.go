package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Defaults of a golden case's script bounds; the agent_evaluation_case_* flags override them.
const (
	DefaultCaseMaxTurns       = 10
	DefaultCaseMaxTools       = 20
	DefaultCaseToolOutputSize = 16 << 10
)

// maxScriptToolID is the width of golden_case_tool.tool_id.
const maxScriptToolID = 80

// ErrSandboxToolFailed answers a call of a tool the script marks failing, as a live tool
// failure reaches the model: as its class only.
var ErrSandboxToolFailed = fmt.Errorf("%w: %w: sandboxed tool failure", domain.ErrContractFailed, domain.ErrNoEffect)

// SandboxFactory returns the fresh sandbox one replay answers every tool call with, built from
// the example's decoded script; an unscripted example has the zero script.
type SandboxFactory func(ctx context.Context, example domain.GoldenExample, script domain.CaseScript) (contract.ToolSandbox, error)

var _ SandboxFactory = ScriptSandbox

// ScriptSandbox is the production SandboxFactory: a strict RecordingSandbox answering each
// tool with its scripted response, so a call of any other tool fails and no replay reaches a
// live tool. An unscripted example gets an empty strict sandbox.
func ScriptSandbox(_ context.Context, _ domain.GoldenExample, script domain.CaseScript) (contract.ToolSandbox, error) {
	sandbox := &RecordingSandbox{Strict: true, Responses: map[string]domain.ToolResult{}, Failures: map[string]bool{}}
	for _, tool := range script.Tools {
		if tool.IsError {
			sandbox.Failures[tool.ToolID] = true
			continue
		}
		sandbox.Responses[tool.ToolID] = domain.ToolResult{Output: append([]byte(nil), tool.Output...)}
	}
	return sandbox, nil
}

// scriptBounds limit what one case may script; zero takes each default.
type scriptBounds struct {
	maxTurns, maxTools, maxToolOutputBytes, maxInputBytes int
}

// normalizeScript validates a script against its bounds and returns it canonical: follow-up
// inputs non-blank UTF-8, tool ids distinct and sorted, outputs compact JSON objects or values,
// and a failing tool without output.
func normalizeScript(script domain.CaseScript, bounds scriptBounds) (domain.CaseScript, error) {
	maxTurns := orDefault(bounds.maxTurns, DefaultCaseMaxTurns)
	if len(script.Turns)+1 > maxTurns {
		return domain.CaseScript{}, fmt.Errorf("%w: a case scripts at most %d turns, its input included", domain.ErrValidation, maxTurns)
	}
	if maxTools := orDefault(bounds.maxTools, DefaultCaseMaxTools); len(script.Tools) > maxTools {
		return domain.CaseScript{}, fmt.Errorf("%w: a case scripts at most %d tool responses", domain.ErrValidation, maxTools)
	}
	maxInput := orDefault(bounds.maxInputBytes, 32<<10)
	normalized := domain.CaseScript{}
	for i, turn := range script.Turns {
		if strings.TrimSpace(turn.Input) == "" || len(turn.Input) > maxInput || !validText([]byte(turn.Input)) {
			return domain.CaseScript{}, fmt.Errorf("%w: turn %d input must be 1 to %d bytes of UTF-8", domain.ErrValidation, i+2, maxInput)
		}
		normalized.Turns = append(normalized.Turns, turn)
	}
	maxOutput := orDefault(bounds.maxToolOutputBytes, DefaultCaseToolOutputSize)
	for _, tool := range script.Tools {
		if len(tool.Output) > maxOutput {
			return domain.CaseScript{}, fmt.Errorf("%w: the output scripted for tool %q exceeds %d bytes", domain.ErrValidation, tool.ToolID, maxOutput)
		}
		canonical, err := canonicalTool(tool)
		if err != nil {
			return domain.CaseScript{}, err
		}
		normalized.Tools = append(normalized.Tools, canonical)
	}
	slices.SortFunc(normalized.Tools, func(a, b domain.CaseTool) int { return strings.Compare(a.ToolID, b.ToolID) })
	for i := 1; i < len(normalized.Tools); i++ {
		if normalized.Tools[i].ToolID == normalized.Tools[i-1].ToolID {
			return domain.CaseScript{}, fmt.Errorf("%w: tool %q is scripted twice", domain.ErrValidation, normalized.Tools[i].ToolID)
		}
	}
	return normalized, nil
}

// canonicalTool checks one tool response and compacts its output.
func canonicalTool(tool domain.CaseTool) (domain.CaseTool, error) {
	if tool.ToolID == "" || tool.ToolID != strings.TrimSpace(tool.ToolID) || len(tool.ToolID) > maxScriptToolID || !validText([]byte(tool.ToolID)) {
		return domain.CaseTool{}, fmt.Errorf("%w: a scripted tool id is 1 to %d bytes without surrounding space", domain.ErrValidation, maxScriptToolID)
	}
	if tool.IsError {
		if len(bytes.TrimSpace(tool.Output)) > 0 {
			return domain.CaseTool{}, fmt.Errorf("%w: failing tool %q carries no output", domain.ErrValidation, tool.ToolID)
		}
		return domain.CaseTool{ToolID: tool.ToolID, IsError: true}, nil
	}
	var compact bytes.Buffer
	if !utf8.Valid(tool.Output) || json.Compact(&compact, tool.Output) != nil || compact.Len() == 0 {
		return domain.CaseTool{}, fmt.Errorf("%w: the output scripted for tool %q must be JSON", domain.ErrValidation, tool.ToolID)
	}
	return domain.CaseTool{ToolID: tool.ToolID, Output: compact.Bytes()}, nil
}

// canonicalScript is the JSON a frozen script is stored and digested as.
func canonicalScript(script domain.CaseScript) ([]byte, error) {
	encoded, err := json.Marshal(script)
	if err != nil {
		return nil, fmt.Errorf("encode case script: %w", err)
	}
	return encoded, nil
}

// readScript reads an example's script and checks it against its digest; an unscripted
// example has the zero script. A script that does not decode strictly is refused.
func readScript(ctx context.Context, reader contract.ObjectReader, example domain.GoldenExample) (domain.CaseScript, error) {
	ref := example.Script
	if ref.URI == "" && ref.Digest == "" {
		return domain.CaseScript{}, nil
	}
	if ref.URI == "" || !isSHA256Hex(ref.Digest) {
		return domain.CaseScript{}, fmt.Errorf("%w: script of example %q needs a URI and a SHA-256 digest", domain.ErrValidation, example.ExampleID)
	}
	if reader == nil {
		return domain.CaseScript{}, fmt.Errorf("%w: reading a case script needs an object reader", domain.ErrNotReady)
	}
	content, err := reader.Read(ctx, ref)
	if err != nil {
		return domain.CaseScript{}, fmt.Errorf("read script of example %q: %w", example.ExampleID, err)
	}
	if sha256Hex(content) != strings.ToLower(ref.Digest) {
		return domain.CaseScript{}, fmt.Errorf("%w: script of example %q does not match its digest", domain.ErrValidation, example.ExampleID)
	}
	var script domain.CaseScript
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&script); err != nil {
		return domain.CaseScript{}, fmt.Errorf("%w: script of example %q: %v", domain.ErrValidation, example.ExampleID, err)
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.CaseScript{}, fmt.Errorf("%w: script of example %q has trailing JSON", domain.ErrValidation, example.ExampleID)
	}
	for i, turn := range script.Turns {
		if strings.TrimSpace(turn.Input) == "" || !validText([]byte(turn.Input)) {
			return domain.CaseScript{}, fmt.Errorf("%w: script of example %q has a blank turn %d", domain.ErrValidation, example.ExampleID, i+2)
		}
	}
	seen := make(map[string]bool, len(script.Tools))
	for _, tool := range script.Tools {
		if _, err = canonicalTool(tool); err != nil || seen[tool.ToolID] {
			return domain.CaseScript{}, fmt.Errorf("%w: script of example %q has an invalid or repeated tool response", domain.ErrValidation, example.ExampleID)
		}
		seen[tool.ToolID] = true
	}
	return script, nil
}
