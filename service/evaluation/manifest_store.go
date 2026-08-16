package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qManifestGet    = "scout_evaluation_manifest_get"
	qManifestInsert = "scout_evaluation_manifest_insert"
)

var manifestQueries = map[string]string{
	qManifestGet: `
SELECT manifest_json
  FROM evaluation_manifest
 WHERE tenant_id = ? AND manifest_id = ?`,
	qManifestInsert: `
INSERT INTO evaluation_manifest (manifest_id, tenant_id, agent_id, candidate_agent_version, baseline_agent_version,
                                 golden_set_id, golden_set_version, dataset_revision, safety_policy_version, manifest_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
}

// ManifestStore persists immutable manifests in evaluation_manifest.
type ManifestStore struct {
	keelStore
}

var _ contract.EvaluationManifestStore = (*ManifestStore)(nil)

// Put inserts a verified manifest; a byte-identical replay is a no-op, a different body is a conflict.
func (store *ManifestStore) Put(ctx context.Context, manifest domain.EvaluationManifest) error {
	if err := (&ManifestBuilder{}).Verify(manifest); err != nil {
		return err
	}
	encoded, err := canonicalJSON(manifest)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	tx, err := store.begin(ctx, "manifest store", manifestQueries)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	existing, err := tx.Query(ctx, qManifestGet, manifest.TenantID, manifest.ManifestID)
	if err != nil {
		return fmt.Errorf("find manifest: %w", err)
	}
	if len(existing.Rows) > 0 {
		if common.AsString(existing.Rows[0][0]) != string(encoded) {
			return fmt.Errorf("%w: manifest %q already stored with different content", domain.ErrConflict, manifest.ManifestID)
		}
	} else if _, err = tx.Query(ctx, qManifestInsert,
		manifest.ManifestID, manifest.TenantID, manifest.Candidate.AgentID, manifest.Candidate.AgentVersion, manifest.Baseline.AgentVersion,
		manifest.GoldenSetID, manifest.GoldenSetVersion, manifest.DatasetRevision, manifest.SafetyPolicyVersion, string(encoded), manifest.CreatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("insert manifest: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit manifest: %w", err)
	}
	committed = true
	return nil
}

// Get returns the stored manifest after re-verifying its content id.
func (store *ManifestStore) Get(ctx context.Context, tenantID int64, manifestID string) (domain.EvaluationManifest, error) {
	if tenantID <= 0 || strings.TrimSpace(manifestID) == "" {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: tenant and manifest id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "manifest store", manifestQueries)
	if err != nil {
		return domain.EvaluationManifest{}, err
	}
	result, err := qs.Query(ctx, qManifestGet, tenantID, manifestID)
	if err != nil {
		return domain.EvaluationManifest{}, fmt.Errorf("get manifest: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.EvaluationManifest{}, fmt.Errorf("%w: manifest %q", domain.ErrNotFound, manifestID)
	}
	var manifest domain.EvaluationManifest
	if err := json.Unmarshal([]byte(common.AsString(result.Rows[0][0])), &manifest); err != nil {
		return domain.EvaluationManifest{}, fmt.Errorf("decode manifest %q: %w", manifestID, err)
	}
	if err := (&ManifestBuilder{}).Verify(manifest); err != nil {
		return domain.EvaluationManifest{}, fmt.Errorf("stored manifest %q: %w", manifestID, err)
	}
	return manifest, nil
}
