package evaluation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func samplingPolicy() domain.SamplingPolicy {
	return domain.SamplingPolicy{TenantID: 7, BaseRate: 0.1, RiskRate: 1, UncertaintyRate: 0.5, MaxPerWindow: 2, Window: time.Hour,
		RedactionRequired: true, ResidencyRegion: "eu-central", RetentionClass: "short", Retention: 24 * time.Hour}
}

func testSignal() domain.SampleSignal {
	return domain.SampleSignal{TenantContext: domain.TenantContext{TenantID: 7, Region: "eu-central"}, RequestID: "req-1", AgentID: "agent", AgentVersion: "v2", Redacted: true}
}

func newEnforcer(policy domain.SamplingPolicy, draw float64) *SamplingPolicyEnforcer {
	return &SamplingPolicyEnforcer{
		Policies: &fake.SamplingPolicyRepository{Policy: policy},
		Draw:     func() float64 { return draw },
		Now:      fixedClock(testClock),
	}
}

func TestSamplingPolicyEnforcerAppliesPolicyGates(t *testing.T) {
	ctx := context.Background()
	risky := testSignal()
	risky.RiskScore = 0.9

	reason, err := newEnforcer(samplingPolicy(), 0.99).Sample(ctx, risky)
	if err != nil || reason != sampleReasonRisk {
		t.Fatalf("risky = %q, %v", reason, err)
	}
	uncertain := testSignal()
	uncertain.Uncertainty = 0.8
	if reason, _ := newEnforcer(samplingPolicy(), 0.4).Sample(ctx, uncertain); reason != sampleReasonUncertainty {
		t.Fatalf("uncertain = %q", reason)
	}
	if reason, _ := newEnforcer(samplingPolicy(), 0.05).Sample(ctx, testSignal()); reason != sampleReasonBase {
		t.Fatalf("base = %q", reason)
	}
	if reason, _ := newEnforcer(samplingPolicy(), 0.5).Sample(ctx, testSignal()); reason != "" {
		t.Fatalf("draw above base rate sampled anyway: %q", reason)
	}

	optedOut := samplingPolicy()
	optedOut.OptOut = true
	if reason, _ := newEnforcer(optedOut, 0).Sample(ctx, risky); reason != "" {
		t.Fatalf("opt-out sampled: %q", reason)
	}
	unredacted := risky
	unredacted.Redacted = false
	if reason, _ := newEnforcer(samplingPolicy(), 0).Sample(ctx, unredacted); reason != "" {
		t.Fatalf("unredacted turn sampled: %q", reason)
	}
	foreignRegion := risky
	foreignRegion.TenantContext.Region = "us-east"
	if reason, _ := newEnforcer(samplingPolicy(), 0).Sample(ctx, foreignRegion); reason != "" {
		t.Fatalf("out-of-residency turn sampled: %q", reason)
	}
	feedback := testSignal()
	feedback.Feedback = map[string]float64{"escalation": 1}
	if reason, _ := newEnforcer(samplingPolicy(), 0.99).Sample(ctx, feedback); reason != sampleReasonFeedback {
		t.Fatalf("escalated turn = %q", reason)
	}
}

func TestSamplingPolicyEnforcerCapsPerTenantWindow(t *testing.T) {
	ctx := context.Background()
	enforcer := newEnforcer(samplingPolicy(), 0)
	risky := testSignal()
	risky.RiskScore = 1
	for i := range 2 {
		if reason, err := enforcer.Sample(ctx, risky); err != nil || reason == "" {
			t.Fatalf("sample %d = %q, %v", i, reason, err)
		}
	}
	if reason, _ := enforcer.Sample(ctx, risky); reason != "" {
		t.Fatalf("cap exceeded: %q", reason)
	}
	// A new window resets the cap.
	enforcer.Now = fixedClock(testClock.Add(2 * time.Hour))
	if reason, _ := enforcer.Sample(ctx, risky); reason == "" {
		t.Fatal("new window did not reset the per-tenant cap")
	}
}

func TestSamplingPolicyEnforcerValidatesPolicy(t *testing.T) {
	invalid := samplingPolicy()
	invalid.MaxPerWindow = 0
	if _, err := newEnforcer(invalid, 0).Sample(context.Background(), testSignal()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero cap = %v", err)
	}
	if _, err := newEnforcer(samplingPolicy(), 0).Sample(context.Background(), domain.SampleSignal{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty signal = %v", err)
	}
}

func testSampleStore(t *testing.T, storage *fake.ObjectStorage, query *queryFake) *EncryptedSampleStore {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 3)
	}
	return &EncryptedSampleStore{
		keelStore: keelStore{DB: dbFake{query: query}}, Storage: storage, Bucket: "eval", Key: key,
		RequireRedacted: true, Now: fixedClock(testClock),
	}
}

func testSample() domain.EvaluationSample {
	return domain.EvaluationSample{
		SampleID: "s1", TenantID: 7, RequestID: "req-1", AgentID: "agent", AgentVersion: "v2", Reason: sampleReasonRisk,
		RiskScore: 0.75, Uncertainty: 0.25, Redacted: true, RetentionClass: "short", Region: "eu-central",
		SampledAt: testClock, ExpiresAt: testClock.Add(24 * time.Hour),
	}
}

func TestEncryptedSampleStoreSealsPayloadAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	storage := &fake.ObjectStorage{}
	query := newQueryFake(nil)
	store := testSampleStore(t, storage, query)
	payload := []byte(`{"prompt":"[REDACTED]","output":"ok"}`)

	stored, err := store.Put(ctx, testSample(), payload)
	if err != nil {
		t.Fatal(err)
	}
	sealed, ok := storage.Payload("eval", "evaluation/samples/7/s1")
	if !ok || string(sealed) == string(payload) {
		t.Fatalf("payload was not sealed: %q", sealed)
	}
	if stored.Payload.Digest != sha256Hex(sealed) {
		t.Fatalf("digest = %q", stored.Payload.Digest)
	}
	args := query.args[qSampleInsert]
	if len(args) != 15 || args[0] != int64(7) || args[1] != "s1" || args[6] != int64(7500) || args[7] != int64(2500) || args[8] != true || args[10] != stored.Payload.Digest {
		t.Fatalf("sample insert args = %v", args)
	}

	query.rows[qSampleGet] = [][]any{{"req-1", "agent", "v2", sampleReasonRisk, int64(7500), int64(2500), true, stored.Payload.URI, stored.Payload.Digest, "short", "eu-central", testClock, testClock.Add(24 * time.Hour)}}
	sample, plain, err := store.Get(ctx, 7, "s1")
	if err != nil || string(plain) != string(payload) || sample.Region != "eu-central" {
		t.Fatalf("get = %+v, %q, %v", sample, plain, err)
	}

	storage.Overwrite("eval", "evaluation/samples/7/s1", []byte("enc:v1:tampered"))
	if _, _, err := store.Get(ctx, 7, "s1"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("digest mismatch = %v", err)
	}
}

func TestEncryptedSampleStoreEnforcesRedactionAndRetention(t *testing.T) {
	ctx := context.Background()
	store := testSampleStore(t, &fake.ObjectStorage{}, newQueryFake(nil))
	unredacted := testSample()
	unredacted.Redacted = false
	if _, err := store.Put(ctx, unredacted, []byte("payload")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unredacted = %v", err)
	}
	noExpiry := testSample()
	noExpiry.ExpiresAt = time.Time{}
	if _, err := store.Put(ctx, noExpiry, []byte("payload")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no expiry = %v", err)
	}
	traversal := testSample()
	traversal.SampleID = "../escape"
	if _, err := store.Put(ctx, traversal, []byte("payload")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("path traversal = %v", err)
	}

	expiredQuery := newQueryFake(map[string][][]any{qSampleGet: {{"req", "agent", "v2", "risk", int64(0), int64(0), true, "object://eval/x", sha256Hex(nil), "short", "eu", testClock.Add(-48 * time.Hour), testClock.Add(-time.Hour)}}})
	expiredStore := testSampleStore(t, &fake.ObjectStorage{}, expiredQuery)
	if _, _, err := expiredStore.Get(ctx, 7, "s1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired retention = %v", err)
	}
}

func TestEncryptedSampleStoreDeleteRemovesRowAndObject(t *testing.T) {
	ctx := context.Background()
	storage := &fake.ObjectStorage{}
	query := newQueryFake(map[string][][]any{qSampleDelete: {{"object://eval/evaluation/samples/7/s1"}}})
	store := testSampleStore(t, storage, query)
	if _, err := store.Put(ctx, testSample(), []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, 7, "s1"); err != nil {
		t.Fatal(err)
	}
	if len(storage.Deletes) != 1 || storage.Deletes[0] != "eval/evaluation/samples/7/s1" {
		t.Fatalf("deletes = %v", storage.Deletes)
	}
	query.rows[qSampleDelete] = nil
	if err := store.Delete(ctx, 7, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing sample = %v", err)
	}
}
