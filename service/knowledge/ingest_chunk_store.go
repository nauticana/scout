package knowledge

import (
	"bytes"
	"context"
	"errors"
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
	// Storage is bound to the chunk bucket.
	Storage storage.ObjectStorage
	// Scheme prefixes the returned URI so ObjectStorageLoader can resolve it; default "object".
	Scheme string
	// Prefix is an optional key namespace inside the bucket.
	Prefix string
	// ContentType is recorded on the object; default text/plain; charset=utf-8.
	ContentType string
}

var (
	_ contract.KnowledgeChunkStore   = (*ObjectChunkStore)(nil)
	_ contract.KnowledgeChunkDeleter = (*ObjectChunkStore)(nil)
)

// PutChunk uploads the chunk content and returns its URI and content digest.
func (store *ObjectChunkStore) PutChunk(ctx context.Context, chunk domain.KnowledgeChunk) (domain.ObjectRef, error) {
	if store.Storage == nil || strings.TrimSpace(store.Storage.Bucket()) == "" {
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
	if err := store.Storage.PutObject(ctx, key, bytes.NewReader(chunk.Content), contentType, nil); err != nil {
		return domain.ObjectRef{}, fmt.Errorf("upload chunk %s: %w", chunk.ChunkID, err)
	}
	scheme := store.Scheme
	if scheme == "" {
		scheme = "object"
	}
	return domain.ObjectRef{URI: scheme + "://" + store.Storage.Bucket() + "/" + key, Digest: digest}, nil
}

func (store *ObjectChunkStore) key(chunk domain.KnowledgeChunk) string {
	return store.documentPrefix(chunk.TenantContext.TenantID, chunk.KnowledgeBaseID, chunk.KnowledgeVersion, chunk.DocumentID) + strconv.Itoa(chunk.ChunkNo) + "-" + chunk.ChunkID
}

func (store *ObjectChunkStore) documentPrefix(tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) string {
	parts := []string{strconv.FormatInt(tenantID, 10), knowledgeBaseID, knowledgeVersion, documentID}
	if prefix := strings.Trim(store.Prefix, "/"); prefix != "" {
		parts = append([]string{prefix}, parts...)
	}
	return strings.Join(parts, "/") + "/"
}

// DeleteChunks removes every object under the document version's prefix, so
// chunks whose rows are already gone are reclaimed too.
func (store *ObjectChunkStore) DeleteChunks(ctx context.Context, tenantID int64, knowledgeBaseID, knowledgeVersion, documentID string) error {
	if store.Storage == nil {
		return fmt.Errorf("object chunk store: storage is required")
	}
	if tenantID <= 0 || knowledgeBaseID == "" || knowledgeVersion == "" || documentID == "" {
		return fmt.Errorf("%w: chunk deletion needs tenant, knowledge base, version, and document", domain.ErrValidation)
	}
	prefix := store.documentPrefix(tenantID, knowledgeBaseID, knowledgeVersion, documentID)
	keys, err := store.Storage.ListObjects(ctx, prefix, 0)
	if err != nil {
		return fmt.Errorf("list chunks under %s: %w", prefix, err)
	}
	for _, key := range keys {
		if err := store.Storage.DeleteObject(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete chunk %s: %w", key, err)
		}
	}
	return nil
}
