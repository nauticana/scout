package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ObjectChunkStore writes chunk content to keel object storage under a
// deterministic tenant-scoped key and returns the reference the relational
// row commits; the same chunk always lands on the same key, so replays are safe.
type ObjectChunkStore struct {
	Storage storage.ObjectStorage
	Bucket  string
	// Scheme prefixes the returned URI so ObjectStorageLoader can resolve it; default "object".
	Scheme string
	// Prefix is an optional key namespace inside the bucket.
	Prefix string
	// ContentType is recorded on the object; default text/plain; charset=utf-8.
	ContentType string
}

var _ contract.KnowledgeChunkStore = (*ObjectChunkStore)(nil)

// PutChunk uploads the chunk content and returns its URI and content digest.
func (store *ObjectChunkStore) PutChunk(ctx context.Context, chunk domain.KnowledgeChunk) (domain.ObjectRef, error) {
	if store.Storage == nil || strings.TrimSpace(store.Bucket) == "" {
		return domain.ObjectRef{}, fmt.Errorf("object chunk store: storage and bucket are required")
	}
	if chunk.TenantContext.TenantID <= 0 || chunk.KnowledgeBaseID == "" || chunk.KnowledgeVersion == "" || chunk.DocumentID == "" || chunk.ChunkID == "" || len(chunk.Content) == 0 {
		return domain.ObjectRef{}, fmt.Errorf("%w: chunk identity and content are required", domain.ErrValidation)
	}
	digest := chunk.ContentDigest
	if digest == "" {
		digest = sha256Bytes(chunk.Content)
	}
	key := store.key(chunk)
	contentType := store.ContentType
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	if err := store.Storage.Upload(ctx, store.Bucket, key, bytes.NewReader(chunk.Content), contentType); err != nil {
		return domain.ObjectRef{}, fmt.Errorf("upload chunk %s: %w", chunk.ChunkID, err)
	}
	scheme := store.Scheme
	if scheme == "" {
		scheme = "object"
	}
	return domain.ObjectRef{URI: scheme + "://" + store.Bucket + "/" + key, Digest: digest}, nil
}

func (store *ObjectChunkStore) key(chunk domain.KnowledgeChunk) string {
	parts := []string{strconv.FormatInt(chunk.TenantContext.TenantID, 10), chunk.KnowledgeBaseID, chunk.KnowledgeVersion, chunk.DocumentID, strconv.Itoa(chunk.ChunkNo) + "-" + chunk.ChunkID}
	if prefix := strings.Trim(store.Prefix, "/"); prefix != "" {
		parts = append([]string{prefix}, parts...)
	}
	return strings.Join(parts, "/")
}
