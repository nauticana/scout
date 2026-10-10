package mcptest

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

type toolCatalog []domain.MCPToolDefinition

func (catalog toolCatalog) Catalog(context.Context) ([]domain.MCPToolDefinition, error) {
	return catalog, nil
}

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

func TestPolicyProblems(t *testing.T) {
	grant := domain.MCPGrant{Object: "REPORT", Action: "READ", Scope: "seo"}
	clean := []domain.MCPToolPolicy{
		{},
		{Tenant: true, ActionLevel: 2, OwnTenantOnly: true, Grants: []domain.MCPGrant{grant}, Sources: []string{"ads"}},
		{Tenant: true, AnyGrant: true, Grants: []domain.MCPGrant{grant, {Object: "REPORT", Action: "EXPORT"}}},
	}
	for _, policy := range clean {
		if problems := policyProblems("t", policy); len(problems) != 0 {
			t.Errorf("%+v: %v", policy, problems)
		}
	}
	broken := map[string]domain.MCPToolPolicy{
		"without Tenant":      {Grants: []domain.MCPGrant{grant}},
		"negative":            {Tenant: true, ActionLevel: -1},
		"AnyGrant":            {Tenant: true, AnyGrant: true, Grants: []domain.MCPGrant{grant}},
		"without object":      {Tenant: true, Grants: []domain.MCPGrant{{Action: "READ"}}},
		"empty or duplicated": {Tenant: true, Sources: []string{"ads", "ads"}},
	}
	for want, policy := range broken {
		if problems := strings.Join(policyProblems("t", policy), "\n"); !strings.Contains(problems, want) {
			t.Errorf("%+v: problems %q lack %q", policy, problems, want)
		}
	}
}

// splitCatalog lists a tool to the host that it does not register.
type splitCatalog struct{ toolCatalog }

func (catalog splitCatalog) ListTools(context.Context, domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	return append(slices.Clone(catalog.toolCatalog), domain.MCPToolDefinition{Name: "ghost"}), nil
}

func TestCatalogProblemsNameUnregisteredListings(t *testing.T) {
	catalog := splitCatalog{toolCatalog{{Name: "search", Description: "search", Policy: domain.MCPToolPolicy{RequiredScopes: []string{"read"}}}}}
	problems, err := catalogProblems(context.Background(), catalog)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], `"ghost" is listed but not in the catalog`) {
		t.Fatalf("problems = %v, err = %v", problems, err)
	}
}

func TestCatalogSize(t *testing.T) {
	catalog := toolCatalog{
		{Name: "small", Description: "s", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "large", Description: strings.Repeat("d", 200),
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"offset":{"type":"integer"}}}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"rows":{"type":"array"}}}`)},
	}
	report, err := CatalogSize(context.Background(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if report.Tools[0].Name != "large" || report.Description != 201 || report.Total <= report.Description+report.Input+report.Output {
		t.Fatalf("report = %+v", report)
	}
	if report.Tools[0].Output <= len(catalog[1].OutputSchema) {
		t.Fatalf("the published output schema, with pagination, is measured: %+v", report.Tools[0])
	}
	AssertCatalogBudget(t, catalog, report.Total)
	if err := report.within(report.Total - 1); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("over-budget catalog error = %v", err)
	}
}
