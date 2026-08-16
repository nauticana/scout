package knowledge

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qManifestLock           = "scout_knowledge_manifest_lock"
	qManifestGet            = "scout_knowledge_manifest_get"
	qManifestInsert         = "scout_knowledge_manifest_insert"
	qManifestSwitch         = "scout_knowledge_manifest_switch"
	qManifestTombstone      = "scout_knowledge_manifest_tombstone"
	qManifestListSuperseded = "scout_knowledge_manifest_list_superseded"
)

const manifestColumns = `
SELECT tenant_id, knowledge_base_id, document_id, active_version, source_version, content_digest, chunker_version,
       tombstoned, tombstoned_at, activated_at, superseded_version, gc_pending,
       (SELECT COUNT(*) FROM knowledge_chunk chunk
         WHERE chunk.tenant_id = manifest.tenant_id AND chunk.knowledge_base_id = manifest.knowledge_base_id
           AND chunk.document_id = manifest.document_id AND chunk.knowledge_version = manifest.superseded_version)
  FROM knowledge_document_manifest manifest`

var manifestQueries = map[string]string{
	qManifestLock: "SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
	qManifestGet:  manifestColumns + ` WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ?`,
	qManifestInsert: `
INSERT INTO knowledge_document_manifest (tenant_id, knowledge_base_id, document_id, active_version, source_version, content_digest, chunker_version,
                                         tombstoned, tombstoned_at, activated_at, superseded_version, gc_pending)
VALUES (?, ?, ?, ?, ?, ?, ?, FALSE, NULL, CURRENT_TIMESTAMP, NULL, FALSE)`,
	qManifestSwitch: `
UPDATE knowledge_document_manifest
   SET superseded_version = active_version, gc_pending = TRUE, active_version = ?, source_version = ?, content_digest = ?, chunker_version = ?,
       tombstoned = FALSE, tombstoned_at = NULL, activated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ?`,
	qManifestTombstone: `
UPDATE knowledge_document_manifest
   SET tombstoned = TRUE, tombstoned_at = CURRENT_TIMESTAMP, gc_pending = TRUE
 WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ? AND tombstoned = FALSE
RETURNING document_id`,
	qManifestListSuperseded: manifestColumns + `
 WHERE tenant_id = ? AND knowledge_base_id = ? AND gc_pending = TRUE
 ORDER BY activated_at, document_id
 LIMIT ?`,
}

// ManifestStore owns the per-document active-version pointer over
// knowledge_document_manifest: Activate switches the pointer to a fully
// published version and marks the previous one for garbage collection.
type ManifestStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.KnowledgeManifestStore = (*ManifestStore)(nil)

func (store *ManifestStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("manifest store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, manifestQueries) })
	if store.qs == nil {
		return fmt.Errorf("manifest store: query service is required")
	}
	return nil
}

func manifestLockKey(tenantID int64, knowledgeBaseID, documentID string) string {
	return fmt.Sprintf("knowledge_manifest|%d|%s|%s", tenantID, knowledgeBaseID, documentID)
}

// Activate points the document at manifest.ActiveVersion; the previous version
// is reported and marked superseded. Re-activating the current version and
// digest is a no-op replay. A superseded or tombstoned version still awaiting
// GC blocks the switch with ErrConflict so no chunk set is orphaned untracked.
func (store *ManifestStore) Activate(ctx context.Context, manifest domain.KnowledgeDocumentManifest) (string, error) {
	if manifest.TenantID <= 0 || strings.TrimSpace(manifest.KnowledgeBaseID) == "" || strings.TrimSpace(manifest.DocumentID) == "" ||
		strings.TrimSpace(manifest.ActiveVersion) == "" || !isSHA256Hex(manifest.ContentDigest) {
		return "", fmt.Errorf("%w: manifest identity, active version, and content digest are required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return "", err
	}
	tx, err := store.DB.BeginTx(ctx, manifestQueries)
	if err != nil {
		return "", fmt.Errorf("activate manifest: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	if _, err = tx.Query(ctx, qManifestLock, manifestLockKey(manifest.TenantID, manifest.KnowledgeBaseID, manifest.DocumentID)); err != nil {
		return "", fmt.Errorf("activate manifest: lock: %w", err)
	}
	current, err := tx.Query(ctx, qManifestGet, manifest.TenantID, manifest.KnowledgeBaseID, manifest.DocumentID)
	if err != nil {
		return "", fmt.Errorf("activate manifest: find: %w", err)
	}
	previous := ""
	switch {
	case len(current.Rows) == 0:
		if _, err = tx.Query(ctx, qManifestInsert, manifest.TenantID, manifest.KnowledgeBaseID, manifest.DocumentID,
			manifest.ActiveVersion, manifest.SourceVersion, manifest.ContentDigest, manifest.ChunkerVersion); err != nil {
			return "", fmt.Errorf("activate manifest: insert: %w", err)
		}
	default:
		existing, superseded, gcPending, err := decodeManifestRow(current.Rows[0])
		if err != nil {
			return "", fmt.Errorf("activate manifest: decode: %w", err)
		}
		if existing.ActiveVersion == manifest.ActiveVersion {
			if existing.Tombstoned {
				return "", fmt.Errorf("%w: document %q version %q is tombstoned and awaits garbage collection", domain.ErrConflict, manifest.DocumentID, manifest.ActiveVersion)
			}
			if existing.ContentDigest != manifest.ContentDigest {
				return "", fmt.Errorf("%w: document %q is active in version %q with different content", domain.ErrConflict, manifest.DocumentID, manifest.ActiveVersion)
			}
			if err = tx.Commit(ctx); err != nil {
				return "", fmt.Errorf("activate manifest: commit replay: %w", err)
			}
			committed = true
			return "", nil
		}
		if gcPending && superseded != "" && superseded != manifest.ActiveVersion {
			return "", fmt.Errorf("%w: document %q still has superseded version %q awaiting garbage collection", domain.ErrConflict, manifest.DocumentID, superseded)
		}
		if _, err = tx.Query(ctx, qManifestSwitch, manifest.ActiveVersion, manifest.SourceVersion, manifest.ContentDigest, manifest.ChunkerVersion,
			manifest.TenantID, manifest.KnowledgeBaseID, manifest.DocumentID); err != nil {
			return "", fmt.Errorf("activate manifest: switch: %w", err)
		}
		previous = existing.ActiveVersion
	}
	if err = tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("activate manifest: commit: %w", err)
	}
	committed = true
	return previous, nil
}

// Tombstone marks the document deleted; retrieval excludes it immediately and GC reclaims its chunks later. Idempotent.
func (store *ManifestStore) Tombstone(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) error {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("%w: tenant, knowledge base, and document are required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	result, err := store.qs.Query(ctx, qManifestTombstone, tenantID, knowledgeBaseID, documentID)
	if err != nil {
		return fmt.Errorf("tombstone document %q: %w", documentID, err)
	}
	if len(result.Rows) > 0 {
		return nil
	}
	if _, err = store.Get(ctx, tenantID, knowledgeBaseID, documentID); err != nil {
		return err
	}
	return nil
}

// Get returns the manifest or ErrNotFound.
func (store *ManifestStore) Get(ctx context.Context, tenantID int64, knowledgeBaseID, documentID string) (domain.KnowledgeDocumentManifest, error) {
	if err := store.init(ctx); err != nil {
		return domain.KnowledgeDocumentManifest{}, err
	}
	result, err := store.qs.Query(ctx, qManifestGet, tenantID, knowledgeBaseID, documentID)
	if err != nil {
		return domain.KnowledgeDocumentManifest{}, fmt.Errorf("get manifest %q: %w", documentID, err)
	}
	if len(result.Rows) == 0 {
		return domain.KnowledgeDocumentManifest{}, fmt.Errorf("%w: manifest for document %q", domain.ErrNotFound, documentID)
	}
	manifest, _, _, err := decodeManifestRow(result.Rows[0])
	if err != nil {
		return domain.KnowledgeDocumentManifest{}, fmt.Errorf("get manifest %q: %w", documentID, err)
	}
	return manifest, nil
}

// ListSuperseded returns manifests with chunks awaiting garbage collection, oldest activation first.
func (store *ManifestStore) ListSuperseded(ctx context.Context, tenantID int64, knowledgeBaseID string, limit int) ([]domain.KnowledgeDocumentManifest, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	result, err := store.qs.Query(ctx, qManifestListSuperseded, tenantID, knowledgeBaseID, limit)
	if err != nil {
		return nil, fmt.Errorf("list superseded manifests: %w", err)
	}
	manifests := make([]domain.KnowledgeDocumentManifest, 0, len(result.Rows))
	for _, row := range result.Rows {
		manifest, _, _, err := decodeManifestRow(row)
		if err != nil {
			return nil, fmt.Errorf("list superseded manifests: %w", err)
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}

func decodeManifestRow(row []any) (manifest domain.KnowledgeDocumentManifest, superseded string, gcPending bool, err error) {
	if len(row) < 13 {
		return manifest, "", false, fmt.Errorf("expected 13 columns, got %d", len(row))
	}
	manifest = domain.KnowledgeDocumentManifest{
		TenantID: common.AsInt64(row[0]), KnowledgeBaseID: common.AsString(row[1]), DocumentID: common.AsString(row[2]),
		ActiveVersion: common.AsString(row[3]), SourceVersion: common.AsString(row[4]), ContentDigest: common.AsString(row[5]),
		ChunkerVersion: common.AsString(row[6]), Tombstoned: common.AsBool(row[7]),
		ActivatedAt: common.AsTime(row[9]), SupersededChunks: int(common.AsInt64(row[12])),
	}
	if row[8] != nil {
		manifest.TombstonedAt = common.AsTime(row[8])
	}
	return manifest, common.AsString(row[10]), common.AsBool(row[11]), nil
}
