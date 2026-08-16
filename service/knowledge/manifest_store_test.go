package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func manifestRow(active, superseded string, tombstoned, gcPending bool, chunks int64) []any {
	var supersededValue any
	if superseded != "" {
		supersededValue = superseded
	}
	return []any{int64(7), "kb", "doc", active, "s1", testDigest, "section/v1", tombstoned, nil, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), supersededValue, gcPending, chunks}
}

func testManifest(version string) domain.KnowledgeDocumentManifest {
	return domain.KnowledgeDocumentManifest{TenantID: 7, KnowledgeBaseID: "kb", DocumentID: "doc", ActiveVersion: version, SourceVersion: "s2", ContentDigest: testDigest, ChunkerVersion: "section/v1"}
}

func TestManifestStoreActivateInsertsSwitchesAndReplays(t *testing.T) {
	query := &ingestQueryFake{}
	store := &ManifestStore{DB: ingestDBFake{query: query}}
	previous, err := store.Activate(context.Background(), testManifest("v1"))
	if err != nil || previous != "" {
		t.Fatalf("insert = %q, %v", previous, err)
	}
	insert := query.named(qManifestInsert)
	if len(insert) != 1 || insert[0].args[0] != int64(7) || insert[0].args[2] != "doc" || insert[0].args[3] != "v1" || insert[0].args[4] != "s2" || insert[0].args[5] != testDigest || insert[0].args[6] != "section/v1" {
		t.Fatalf("insert args = %v", insert)
	}
	if names := query.names(); names[0] != qManifestLock || names[1] != qManifestGet || query.commits != 1 {
		t.Fatalf("order = %v commits = %d", names, query.commits)
	}

	query = &ingestQueryFake{rows: map[string][][]any{qManifestGet: {manifestRow("v1", "", false, false, 0)}}}
	store = &ManifestStore{DB: ingestDBFake{query: query}}
	previous, err = store.Activate(context.Background(), testManifest("v2"))
	if err != nil || previous != "v1" {
		t.Fatalf("switch = %q, %v", previous, err)
	}
	sw := query.named(qManifestSwitch)
	if len(sw) != 1 || sw[0].args[0] != "v2" || sw[0].args[1] != "s2" || sw[0].args[2] != testDigest || sw[0].args[3] != "section/v1" || sw[0].args[4] != int64(7) || sw[0].args[5] != "kb" || sw[0].args[6] != "doc" {
		t.Fatalf("switch args = %v", sw)
	}

	replay, err := store.Activate(context.Background(), testManifest("v1"))
	if err != nil || replay != "" || len(query.named(qManifestSwitch)) != 1 {
		t.Fatalf("replay = %q, %v", replay, err)
	}
	changed := testManifest("v1")
	changed.ContentDigest = "ffff" + testDigest[4:]
	if _, err := store.Activate(context.Background(), changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed digest = %v", err)
	}
}

func TestManifestStoreActivateBlocksUntilGC(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{qManifestGet: {manifestRow("v2", "v1", false, true, 3)}}}
	store := &ManifestStore{DB: ingestDBFake{query: query}}
	if _, err := store.Activate(context.Background(), testManifest("v3")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("pending superseded = %v", err)
	}
	query.rows[qManifestGet] = [][]any{manifestRow("v2", "", true, true, 0)}
	if _, err := store.Activate(context.Background(), testManifest("v2")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tombstoned same version = %v", err)
	}
	previous, err := store.Activate(context.Background(), testManifest("v3"))
	if err != nil || previous != "v2" {
		t.Fatalf("resurrect = %q, %v", previous, err)
	}
	if _, err := store.Activate(context.Background(), domain.KnowledgeDocumentManifest{TenantID: 7}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
}

func TestManifestStoreTombstoneGetList(t *testing.T) {
	query := &ingestQueryFake{rows: map[string][][]any{qManifestTombstone: {{"doc"}}}}
	store := &ManifestStore{DB: ingestDBFake{query: query}}
	if err := store.Tombstone(context.Background(), 7, "kb", "doc"); err != nil {
		t.Fatal(err)
	}
	if args := query.named(qManifestTombstone)[0].args; args[0] != int64(7) || args[1] != "kb" || args[2] != "doc" {
		t.Fatalf("tombstone args = %v", args)
	}
	query.rows[qManifestTombstone] = nil
	if err := store.Tombstone(context.Background(), 7, "kb", "doc"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	query.rows[qManifestGet] = [][]any{manifestRow("v2", "v1", true, true, 5)}
	if err := store.Tombstone(context.Background(), 7, "kb", "doc"); err != nil {
		t.Fatalf("already tombstoned = %v", err)
	}
	manifest, err := store.Get(context.Background(), 7, "kb", "doc")
	if err != nil || manifest.ActiveVersion != "v2" || !manifest.Tombstoned || manifest.SupersededChunks != 5 || manifest.ActivatedAt.IsZero() {
		t.Fatalf("get = %+v, %v", manifest, err)
	}
	query.rows[qManifestListSuperseded] = [][]any{manifestRow("v2", "v1", false, true, 2)}
	list, err := store.ListSuperseded(context.Background(), 7, "kb", 10)
	if err != nil || len(list) != 1 || list[0].SupersededChunks != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if args := query.named(qManifestListSuperseded)[0].args; args[0] != int64(7) || args[1] != "kb" || args[2] != 10 {
		t.Fatalf("list args = %v", args)
	}
	if _, err := store.ListSuperseded(context.Background(), 7, "kb", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("limit = %v", err)
	}
}
