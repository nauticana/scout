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
	qAliasLock            = "scout_knowledge_alias_lock"
	qAliasGet             = "scout_knowledge_alias_get"
	qAliasInsert          = "scout_knowledge_alias_insert"
	qAliasSwap            = "scout_knowledge_alias_swap"
	qAliasRepointManifest = "scout_knowledge_alias_repoint_manifests"
)

var aliasQueries = map[string]string{
	qAliasLock: "SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
	qAliasGet:  `SELECT active_version FROM knowledge_base_alias WHERE tenant_id = ? AND knowledge_base_id = ?`,
	qAliasInsert: `
INSERT INTO knowledge_base_alias (tenant_id, knowledge_base_id, active_version, previous_version, swapped_at)
VALUES (?, ?, ?, NULL, CURRENT_TIMESTAMP)`,
	qAliasSwap: `
UPDATE knowledge_base_alias
   SET previous_version = active_version, active_version = ?, swapped_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND knowledge_base_id = ? AND active_version = ?
RETURNING previous_version`,
	// Documents rebuilt into the new generation follow the alias in the same transaction; the rest keep their pointer.
	qAliasRepointManifest: `
UPDATE knowledge_document_manifest manifest
   SET superseded_version = manifest.active_version, gc_pending = TRUE, active_version = ?, activated_at = CURRENT_TIMESTAMP
 WHERE manifest.tenant_id = ? AND manifest.knowledge_base_id = ? AND manifest.tombstoned = FALSE AND manifest.gc_pending = FALSE
   AND manifest.active_version <> ?
   AND EXISTS (SELECT 1 FROM knowledge_document doc
                WHERE doc.tenant_id = manifest.tenant_id AND doc.knowledge_base_id = manifest.knowledge_base_id
                  AND doc.knowledge_version = ? AND doc.document_id = manifest.document_id)`,
}

// VersionAliaser is the KB-level generation pointer over knowledge_base_alias.
// Swap is a compare-and-set: the alias moves only from the expected version,
// and every document already published in the new generation is repointed in
// the same transaction so retrieval never mixes embedding generations.
type VersionAliaser struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.KnowledgeVersionAliaser = (*VersionAliaser)(nil)

func (aliaser *VersionAliaser) init(ctx context.Context) error {
	if aliaser.DB == nil {
		return fmt.Errorf("version aliaser: database is required")
	}
	aliaser.once.Do(func() { aliaser.qs = aliaser.DB.GetQueryService(ctx, aliasQueries) })
	if aliaser.qs == nil {
		return fmt.Errorf("version aliaser: query service is required")
	}
	return nil
}

// Swap makes newVersion active when the alias currently is expectedVersion (empty = no alias yet); otherwise ErrConflict.
func (aliaser *VersionAliaser) Swap(ctx context.Context, tenantID int64, knowledgeBaseID, expectedVersion, newVersion string) error {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(newVersion) == "" {
		return fmt.Errorf("%w: tenant, knowledge base, and new version are required", domain.ErrValidation)
	}
	if expectedVersion == newVersion {
		return fmt.Errorf("%w: new version must differ from the expected version", domain.ErrValidation)
	}
	if err := aliaser.init(ctx); err != nil {
		return err
	}
	tx, err := aliaser.DB.BeginTx(ctx, aliasQueries)
	if err != nil {
		return fmt.Errorf("swap alias: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	if _, err = tx.Query(ctx, qAliasLock, fmt.Sprintf("knowledge_alias|%d|%s", tenantID, knowledgeBaseID)); err != nil {
		return fmt.Errorf("swap alias: lock: %w", err)
	}
	current, err := tx.Query(ctx, qAliasGet, tenantID, knowledgeBaseID)
	if err != nil {
		return fmt.Errorf("swap alias: find: %w", err)
	}
	switch {
	case len(current.Rows) == 0 && expectedVersion == "":
		if _, err = tx.Query(ctx, qAliasInsert, tenantID, knowledgeBaseID, newVersion); err != nil {
			return fmt.Errorf("swap alias: insert: %w", err)
		}
	case len(current.Rows) == 0:
		return fmt.Errorf("%w: knowledge base %q has no alias, expected version %q", domain.ErrConflict, knowledgeBaseID, expectedVersion)
	default:
		if active := common.AsString(current.Rows[0][0]); active != expectedVersion {
			return fmt.Errorf("%w: knowledge base %q is at version %q, expected %q", domain.ErrConflict, knowledgeBaseID, active, expectedVersion)
		}
		swapped, err := tx.Query(ctx, qAliasSwap, newVersion, tenantID, knowledgeBaseID, expectedVersion)
		if err != nil {
			return fmt.Errorf("swap alias: %w", err)
		}
		if len(swapped.Rows) == 0 {
			return fmt.Errorf("%w: knowledge base %q alias changed concurrently", domain.ErrConflict, knowledgeBaseID)
		}
	}
	if _, err = tx.Query(ctx, qAliasRepointManifest, newVersion, tenantID, knowledgeBaseID, newVersion, newVersion); err != nil {
		return fmt.Errorf("swap alias: repoint manifests: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("swap alias: commit: %w", err)
	}
	committed = true
	return nil
}

// Active returns the generation retrieval must read, or ErrNotFound before the first swap.
func (aliaser *VersionAliaser) Active(ctx context.Context, tenantID int64, knowledgeBaseID string) (string, error) {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return "", fmt.Errorf("%w: tenant and knowledge base are required", domain.ErrValidation)
	}
	if err := aliaser.init(ctx); err != nil {
		return "", err
	}
	result, err := aliaser.qs.Query(ctx, qAliasGet, tenantID, knowledgeBaseID)
	if err != nil {
		return "", fmt.Errorf("active alias: %w", err)
	}
	if len(result.Rows) == 0 {
		return "", fmt.Errorf("%w: knowledge base %q has no active version", domain.ErrNotFound, knowledgeBaseID)
	}
	return common.AsString(result.Rows[0][0]), nil
}
