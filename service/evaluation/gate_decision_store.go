package evaluation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qGateInsert = "scout_evaluation_gate_insert"
	qGateLatest = "scout_evaluation_gate_latest"
)

var gateQueries = map[string]string{
	qGateInsert: `
INSERT INTO gate_decision (decision_id, tenant_id, manifest_id, platform_version, dataset_revision, verdict_code, confidence_bp,
                           decision_json, signature_hex, signer_key_id, issued_at, expires_at, telemetry_fresh_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qGateLatest: `
SELECT decision_id, decision_json, signature_hex, signer_key_id
  FROM gate_decision
 WHERE platform_version = ?
 ORDER BY issued_at DESC, decision_id
 LIMIT 1`,
}

// GateDecisionStore persists signed decisions in gate_decision; the row keeps
// the canonical JSON so verification re-reads exactly what was signed.
type GateDecisionStore struct {
	keelStore
}

var _ contract.GateDecisionStore = (*GateDecisionStore)(nil)

// Put inserts a signed decision.
func (store *GateDecisionStore) Put(ctx context.Context, decision domain.GateDecision) error {
	if decision.TenantID <= 0 || !isSHA256Hex(decision.DecisionID) || !isSHA256Hex(decision.ManifestID) || len(decision.Signature) == 0 || strings.TrimSpace(decision.SignerKeyID) == "" {
		return fmt.Errorf("%w: decision needs tenant, content id, manifest id, and signature", domain.ErrValidation)
	}
	if !decision.ExpiresAt.After(decision.IssuedAt) {
		return fmt.Errorf("%w: decision must expire after it is issued", domain.ErrValidation)
	}
	payload, err := DecisionPayload(decision)
	if err != nil {
		return err
	}
	if sha256Hex(payload) != decision.DecisionID {
		return fmt.Errorf("%w: decision id does not match content", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "gate decision store", gateQueries)
	if err != nil {
		return err
	}
	if _, err := qs.Query(ctx, qGateInsert,
		decision.DecisionID, decision.TenantID, decision.ManifestID, nullable(decision.PlatformVersion), decision.DatasetRevision,
		string(decision.Verdict), int64(math.Round(clamp01(decision.Confidence)*10000)), string(payload), hex.EncodeToString(decision.Signature),
		decision.SignerKeyID, decision.IssuedAt.UTC(), decision.ExpiresAt.UTC(), decision.TelemetryFreshAt.UTC(),
	); err != nil {
		return fmt.Errorf("insert gate decision: %w", err)
	}
	return nil
}

// Latest returns the most recently issued decision for a platform build.
func (store *GateDecisionStore) Latest(ctx context.Context, platformVersion string) (domain.GateDecision, error) {
	if strings.TrimSpace(platformVersion) == "" {
		return domain.GateDecision{}, fmt.Errorf("%w: platform version is required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "gate decision store", gateQueries)
	if err != nil {
		return domain.GateDecision{}, err
	}
	result, err := qs.Query(ctx, qGateLatest, platformVersion)
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("latest gate decision: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.GateDecision{}, fmt.Errorf("%w: gate decision for %q", domain.ErrNotFound, platformVersion)
	}
	row := result.Rows[0]
	if len(row) < 4 {
		return domain.GateDecision{}, fmt.Errorf("gate decision row: expected 4 columns, got %d", len(row))
	}
	payload := []byte(common.AsString(row[1]))
	var decision domain.GateDecision
	if err := json.Unmarshal(payload, &decision); err != nil {
		return domain.GateDecision{}, fmt.Errorf("decode gate decision: %w", err)
	}
	signature, err := hex.DecodeString(common.AsString(row[2]))
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("decode gate signature: %w", err)
	}
	decision.DecisionID = common.AsString(row[0])
	decision.Signature = signature
	decision.SignerKeyID = common.AsString(row[3])
	return decision, nil
}
