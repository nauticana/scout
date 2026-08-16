package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func gcRow(doc, active, superseded string, tombstoned bool) []any {
	var supersededValue any
	if superseded != "" {
		supersededValue = superseded
	}
	return []any{int64(7), "kb", doc, active, supersededValue, tombstoned, true}
}

func TestGarbageCollectorSweepsSupersededAndTombstoned(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qGCListPending: {gcRow("a", "v2", "v1", false), gcRow("b", "v3", "", true)},
	}}
	query.respond = func(name string, args []any) ([][]any, error) {
		if name == qGCGetManifest {
			if args[2] == "a" {
				return [][]any{gcRow("a", "v2", "v1", false)}, nil
			}
			return [][]any{gcRow("b", "v3", "", true)}, nil
		}
		return nil, nil
	}
	var removed []string
	collector := &GarbageCollector{DB: ingestDBFake{query: query}, Index: &fake.KnowledgeVectorIndex{
		RemoveFunc: func(_ context.Context, tenantID int64, kb, version, doc string) error {
			removed = append(removed, doc+"@"+version)
			return nil
		},
	}}
	swept, err := collector.Sweep(context.Background(), 10)
	if err != nil || swept != 2 {
		t.Fatalf("swept = %d, %v", swept, err)
	}
	if len(removed) != 2 || removed[0] != "a@v1" || removed[1] != "b@v3" {
		t.Fatalf("removed = %v", removed)
	}
	if args := query.named(qGCListPending)[0].args; args[0] != 10 {
		t.Fatalf("list args = %v", args)
	}
	chunks := query.named(qGCDeleteChunks)
	docs := query.named(qGCDeleteDocument)
	if len(chunks) != 2 || len(docs) != 2 || chunks[0].args[2] != "v1" || chunks[0].args[3] != "a" || docs[1].args[2] != "v3" || docs[1].args[3] != "b" {
		t.Fatalf("deletes = %v / %v", chunks, docs)
	}
	if len(query.named(qGCClearPending)) != 1 || len(query.named(qGCDeleteManifest)) != 1 || query.commits != 2 {
		t.Fatalf("finalizers: clear %d delete %d commits %d", len(query.named(qGCClearPending)), len(query.named(qGCDeleteManifest)), query.commits)
	}
	// Index removal precedes every row delete inside the transaction.
	names := query.names()
	for i, name := range names {
		if name == qGCDeleteChunks && names[i-1] != qGCGetManifest && names[i-1] != qGCDeleteDocument {
			t.Fatalf("delete outside locked transaction: %v", names)
		}
	}
}

func TestGarbageCollectorSkipsChangedManifestAndReportsFailures(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{
		qGCListPending: {gcRow("a", "v2", "v1", false), gcRow("b", "v2", "v1", false)},
	}}
	query.respond = func(name string, args []any) ([][]any, error) {
		if name == qGCGetManifest && args[2] == "a" {
			// Re-activated meanwhile: v3 active, v2 superseded; v1 vectors were removed but rows of v2 must survive.
			return [][]any{gcRow("a", "v3", "v2", false)}, nil
		}
		if name == qGCGetManifest {
			return [][]any{gcRow("b", "v2", "v1", false)}, nil
		}
		return nil, nil
	}
	collector := &GarbageCollector{DB: ingestDBFake{query: query}, Index: &fake.KnowledgeVectorIndex{
		RemoveFunc: func(_ context.Context, _ int64, _, _, doc string) error {
			if doc == "b" {
				return errors.New("index down")
			}
			return nil
		},
	}}
	swept, err := collector.Sweep(context.Background(), 5)
	if swept != 0 || err == nil || len(query.named(qGCDeleteChunks)) != 0 {
		t.Fatalf("swept = %d, err = %v, deletes = %d", swept, err, len(query.named(qGCDeleteChunks)))
	}
	if _, err := collector.Sweep(context.Background(), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("limit = %v", err)
	}
}
