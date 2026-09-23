package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

// storedChunks serves chunk content by its stored URI.
type storedChunks map[string]string

func (chunks storedChunks) Load(_ context.Context, document domain.KnowledgeDocument) ([]byte, error) {
	content, stored := chunks[document.SourceURI]
	if !stored {
		return nil, domain.ErrNotFound
	}
	return []byte(content), nil
}

func pinnedRequest() domain.PinnedKnowledgeRequest {
	return domain.PinnedKnowledgeRequest{
		TenantContext: domain.TenantContext{TenantID: 7}, AgentID: "writer", AgentVersion: "v3",
		Entitlements: []byte(`["public"]`),
	}
}

func chunkRowOf(documentID string, chunkNo int, uri string, tokens int64) []any {
	start, end := chunkNo*16, chunkNo*16+24
	return []any{documentID, int64(chunkNo), uri, tokens, "object://src/" + documentID, "s1", int64(start), int64(end), `["public"]`}
}

func TestPinnedKnowledgeKeepsIncidentalPrefixAndSeparatesDisjointChunks(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", nil, "standing"}},
		qPinnedChunks: {
			{"standing", int64(0), "object://c/0", int64(2), "object://src/standing", "s1", int64(0), int64(5), `["public"]`},
			{"standing", int64(1), "object://c/1", int64(2), "object://src/standing", "s1", int64(6), int64(11), `["public"]`},
		},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{
		"object://c/0": "alpha", "object://c/1": "apple",
	}}
	pinned, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(pinned.Documents[0].Content); got != "alpha\napple" {
		t.Fatalf("content = %q", got)
	}
}

func TestPinnedKnowledgeReadsBoundDocumentsWholeWithoutRepeatingOverlap(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", nil, "standing"}},
		qPinnedChunks: {
			chunkRowOf("standing", 0, "object://c/0", 4),
			chunkRowOf("standing", 1, "object://c/1", 3),
			chunkRowOf("other", 0, "object://c/2", 9),
		},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{
		"object://c/0": "never promise a ranking",
		"object://c/1": "a ranking; always cite the source",
		"object://c/2": "unbound",
	}}
	pinned, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest())
	if err != nil {
		t.Fatalf("PinnedKnowledge: %v", err)
	}
	if len(pinned.Documents) != 1 || pinned.Documents[0].DocumentID != "standing" {
		t.Fatalf("only the bound document is read whole, got %+v", pinned.Documents)
	}
	if content := string(pinned.Documents[0].Content); content != "never promise a ranking; always cite the source" {
		t.Fatalf("content = %q", content)
	}
	if pinned.TokenCount != 7 || pinned.Documents[0].SourceVersion != "s1" {
		t.Fatalf("pinned = %+v", pinned)
	}
	if args := query.named(qPinnedChunks)[0].args; len(args) != 3 || args[0] != int64(7) || args[1] != "rules" || args[2] != "kv1" {
		t.Fatalf("scan args = %v", args)
	}
}

func TestPinnedKnowledgeReadsEveryDocumentWhenTheBindingNamesNone(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", nil, nil}},
		qPinnedChunks: {
			chunkRowOf("a", 0, "object://c/a", 2),
			chunkRowOf("b", 0, "object://c/b", 2),
		},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{
		"object://c/a": "first", "object://c/b": "second",
	}}
	pinned, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest())
	if err != nil || len(pinned.Documents) != 2 || pinned.TokenCount != 4 {
		t.Fatalf("pinned = %+v, %v", pinned, err)
	}
}

func TestPinnedKnowledgeFailsLoudlyInsteadOfTruncating(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", int64(3), "standing"}},
		qPinnedChunks:   {chunkRowOf("standing", 0, "object://c/0", 9)},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{"object://c/0": "long"}}
	if _, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest()); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("a binding above its token ceiling = %v", err)
	}
}

func TestPinnedKnowledgeFailsWhenABoundDocumentIsNotAuthorized(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", nil, "standing"}},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{}}
	if _, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a bound document with no authorized chunk = %v", err)
	}
}

func TestPinnedKnowledgeSkipsChunksThePrincipalIsNotEntitledTo(t *testing.T) {
	secret := chunkRowOf("standing", 1, "object://c/1", 3)
	secret[8] = `["group:legal"]`
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", nil, nil}},
		qPinnedChunks:   {chunkRowOf("standing", 0, "object://c/0", 4), secret},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{"object://c/0": "public rule"}}
	pinned, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest())
	if err != nil || string(pinned.Documents[0].Content) != "public rule" || pinned.TokenCount != 4 {
		t.Fatalf("pinned = %+v, %v", pinned, err)
	}

	corrupt := chunkRowOf("standing", 0, "object://c/0", 4)
	corrupt[8] = "not json"
	query.rows[qPinnedChunks] = [][]any{corrupt}
	if _, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest()); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unreadable stored labels = %v", err)
	}
}

func TestPinnedKnowledgeIgnoresUnboundDocumentsBeforeReadingTheirLabels(t *testing.T) {
	unbound := chunkRowOf("other", 0, "object://c/other", 4)
	unbound[8] = "not json"
	query := &ingestQueryFake{rows: map[string][][]any{
		qPinnedBindings: {{"rules", "kv1", nil, "standing"}},
		qPinnedChunks:   {unbound, chunkRowOf("standing", 0, "object://c/0", 4)},
	}}
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: query}, Content: storedChunks{"object://c/0": "public rule"}}
	pinned, err := resolver.PinnedKnowledge(context.Background(), pinnedRequest())
	if err != nil || len(pinned.Documents) != 1 || pinned.Documents[0].DocumentID != "standing" {
		t.Fatalf("pinned = %+v, %v", pinned, err)
	}
}

func TestPinnedKnowledgeRequiresAnAgentVersionAndEntitlements(t *testing.T) {
	resolver := &TablePinnedKnowledge{DB: ingestDBFake{query: &ingestQueryFake{}}, Content: storedChunks{}}
	ctx := context.Background()
	bare := pinnedRequest()
	bare.AgentVersion = ""
	if _, err := resolver.PinnedKnowledge(ctx, bare); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("without an agent version = %v", err)
	}
	open := pinnedRequest()
	open.Entitlements = nil
	if _, err := resolver.PinnedKnowledge(ctx, open); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("retrieval fails closed, so whole reading must too: %v", err)
	}
}
