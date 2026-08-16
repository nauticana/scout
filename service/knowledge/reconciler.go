package knowledge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qReconcileActive     = "scout_knowledge_reconcile_active"
	qReconcileOldest     = "scout_knowledge_reconcile_oldest_pending"
	qReconcileOrphans    = "scout_knowledge_reconcile_orphans"
	qReconcileTombstones = "scout_knowledge_reconcile_tombstones"
)

var reconcileQueries = map[string]string{
	qReconcileActive: `SELECT active_version FROM knowledge_base_alias WHERE tenant_id = ? AND knowledge_base_id = ?`,
	qReconcileOldest: `
SELECT MIN(occurred_at) FROM knowledge_source_event
 WHERE tenant_id = ? AND knowledge_base_id = ? AND acked_at IS NULL`,
	// Orphans are published chunks that no manifest points at, actively or as a pending superseded version.
	qReconcileOrphans: `
SELECT COUNT(*) FROM knowledge_chunk chunk
 WHERE chunk.tenant_id = ? AND chunk.knowledge_base_id = ?
   AND NOT EXISTS (SELECT 1 FROM knowledge_document_manifest manifest
                    WHERE manifest.tenant_id = chunk.tenant_id AND manifest.knowledge_base_id = chunk.knowledge_base_id
                      AND manifest.document_id = chunk.document_id
                      AND (manifest.active_version = chunk.knowledge_version
                        OR manifest.gc_pending = TRUE AND manifest.superseded_version = chunk.knowledge_version))`,
	qReconcileTombstones: `
SELECT COUNT(*) FROM knowledge_document_manifest
 WHERE tenant_id = ? AND knowledge_base_id = ? AND tombstoned = TRUE`,
}

// Reconciler reports freshness lag (oldest unacked source event), orphan
// chunks (published rows no manifest tracks), and tombstones awaiting GC.
type Reconciler struct {
	DB  keelport.DatabaseRepository
	Now func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.KnowledgeReconciler = (*Reconciler)(nil)

func (reconciler *Reconciler) now() time.Time {
	if reconciler.Now != nil {
		return reconciler.Now()
	}
	return time.Now()
}

func (reconciler *Reconciler) init(ctx context.Context) error {
	if reconciler.DB == nil {
		return fmt.Errorf("reconciler: database is required")
	}
	reconciler.once.Do(func() { reconciler.qs = reconciler.DB.GetQueryService(ctx, reconcileQueries) })
	if reconciler.qs == nil {
		return fmt.Errorf("reconciler: query service is required")
	}
	return nil
}

// Reconcile computes the freshness report for one knowledge base.
func (reconciler *Reconciler) Reconcile(ctx context.Context, tenantID int64, knowledgeBaseID string) (domain.KnowledgeFreshnessReport, error) {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return domain.KnowledgeFreshnessReport{}, fmt.Errorf("%w: tenant and knowledge base are required", domain.ErrValidation)
	}
	if err := reconciler.init(ctx); err != nil {
		return domain.KnowledgeFreshnessReport{}, err
	}
	report := domain.KnowledgeFreshnessReport{TenantID: tenantID, KnowledgeBaseID: knowledgeBaseID, CheckedAt: reconciler.now()}
	active, err := reconciler.qs.Query(ctx, qReconcileActive, tenantID, knowledgeBaseID)
	if err != nil {
		return domain.KnowledgeFreshnessReport{}, fmt.Errorf("reconcile: active version: %w", err)
	}
	if len(active.Rows) > 0 {
		report.ActiveVersion = common.AsString(active.Rows[0][0])
	}
	oldest, err := reconciler.qs.Query(ctx, qReconcileOldest, tenantID, knowledgeBaseID)
	if err != nil {
		return domain.KnowledgeFreshnessReport{}, fmt.Errorf("reconcile: oldest pending event: %w", err)
	}
	if len(oldest.Rows) > 0 && oldest.Rows[0][0] != nil {
		if occurredAt, ok := common.AsTimeOK(oldest.Rows[0][0]); ok {
			report.FreshnessLag = report.CheckedAt.Sub(occurredAt)
			if report.FreshnessLag < 0 {
				report.FreshnessLag = 0
			}
		}
	}
	orphans, err := reconciler.qs.Query(ctx, qReconcileOrphans, tenantID, knowledgeBaseID)
	if err != nil {
		return domain.KnowledgeFreshnessReport{}, fmt.Errorf("reconcile: orphan chunks: %w", err)
	}
	if len(orphans.Rows) > 0 {
		report.OrphanChunks = int(common.AsInt64(orphans.Rows[0][0]))
	}
	tombstones, err := reconciler.qs.Query(ctx, qReconcileTombstones, tenantID, knowledgeBaseID)
	if err != nil {
		return domain.KnowledgeFreshnessReport{}, fmt.Errorf("reconcile: tombstones: %w", err)
	}
	if len(tombstones.Rows) > 0 {
		report.Tombstones = int(common.AsInt64(tombstones.Rows[0][0]))
	}
	return report, nil
}
