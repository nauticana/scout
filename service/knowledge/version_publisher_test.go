package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func knowledgeVersion() domain.KnowledgeVersion {
	return domain.KnowledgeVersion{
		TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1",
		EmbeddingProvider: "openai", EmbeddingModel: "text-embedding-3-small",
	}
}

func TestVersionPublisherCreatesTheBaseAndTheVersionOnce(t *testing.T) {
	query := &ingestQueryFake{}
	publisher := &VersionPublisher{DB: ingestDBFake{query: query}}
	if err := publisher.EnsureVersion(context.Background(), knowledgeVersion()); err != nil {
		t.Fatal(err)
	}
	if len(query.named(qVersionBaseInsert)) != 1 || len(query.named(qVersionInsert)) != 1 || query.commits != 1 {
		t.Fatalf("calls = %v", query.names())
	}
	if args := query.named(qVersionBaseInsert)[0].args; args[2] != "kb" {
		t.Fatalf("base display name defaults to its id, got %v", args)
	}
	query.rows = map[string][][]any{qVersionGet: {{"openai", "text-embedding-3-small"}}}
	if err := publisher.EnsureVersion(context.Background(), knowledgeVersion()); err != nil || len(query.named(qVersionInsert)) != 1 {
		t.Fatalf("replay = %v, inserts = %d", err, len(query.named(qVersionInsert)))
	}
	changed := knowledgeVersion()
	changed.EmbeddingModel = "text-embedding-3-large"
	if err := publisher.EnsureVersion(context.Background(), changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a published version never changes its embedding: %v", err)
	}
	wholeOnly := knowledgeVersion()
	wholeOnly.EmbeddingProvider, wholeOnly.EmbeddingModel = "", ""
	if err := publisher.EnsureVersion(context.Background(), wholeOnly); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("whole-only against an embedded version: %v", err)
	}
	half := knowledgeVersion()
	half.EmbeddingModel = ""
	if err := publisher.EnsureVersion(context.Background(), half); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("half an embedding = %v", err)
	}
	if err := publisher.EnsureVersion(context.Background(), domain.KnowledgeVersion{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty = %v", err)
	}
}
