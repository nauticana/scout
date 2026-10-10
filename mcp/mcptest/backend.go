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

// AssertToolBackend checks the catalog a backend registers: unique, described
// tools, object schemas, consistent annotations, at least one scope on every
// tool not marked read-only, coherent tenant policy, and a host listing that
// names only catalog tools.
func AssertToolBackend(t *testing.T, backend contract.MCPToolCatalog) {
	t.Helper()
	problems, err := catalogProblems(t.Context(), backend)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

func catalogProblems(ctx context.Context, backend contract.MCPToolCatalog) ([]string, error) {
	definitions, err := backend.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("tool catalog: %w", err)
	}
	listed, err := backend.ListTools(ctx, mcp.HostCaller())
	if err != nil {
		return nil, fmt.Errorf("list tools for the host: %w", err)
	}
	problems := toolProblems(definitions)
	for _, definition := range listed {
		if !slices.ContainsFunc(definitions, func(d domain.MCPToolDefinition) bool { return d.Name == definition.Name }) {
			problems = append(problems, fmt.Sprintf("tool %q is listed but not in the catalog", definition.Name))
		}
	}
	return problems, nil
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
		problems = append(problems, policyProblems(name, definition.Policy)...)
	}
	return problems
}

// policyProblems checks the tenant fields mcp.ToolLister reads.
func policyProblems(name string, policy domain.MCPToolPolicy) []string {
	var problems []string
	if !policy.Tenant {
		if policy.ActionLevel != 0 || policy.OwnTenantOnly || len(policy.Grants) > 0 || policy.AnyGrant || len(policy.Sources) > 0 {
			problems = append(problems, fmt.Sprintf("tool %q declares tenant policy without Tenant", name))
		}
		return problems
	}
	if policy.ActionLevel < 0 {
		problems = append(problems, fmt.Sprintf("tool %q has a negative action level", name))
	}
	if policy.AnyGrant && len(policy.Grants) < 2 {
		problems = append(problems, fmt.Sprintf("tool %q sets AnyGrant with fewer than two grants", name))
	}
	for _, grant := range policy.Grants {
		if strings.TrimSpace(grant.Object) == "" || strings.TrimSpace(grant.Action) == "" {
			problems = append(problems, fmt.Sprintf("tool %q has a grant without object or action", name))
		}
	}
	seen := make(map[string]bool, len(policy.Sources))
	for _, source := range policy.Sources {
		if strings.TrimSpace(source) == "" || seen[source] {
			problems = append(problems, fmt.Sprintf("tool %q source %q is empty or duplicated", name, source))
		}
		seen[source] = true
	}
	return problems
}

func promptProblems(ctx context.Context, prompts contract.MCPPromptCatalog, tools contract.MCPToolCatalog, uses map[string][]string) ([]string, error) {
	promptDefinitions, err := prompts.ListPrompts(ctx, mcp.HostCaller())
	if err != nil {
		return nil, fmt.Errorf("list prompts: %w", err)
	}
	toolDefinitions, err := tools.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("tool catalog: %w", err)
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
