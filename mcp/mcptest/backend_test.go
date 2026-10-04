package mcptest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

type toolCatalog []domain.MCPToolDefinition

func (catalog toolCatalog) ListTools(context.Context, domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	return catalog, nil
}

type promptCatalog []domain.MCPPromptDefinition

func (catalog promptCatalog) ListPrompts(context.Context, domain.MCPCaller) ([]domain.MCPPromptDefinition, error) {
	return catalog, nil
}

func hint(value bool) *bool { return &value }

func TestToolProblems(t *testing.T) {
	readOnly := domain.MCPToolAnnotations{ReadOnlyHint: hint(true)}
	clean := toolCatalog{
		{Name: "search", Description: "search", Annotations: readOnly, InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "submit", Description: "submit", Policy: domain.MCPToolPolicy{RequiredScopes: []string{"write"}}},
	}
	if problems := toolProblems(clean); len(problems) != 0 {
		t.Fatalf("clean catalog problems = %v", problems)
	}
	broken := toolCatalog{
		{Name: "search", Description: "search", Annotations: domain.MCPToolAnnotations{ReadOnlyHint: hint(true), DestructiveHint: hint(true)}},
		{Name: "search", Annotations: readOnly, OutputSchema: json.RawMessage(`{"type":"array"}`)},
		{Name: "submit", Description: "submit"},
	}
	problems := strings.Join(toolProblems(broken), "\n")
	for _, want := range []string{"read-only and destructive", "duplicated", "empty description", "output schema", "requires no scope"} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems missing %q:\n%s", want, problems)
		}
	}
}

func TestPromptProblems(t *testing.T) {
	tools := toolCatalog{{Name: "search"}}
	prompts := promptCatalog{
		{Name: "audit", Description: "audit a page", Arguments: []domain.MCPPromptArgument{{Name: "url"}}},
		{Name: "plan", Arguments: []domain.MCPPromptArgument{{}}},
	}
	problems, err := promptProblems(context.Background(), prompts, tools, map[string][]string{"audit": {"search", "submit"}, "gone": nil})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`unpublished tool "submit"`, `"plan" has an empty description`, "unnamed argument", `"plan" does not declare`, `unpublished prompt "gone"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q:\n%s", want, joined)
		}
	}
	clean, err := promptProblems(context.Background(), prompts[:1], tools, map[string][]string{"audit": {"search"}})
	if err != nil || len(clean) != 0 {
		t.Fatalf("clean problems = %v, err = %v", clean, err)
	}
}
