package knowledge

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qGCListPending    = "scout_knowledge_gc_list_pending"
	qGCLock           = "scout_knowledge_gc_lock"
	qGCGetManifest    = "scout_knowledge_gc_get_manifest"
	qGCDeleteChunks   = "scout_knowledge_gc_delete_chunks"
	qGCDeleteDocument = "scout_knowledge_gc_delete_document"
	qGCClearPending   = "scout_knowledge_gc_clear_pending"
	qGCDeleteManifest = "scout_knowledge_gc_delete_manifest"
)

const gcManifestColumns = `
SELECT tenant_id, knowledge_base_id, document_id, active_version, superseded_version, tombstoned, gc_pending
  FROM knowledge_document_manifest`

var gcQueries = map[string]string{
	qGCListPending: gcManifestColumns + `
 WHERE gc_pending = TRUE
 ORDER BY activated_at, tenant_id, knowledge_base_id, document_id
 LIMIT ?`,
	qGCLock:        "SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
	qGCGetManifest: gcManifestColumns + ` WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ?`,
	qGCDeleteChunks: `
DELETE FROM knowledge_chunk
 WHERE tenant_id = ? AND knowledge_base_id = ? AND knowledge_version = ? AND document_id = ?`,
	qGCDeleteDocument: `
DELETE FROM knowledge_document
 WHERE tenant_id = ? AND knowledge_base_id = ? AND knowledge_version = ? AND document_id = ?`,
	qGCClearPending: `
UPDATE knowledge_document_manifest
   SET superseded_version = NULL, gc_pending = tombstoned
 WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ?`,
	qGCDeleteManifest: `
DELETE FROM knowledge_document_manifest
 WHERE tenant_id = ? AND knowledge_base_id = ? AND document_id = ? AND tombstoned = TRUE`,
}

// GarbageCollector reclaims superseded and tombstoned document versions in
// bounded batches: vectors are removed first (idempotent), then chunk,
// document, and manifest rows go in one transaction under the manifest lock,
// so a concurrent activation can never lose a live chunk set.
type GarbageCollector struct {
	DB    keelport.DatabaseRepository
	Index contract.KnowledgeVectorIndex

	once sync.Once
	qs   keelport.QueryService
}

type gcManifest struct {
	tenantID              int64
	knowledgeBaseID       string
	documentID            string
	activeVersion         string
	supersededVersion     string
	tombstoned, gcPending bool
}

// versions lists the document versions whose chunks are reclaimable in this state.
func (manifest gcManifest) versions() []string {
	var versions []string
	if manifest.gcPending && manifest.supersededVersion != "" && manifest.supersededVersion != manifest.activeVersion {
		versions = append(versions, manifest.supersededVersion)
	}
	if manifest.tombstoned {
		versions = append(versions, manifest.activeVersion)
	}
	return versions
}

func decodeGCManifest(row []any) (gcManifest, error) {
	if len(row) < 7 {
		return gcManifest{}, fmt.Errorf("expected 7 columns, got %d", len(row))
	}
	return gcManifest{
		tenantID: common.AsInt64(row[0]), knowledgeBaseID: common.AsString(row[1]), documentID: common.AsString(row[2]),
		activeVersion: common.AsString(row[3]), supersededVersion: common.AsString(row[4]),
		tombstoned: common.AsBool(row[5]), gcPending: common.AsBool(row[6]),
	}, nil
}

func (collector *GarbageCollector) init(ctx context.Context) error {
	if collector.DB == nil || collector.Index == nil {
		return fmt.Errorf("garbage collector: database and index are required")
	}
	collector.once.Do(func() { collector.qs = collector.DB.GetQueryService(ctx, gcQueries) })
	if collector.qs == nil {
		return fmt.Errorf("garbage collector: query service is required")
	}
	return nil
}

// Sweep reclaims up to limit pending manifests and returns how many it finished; run it from a periodic worker.
func (collector *GarbageCollector) Sweep(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	if err := collector.init(ctx); err != nil {
		return 0, err
	}
	pending, err := collector.qs.Query(ctx, qGCListPending, limit)
	if err != nil {
		return 0, fmt.Errorf("gc sweep: list pending: %w", err)
	}
	swept := 0
	var failures error
	for _, row := range pending.Rows {
		if err := ctx.Err(); err != nil {
			return swept, errors.Join(failures, err)
		}
		manifest, err := decodeGCManifest(row)
		if err != nil {
			return swept, errors.Join(failures, fmt.Errorf("gc sweep: decode manifest: %w", err))
		}
		done, err := collector.reclaim(ctx, manifest)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("gc sweep document %q: %w", manifest.documentID, err))
			continue
		}
		if done {
			swept++
		}
	}
	return swept, failures
}

func (collector *GarbageCollector) reclaim(ctx context.Context, snapshot gcManifest) (bool, error) {
	removed := snapshot.versions()
	for _, version := range removed {
		if err := collector.Index.Remove(ctx, snapshot.tenantID, snapshot.knowledgeBaseID, version, snapshot.documentID); err != nil {
			return false, fmt.Errorf("remove version %q from index: %w", version, err)
		}
	}
	tx, err := collector.DB.BeginTx(ctx, gcQueries)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	if _, err = tx.Query(ctx, qGCLock, manifestLockKey(snapshot.tenantID, snapshot.knowledgeBaseID, snapshot.documentID)); err != nil {
		return false, fmt.Errorf("lock: %w", err)
	}
	current, err := tx.Query(ctx, qGCGetManifest, snapshot.tenantID, snapshot.knowledgeBaseID, snapshot.documentID)
	if err != nil {
		return false, fmt.Errorf("reread manifest: %w", err)
	}
	if len(current.Rows) == 0 {
		return false, nil
	}
	manifest, err := decodeGCManifest(current.Rows[0])
	if err != nil {
		return false, err
	}
	// Only versions whose vectors this sweep removed may lose their rows; anything newer waits for the next sweep.
	reclaimable := 0
	deleteManifest := false
	for _, version := range manifest.versions() {
		if !containsFold(removed, version) {
			continue
		}
		reclaimable++
		if _, err = tx.Query(ctx, qGCDeleteChunks, manifest.tenantID, manifest.knowledgeBaseID, version, manifest.documentID); err != nil {
			return false, fmt.Errorf("delete chunks of version %q: %w", version, err)
		}
		if manifest.tombstoned && version == manifest.activeVersion {
			deleteManifest = true
			if _, err = tx.Query(ctx, qGCDeleteManifest, manifest.tenantID, manifest.knowledgeBaseID, manifest.documentID); err != nil {
				return false, fmt.Errorf("delete manifest: %w", err)
			}
		}
		if _, err = tx.Query(ctx, qGCDeleteDocument, manifest.tenantID, manifest.knowledgeBaseID, version, manifest.documentID); err != nil {
			return false, fmt.Errorf("delete document of version %q: %w", version, err)
		}
	}
	if reclaimable == 0 {
		return false, nil
	}
	if !deleteManifest {
		if _, err = tx.Query(ctx, qGCClearPending, manifest.tenantID, manifest.knowledgeBaseID, manifest.documentID); err != nil {
			return false, fmt.Errorf("clear pending: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return reclaimable == len(manifest.versions()), nil
}
