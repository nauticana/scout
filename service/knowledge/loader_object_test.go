package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestObjectStorageLoaderAndChunkStoreRoundTrip(t *testing.T) {
	storage := &fake.ObjectStorage{Name: "chunks"}
	store := &ObjectChunkStore{Storage: storage, Prefix: "/kb/"}
	chunk := domain.KnowledgeChunk{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: "v1", DocumentID: "d", ChunkNo: 3, ChunkID: "cid", Content: []byte("hello")}
	ref, err := store.PutChunk(context.Background(), chunk)
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI != "object://chunks/kb/7/kb/v1/d/3-cid" || ref.Digest != sha256Bytes([]byte("hello")) {
		t.Fatalf("ref = %+v", ref)
	}
	loader := &ObjectStorageLoader{Storage: storage, Schemes: []string{"object"}}
	raw, err := loader.Load(context.Background(), domain.KnowledgeDocument{SourceURI: ref.URI})
	if err != nil || string(raw) != "hello" {
		t.Fatalf("load = %q, %v", raw, err)
	}
	if _, err := loader.Load(context.Background(), domain.KnowledgeDocument{SourceURI: "s3://chunks/kb/7/kb/v1/d/3-cid"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("scheme = %v", err)
	}
	if _, err := loader.Load(context.Background(), domain.KnowledgeDocument{SourceURI: "object://other/kb/7/kb/v1/d/3-cid"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("other bucket = %v", err)
	}
	if _, err := loader.Load(context.Background(), domain.KnowledgeDocument{SourceURI: "nonsense"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("uri = %v", err)
	}
	small := &ObjectStorageLoader{Storage: storage, MaxBytes: 3}
	if _, err := small.Load(context.Background(), domain.KnowledgeDocument{SourceURI: ref.URI}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("max bytes = %v", err)
	}
	if _, err := store.PutChunk(context.Background(), domain.KnowledgeChunk{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty chunk = %v", err)
	}
	if _, err := loader.Load(context.Background(), domain.KnowledgeDocument{SourceURI: "object://chunks/missing"}); err == nil {
		t.Fatal("missing object loaded")
	}
}

func TestObjectChunkStoreDeletesOneDocumentVersionsObjects(t *testing.T) {
	storage := &fake.ObjectStorage{Name: "chunks"}
	store := &ObjectChunkStore{Storage: storage, Prefix: "kb"}
	put := func(version, doc string, no int) {
		if _, err := store.PutChunk(context.Background(), domain.KnowledgeChunk{TenantContext: domain.TenantContext{TenantID: 7}, KnowledgeBaseID: "kb", KnowledgeVersion: version, DocumentID: doc, ChunkNo: no, ChunkID: "c", Content: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	put("v1", "d", 0)
	put("v1", "d", 1)
	put("v1", "d2", 0)
	put("v2", "d", 0)
	if err := store.DeleteChunks(context.Background(), 7, "kb", "v1", "d"); err != nil {
		t.Fatal(err)
	}
	if len(storage.Objects) != 2 || len(storage.Deletes) != 2 {
		t.Fatalf("objects left = %v, deleted = %v", storage.Objects, storage.Deletes)
	}
	if err := store.DeleteChunks(context.Background(), 7, "kb", "v1", "d"); err != nil {
		t.Fatalf("deleting nothing = %v", err)
	}
	if err := store.DeleteChunks(context.Background(), 7, "kb", "", "d"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("without a version = %v", err)
	}
}
