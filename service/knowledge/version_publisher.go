package knowledge

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qVersionLock       = "scout_knowledge_version_lock"
	qVersionGet        = "scout_knowledge_version_get"
	qVersionBaseGet    = "scout_knowledge_version_base_get"
	qVersionBaseInsert = "scout_knowledge_version_base_insert"
	qVersionInsert     = "scout_knowledge_version_insert"
)

var versionQueries = map[string]string{
	qVersionLock: "SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
	qVersionGet: `
SELECT embedding_provider, embedding_model FROM knowledge_base_version
 WHERE tenant_id = ? AND knowledge_base_id = ? AND knowledge_version = ?`,
	qVersionBaseGet: `SELECT 1 FROM knowledge_base WHERE tenant_id = ? AND knowledge_base_id = ?`,
	qVersionBaseInsert: `
INSERT INTO knowledge_base (tenant_id, knowledge_base_id, display_name) VALUES (?, ?, ?)`,
	qVersionInsert: `
INSERT INTO knowledge_base_version (tenant_id, knowledge_base_id, knowledge_version, embedding_provider, embedding_model)
VALUES (?, ?, ?, ?, ?)`,
}

// VersionPublisher creates knowledge_base and knowledge_base_version rows. It
// is the control-plane call before ingestion; the pipeline never creates them.
type VersionPublisher struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.KnowledgeVersionPublisher = (*VersionPublisher)(nil)

func (publisher *VersionPublisher) init(ctx context.Context) error {
	if publisher.DB == nil {
		return fmt.Errorf("version publisher: database is required")
	}
	publisher.once.Do(func() { publisher.qs = publisher.DB.GetQueryService(ctx, versionQueries) })
	if publisher.qs == nil {
		return fmt.Errorf("version publisher: query service is required")
	}
	return nil
}

// EnsureVersion creates the version and, when missing, its knowledge base.
func (publisher *VersionPublisher) EnsureVersion(ctx context.Context, version domain.KnowledgeVersion) error {
	tenantID := version.TenantContext.TenantID
	baseID, versionID := strings.TrimSpace(version.KnowledgeBaseID), strings.TrimSpace(version.KnowledgeVersion)
	if tenantID <= 0 || baseID == "" || versionID == "" {
		return fmt.Errorf("%w: tenant, knowledge base, and version are required", domain.ErrValidation)
	}
	if (version.EmbeddingProvider == "") != (version.EmbeddingModel == "") {
		return fmt.Errorf("%w: embedding provider and model are set together or not at all", domain.ErrValidation)
	}
	if err := publisher.init(ctx); err != nil {
		return err
	}
	tx, err := publisher.DB.BeginTx(ctx, versionQueries)
	if err != nil {
		return fmt.Errorf("ensure knowledge version: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	if _, err = tx.Query(ctx, qVersionLock, fmt.Sprintf("knowledge_base:%d:%s", tenantID, baseID)); err != nil {
		return fmt.Errorf("ensure knowledge version: lock: %w", err)
	}
	existing, err := tx.Query(ctx, qVersionGet, tenantID, baseID, versionID)
	if err != nil {
		return fmt.Errorf("ensure knowledge version: read: %w", err)
	}
	if len(existing.Rows) > 0 {
		if common.AsString(existing.Rows[0][0]) != version.EmbeddingProvider || common.AsString(existing.Rows[0][1]) != version.EmbeddingModel {
			return fmt.Errorf("%w: knowledge version %s@%s exists with another embedding", domain.ErrConflict, baseID, versionID)
		}
		return nil
	}
	base, err := tx.Query(ctx, qVersionBaseGet, tenantID, baseID)
	if err != nil {
		return fmt.Errorf("ensure knowledge version: read base: %w", err)
	}
	if len(base.Rows) == 0 {
		name := strings.TrimSpace(version.DisplayName)
		if name == "" {
			name = baseID
		}
		if _, err = tx.Query(ctx, qVersionBaseInsert, tenantID, baseID, name); err != nil {
			return fmt.Errorf("ensure knowledge version: insert base: %w", err)
		}
	}
	if _, err = tx.Query(ctx, qVersionInsert, tenantID, baseID, versionID, version.EmbeddingProvider, version.EmbeddingModel); err != nil {
		return fmt.Errorf("ensure knowledge version: insert: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("ensure knowledge version: commit: %w", err)
	}
	committed = true
	return nil
}
