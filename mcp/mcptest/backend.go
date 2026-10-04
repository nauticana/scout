package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
	"github.com/nauticana/scout/mcp"
)

// AssertToolBackend checks the full catalog a backend publishes: unique,
// described tools, object schemas, consistent annotations, and at least one
// scope on every tool not marked read-only.
func AssertToolBackend(t *testing.T, backend contract.MCPToolCatalog) {
	t.Helper()
	definitions, err := backend.ListTools(t.Context(), mcp.HostCaller())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, problem := range toolProblems(definitions) {
		t.Error(problem)
	}
}

// AssertPromptBackend checks the full prompt catalog: unique, described
// prompts with named arguments, and uses (prompt name to the tools it guides
// the client to call) covering every prompt and naming only published tools.
func AssertPromptBackend(t *testing.T, prompts contract.MCPPromptCatalog, tools contract.MCPToolCatalog, uses map[string][]string) {
	t.Helper()
	problems, err := promptProblems(t.Context(), prompts, tools, uses)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

func toolProblems(definitions []domain.MCPToolDefinition) []string {
	var problems []string
	seen := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		name := definition.Name
		if strings.TrimSpace(name) == "" || seen[name] {
			problems = append(problems, fmt.Sprintf("tool name %q is empty or duplicated", name))
		}
		seen[name] = true
		if strings.TrimSpace(definition.Description) == "" {
			problems = append(problems, fmt.Sprintf("tool %q has an empty description", name))
		}
		if !objectSchema(definition.InputSchema) {
			problems = append(problems, fmt.Sprintf("tool %q input schema is not a JSON object schema", name))
		}
		// Registration compiles output schemas with the same validator.
		if len(definition.OutputSchema) > 0 {
			if schema, err := jsonschema.Compile(definition.OutputSchema); err != nil || schema.Type != "object" {
				problems = append(problems, fmt.Sprintf("tool %q output schema is not an enforceable JSON object schema: %v", name, err))
			}
		}
		annotations := definition.Annotations
		readOnly := annotations.ReadOnlyHint != nil && *annotations.ReadOnlyHint
		if readOnly && annotations.DestructiveHint != nil && *annotations.DestructiveHint {
			problems = append(problems, fmt.Sprintf("tool %q is marked both read-only and destructive", name))
		}
		if !readOnly && len(definition.Policy.RequiredScopes) == 0 {
			problems = append(problems, fmt.Sprintf("tool %q may write but requires no scope", name))
		}
	}
	return problems
}

func promptProblems(ctx context.Context, prompts contract.MCPPromptCatalog, tools contract.MCPToolCatalog, uses map[string][]string) ([]string, error) {
	promptDefinitions, err := prompts.ListPrompts(ctx, mcp.HostCaller())
	if err != nil {
		return nil, fmt.Errorf("list prompts: %w", err)
	}
	toolDefinitions, err := tools.ListTools(ctx, mcp.HostCaller())
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	var problems []string
	published := make(map[string]bool, len(promptDefinitions))
	for _, definition := range promptDefinitions {
		name := definition.Name
		if strings.TrimSpace(name) == "" || published[name] {
			problems = append(problems, fmt.Sprintf("prompt name %q is empty or duplicated", name))
		}
		published[name] = true
		if strings.TrimSpace(definition.Description) == "" {
			problems = append(problems, fmt.Sprintf("prompt %q has an empty description", name))
		}
		for _, argument := range definition.Arguments {
			if strings.TrimSpace(argument.Name) == "" {
				problems = append(problems, fmt.Sprintf("prompt %q has an unnamed argument", name))
			}
		}
		if _, ok := uses[name]; !ok {
			problems = append(problems, fmt.Sprintf("prompt %q does not declare the tools it uses", name))
		}
	}
	for prompt, named := range uses {
		if !published[prompt] {
			problems = append(problems, fmt.Sprintf("uses names unpublished prompt %q", prompt))
		}
		for _, tool := range named {
			if !slices.ContainsFunc(toolDefinitions, func(definition domain.MCPToolDefinition) bool { return definition.Name == tool }) {
				problems = append(problems, fmt.Sprintf("prompt %q names unpublished tool %q", prompt, tool))
			}
		}
	}
	return problems, nil
}

func objectSchema(schema json.RawMessage) bool {
	if len(schema) == 0 {
		return true
	}
	var parsed struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(schema, &parsed) == nil && parsed.Type == "object"
}
