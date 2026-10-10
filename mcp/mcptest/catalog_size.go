package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/mcp"
)

// reportedTools is how many of the largest tools CatalogReport.String names.
const reportedTools = 20

// ToolSize is one tool's share of the encoded tools/list.
type ToolSize struct {
	Name        string
	Total       int
	Description int
	Input       int
	Output      int
}

// CatalogReport measures the tools/list a caller who sees the whole catalog
// downloads, output schemas included; clients load it into every conversation.
type CatalogReport struct {
	Total       int
	Description int
	Input       int
	Output      int
	// Tools are largest first.
	Tools []ToolSize
}

// CatalogSize encodes every catalog tool as Scout publishes it.
func CatalogSize(ctx context.Context, catalog contract.MCPToolCatalog) (CatalogReport, error) {
	definitions, err := catalog.Catalog(ctx)
	if err != nil {
		return CatalogReport{}, fmt.Errorf("tool catalog: %w", err)
	}
	report := CatalogReport{Tools: make([]ToolSize, 0, len(definitions))}
	tools := make([]mcpgo.Tool, 0, len(definitions))
	for _, definition := range definitions {
		tool, err := mcp.ListedTool(definition)
		if err != nil {
			return CatalogReport{}, err
		}
		tools = append(tools, tool)
		encoded, err := json.Marshal(tool)
		if err != nil {
			return CatalogReport{}, fmt.Errorf("encode tool %q: %w", definition.Name, err)
		}
		size := ToolSize{Name: definition.Name, Total: len(encoded), Description: len(tool.Description),
			Input: len(definition.InputSchema), Output: len(tool.RawOutputSchema)}
		report.Description += size.Description
		report.Input += size.Input
		report.Output += size.Output
		report.Tools = append(report.Tools, size)
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return CatalogReport{}, fmt.Errorf("encode catalog: %w", err)
	}
	report.Total = len(encoded)
	sort.SliceStable(report.Tools, func(i, j int) bool { return report.Tools[i].Total > report.Tools[j].Total })
	return report, nil
}

func (r CatalogReport) String() string {
	var text strings.Builder
	fmt.Fprintf(&text, "tools/list %d bytes for %d tools: descriptions %d, input schemas %d, output schemas %d\n",
		r.Total, len(r.Tools), r.Description, r.Input, r.Output)
	for _, tool := range r.Tools[:min(reportedTools, len(r.Tools))] {
		fmt.Fprintf(&text, "  %-40s %7d  description %6d  input %6d  output %6d\n", tool.Name, tool.Total, tool.Description, tool.Input, tool.Output)
	}
	return text.String()
}

// AssertCatalogBudget fails when the encoded catalog exceeds maxBytes. The
// report is logged, so `go test -v` shows it on success too.
func AssertCatalogBudget(t *testing.T, catalog contract.MCPToolCatalog, maxBytes int) {
	t.Helper()
	if maxBytes <= 0 {
		t.Fatalf("catalog budget %d must be positive", maxBytes)
	}
	report, err := CatalogSize(t.Context(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(report.String())
	if err := report.within(maxBytes); err != nil {
		t.Error(err)
	}
}

func (r CatalogReport) within(maxBytes int) error {
	if r.Total > maxBytes {
		return fmt.Errorf("tools/list is %d bytes, over the %d budget", r.Total, maxBytes)
	}
	return nil
}
