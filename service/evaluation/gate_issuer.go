package evaluation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const auditCategoryGateDecision = "evaluation.gate_decision"

// HMACSigner signs gate decisions with HMAC-SHA256 under one named key.
type HMACSigner struct {
	KeyID string
	Key   []byte
}

var _ contract.GateSigner = (*HMACSigner)(nil)

func (signer *HMACSigner) validate() error {
	if strings.TrimSpace(signer.KeyID) == "" || len(signer.Key) < 32 {
		return fmt.Errorf("hmac signer: key id and a key of at least 32 bytes are required")
	}
	return nil
}

// Sign returns the MAC and the key id it was made with.
func (signer *HMACSigner) Sign(_ context.Context, payload []byte) ([]byte, string, error) {
	if err := signer.validate(); err != nil {
		return nil, "", err
	}
	mac := hmac.New(sha256.New, signer.Key)
	mac.Write(payload)
	return mac.Sum(nil), signer.KeyID, nil
}

// Verify rejects a foreign key id or a MAC that does not match the payload.
func (signer *HMACSigner) Verify(_ context.Context, payload, signature []byte, keyID string) error {
	if err := signer.validate(); err != nil {
		return err
	}
	if keyID != signer.KeyID {
		return fmt.Errorf("%w: gate decision signed by unknown key %q", domain.ErrUnauthorized, keyID)
	}
	mac := hmac.New(sha256.New, signer.Key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), signature) {
		return fmt.Errorf("%w: gate decision signature does not match", domain.ErrUnauthorized)
	}
	return nil
}

// GateIssuer turns a paired summary into a signed, expiring, audited gate decision.
type GateIssuer struct {
	Signer contract.GateSigner
	Store  contract.GateDecisionStore
	Audit  contract.AuditSink
	// TTL bounds how long a decision may be consumed.
	TTL time.Duration
	// MaxTelemetryAge rejects summaries whose supporting telemetry is older than this at issue time.
	MaxTelemetryAge time.Duration
	Now             func() time.Time
}

func (issuer *GateIssuer) now() time.Time {
	if issuer.Now != nil {
		return issuer.Now()
	}
	return time.Now()
}

func (issuer *GateIssuer) validate() error {
	if issuer.Signer == nil || issuer.Store == nil || issuer.Audit == nil {
		return fmt.Errorf("gate issuer: signer, store, and audit sink are required")
	}
	if issuer.TTL <= 0 || issuer.MaxTelemetryAge <= 0 {
		return fmt.Errorf("%w: gate issuer TTL and telemetry age must be positive", domain.ErrValidation)
	}
	return nil
}

// Issue signs and stores the decision for a manifest; platformVersion may be empty for agent-only gates.
func (issuer *GateIssuer) Issue(ctx context.Context, manifest domain.EvaluationManifest, summary domain.EvaluationSummary, platformVersion string, telemetryFreshAt time.Time, exemptions []domain.GateApproval) (domain.GateDecision, error) {
	if err := issuer.validate(); err != nil {
		return domain.GateDecision{}, err
	}
	if summary.ManifestID != manifest.ManifestID || manifest.TenantID <= 0 {
		return domain.GateDecision{}, fmt.Errorf("%w: summary does not belong to the manifest", domain.ErrValidation)
	}
	now := issuer.now().UTC()
	if telemetryFreshAt.IsZero() || now.Sub(telemetryFreshAt) > issuer.MaxTelemetryAge {
		return domain.GateDecision{}, fmt.Errorf("%w: supporting telemetry is older than %s", domain.ErrStaleEvidence, issuer.MaxTelemetryAge)
	}
	for _, exemption := range exemptions {
		if strings.TrimSpace(exemption.Approver) == "" || strings.TrimSpace(exemption.Reason) == "" {
			return domain.GateDecision{}, fmt.Errorf("%w: every exemption needs an approver and a reason", domain.ErrValidation)
		}
	}
	decision := domain.GateDecision{
		TenantID:         manifest.TenantID,
		ManifestID:       manifest.ManifestID,
		PlatformVersion:  strings.TrimSpace(platformVersion),
		DatasetRevision:  manifest.DatasetRevision,
		JudgeVersions:    judgeVersions(manifest.Evaluators),
		Deltas:           append([]domain.SliceDelta(nil), summary.Deltas...),
		Confidence:       decisionConfidence(summary),
		Verdict:          verdictOf(summary, exemptions),
		Exemptions:       append([]domain.GateApproval(nil), exemptions...),
		IssuedAt:         now,
		ExpiresAt:        now.Add(issuer.TTL),
		TelemetryFreshAt: telemetryFreshAt.UTC(),
	}
	payload, err := DecisionPayload(decision)
	if err != nil {
		return domain.GateDecision{}, err
	}
	decision.DecisionID = sha256Hex(payload)
	signature, keyID, err := issuer.Signer.Sign(ctx, payload)
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("sign gate decision: %w", err)
	}
	decision.Signature, decision.SignerKeyID = signature, keyID
	if err := issuer.Store.Put(ctx, decision); err != nil {
		return domain.GateDecision{}, fmt.Errorf("store gate decision: %w", err)
	}
	auditPayload, err := json.Marshal(struct {
		DecisionID string                `json:"decision_id"`
		ManifestID string                `json:"manifest_id"`
		Verdict    domain.RolloutVerdict `json:"verdict"`
		Reasons    []string              `json:"reasons,omitempty"`
		Exemptions int                   `json:"exemptions"`
	}{decision.DecisionID, decision.ManifestID, decision.Verdict, summary.Reasons, len(exemptions)})
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("encode gate audit: %w", err)
	}
	if err := issuer.Audit.Record(ctx, domain.AuditEvent{TenantID: manifest.TenantID, Category: auditCategoryGateDecision, Payload: auditPayload, OccurredAt: now}); err != nil {
		return domain.GateDecision{}, fmt.Errorf("audit gate decision: %w", err)
	}
	return decision, nil
}

// VerifyDecision recomputes the canonical payload, checks the id, and verifies the signature.
func VerifyDecision(ctx context.Context, signer contract.GateSigner, decision domain.GateDecision) error {
	payload, err := DecisionPayload(decision)
	if err != nil {
		return err
	}
	if sha256Hex(payload) != decision.DecisionID {
		return fmt.Errorf("%w: gate decision id does not match content", domain.ErrUnauthorized)
	}
	return signer.Verify(ctx, payload, decision.Signature, decision.SignerKeyID)
}

// DecisionPayload is the canonical signed form: every field except id and signature.
func DecisionPayload(decision domain.GateDecision) ([]byte, error) {
	unsigned := decision
	unsigned.DecisionID, unsigned.Signature, unsigned.SignerKeyID = "", nil, ""
	payload, err := canonicalJSON(unsigned)
	if err != nil {
		return nil, fmt.Errorf("canonical gate decision: %w", err)
	}
	return payload, nil
}

func judgeVersions(evaluators []domain.EvaluatorVersion) []domain.EvaluatorVersion {
	var judges []domain.EvaluatorVersion
	for _, evaluator := range evaluators {
		if evaluator.Kind == evaluatorKindJudge {
			judges = append(judges, evaluator)
		}
	}
	return judges
}

// verdictOf maps evidence to a rollout verdict: too little evidence is
// inconclusive, a failed policy is unhealthy unless every reason is exempted.
func verdictOf(summary domain.EvaluationSummary, exemptions []domain.GateApproval) domain.RolloutVerdict {
	if summary.Promotable {
		return domain.RolloutHealthy
	}
	if summary.Samples == 0 || summary.HumanReviewPending > 0 && summary.CriticalFailures == 0 && len(summary.Reasons) == 0 {
		return domain.RolloutInconclusive
	}
	if len(summary.Reasons) > 0 && len(exemptions) >= len(summary.Reasons) && summary.CriticalFailures == 0 {
		return domain.RolloutHealthy
	}
	return domain.RolloutUnhealthy
}

// decisionConfidence is the narrowest relative CI among aggregate deltas, in [0,1].
func decisionConfidence(summary domain.EvaluationSummary) float64 {
	confidence := 0.0
	for _, delta := range summary.Deltas {
		if delta.Slice != domain.SliceAll || delta.Samples == 0 {
			continue
		}
		width := delta.CIHigh - delta.CILow
		value := clamp01(1 - width)
		if value > confidence {
			confidence = value
		}
	}
	return confidence
}
