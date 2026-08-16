package evaluation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func testSigner() *HMACSigner {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return &HMACSigner{KeyID: "gate-2026-03", Key: key}
}

func promotableSummary(manifestID string) domain.EvaluationSummary {
	return domain.EvaluationSummary{
		ManifestID: manifestID, Samples: 40, Promotable: true,
		Deltas: []domain.SliceDelta{{Slice: domain.SliceAll, Metric: domain.MetricCorrectness, Delta: 0.05, CILow: 0.01, CIHigh: 0.09, Samples: 40}},
	}
}

func newIssuer(store *fake.GateDecisionStore, audit *fake.AuditSink) *GateIssuer {
	return &GateIssuer{Signer: testSigner(), Store: store, Audit: audit, TTL: 24 * time.Hour, MaxTelemetryAge: time.Hour, Now: fixedClock(testClock)}
}

func TestGateIssuerSignsStoresAndAuditsDecision(t *testing.T) {
	manifest := testManifest(t, testExample("a"))
	var stored domain.GateDecision
	var audited domain.AuditEvent
	issuer := newIssuer(
		&fake.GateDecisionStore{PutFunc: func(_ context.Context, decision domain.GateDecision) error { stored = decision; return nil }},
		&fake.AuditSink{RecordFunc: func(_ context.Context, event domain.AuditEvent) error { audited = event; return nil }},
	)
	decision, err := issuer.Issue(context.Background(), manifest, promotableSummary(manifest.ManifestID), "platform-9", testClock.Add(-time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Verdict != domain.RolloutHealthy || decision.SignerKeyID != "gate-2026-03" || len(decision.Signature) == 0 {
		t.Fatalf("decision = %+v", decision)
	}
	if !decision.ExpiresAt.Equal(testClock.Add(24 * time.Hour)) {
		t.Fatalf("expiry = %s", decision.ExpiresAt)
	}
	if stored.DecisionID != decision.DecisionID || audited.TenantID != manifest.TenantID || audited.Category != auditCategoryGateDecision {
		t.Fatalf("stored = %+v, audited = %+v", stored, audited)
	}
	if err := VerifyDecision(context.Background(), testSigner(), decision); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestGateIssuerVerdictFollowsEvidence(t *testing.T) {
	manifest := testManifest(t, testExample("a"))
	issuer := newIssuer(&fake.GateDecisionStore{}, &fake.AuditSink{})
	base := promotableSummary(manifest.ManifestID)

	regressed := base
	regressed.Promotable, regressed.Reasons, regressed.CriticalFailures = false, []string{"correctness regressed"}, 1
	unhealthy, err := issuer.Issue(context.Background(), manifest, regressed, "p", testClock, nil)
	if err != nil || unhealthy.Verdict != domain.RolloutUnhealthy {
		t.Fatalf("unhealthy = %+v, %v", unhealthy.Verdict, err)
	}

	noSamples := base
	noSamples.Promotable, noSamples.Samples, noSamples.Reasons = false, 0, []string{"samples below minimum"}
	inconclusive, err := issuer.Issue(context.Background(), manifest, noSamples, "p", testClock, nil)
	if err != nil || inconclusive.Verdict != domain.RolloutInconclusive {
		t.Fatalf("inconclusive = %+v, %v", inconclusive.Verdict, err)
	}

	exempted := base
	exempted.Promotable, exempted.Reasons = false, []string{"latency regressed"}
	approved, err := issuer.Issue(context.Background(), manifest, exempted, "p", testClock, []domain.GateApproval{{Approver: "release-manager", Reason: "known cold start", Scope: "latency"}})
	if err != nil || approved.Verdict != domain.RolloutHealthy || len(approved.Exemptions) != 1 {
		t.Fatalf("exempted = %+v, %v", approved, err)
	}
}

func TestGateIssuerRejectsStaleTelemetryAndUnsignedExemptions(t *testing.T) {
	manifest := testManifest(t, testExample("a"))
	issuer := newIssuer(&fake.GateDecisionStore{}, &fake.AuditSink{})
	if _, err := issuer.Issue(context.Background(), manifest, promotableSummary(manifest.ManifestID), "p", testClock.Add(-2*time.Hour), nil); !errors.Is(err, domain.ErrStaleEvidence) {
		t.Fatalf("stale telemetry = %v", err)
	}
	if _, err := issuer.Issue(context.Background(), manifest, promotableSummary(manifest.ManifestID), "p", testClock, []domain.GateApproval{{Reason: "no approver"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unapproved exemption = %v", err)
	}
	other := testManifest(t, testExample("b"))
	if _, err := issuer.Issue(context.Background(), manifest, promotableSummary(other.ManifestID), "p", testClock, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign summary = %v", err)
	}
}

func TestHMACSignerRejectsTamperAndForeignKey(t *testing.T) {
	signer := testSigner()
	ctx := context.Background()
	signature, keyID, err := signer.Sign(ctx, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Verify(ctx, []byte("payload"), signature, keyID); err != nil {
		t.Fatal(err)
	}
	if err := signer.Verify(ctx, []byte("payload!"), signature, keyID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("tampered payload = %v", err)
	}
	if err := signer.Verify(ctx, []byte("payload"), signature, "other-key"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("foreign key = %v", err)
	}
	if _, _, err := (&HMACSigner{KeyID: "k", Key: []byte("short")}).Sign(ctx, nil); err == nil {
		t.Fatal("accepted a short key")
	}
}
