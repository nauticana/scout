package mcp

import (
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func fixedEnvelopes() Envelopes {
	return Envelopes{Source: "source-a", now: func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}}
}

func resultText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("empty content")
	}
	content, ok := mcpgo.AsTextContent(result.Content[0])
	if !ok {
		t.Fatalf("content is not text: %T", result.Content[0])
	}
	return content.Text
}

func TestEnvelopesDefaults(t *testing.T) {
	text := resultText(t, fixedEnvelopes().Wrap(map[string]int{"n": 1}, nil))
	if !strings.Contains(text, `"source": "source-a"`) || !strings.Contains(text, "2026-01-02T03:04:05Z") {
		t.Fatalf("envelope defaults missing: %s", text)
	}
}

func TestEnvelopesPagination(t *testing.T) {
	withNext := resultText(t, fixedEnvelopes().WrapWithPagination([]int{1, 2}, 2, 0, 2, true))
	if !strings.Contains(withNext, `"next_offset": 2`) {
		t.Fatalf("next offset missing: %s", withNext)
	}
	withoutNext := resultText(t, fixedEnvelopes().WrapWithPagination([]int{1}, 2, 0, 1, false))
	if strings.Contains(withoutNext, "next_offset") {
		t.Fatalf("unexpected next offset: %s", withoutNext)
	}
}
