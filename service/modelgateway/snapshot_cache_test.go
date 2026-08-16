package modelgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func testSigner(t *testing.T) *HMACSnapshotSigner {
	t.Helper()
	signer, err := NewHMACSnapshotSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestSnapshotCacheServesLastGoodUntilExpiry(t *testing.T) {
	clock := routerNow
	catalog := &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Generation: 3, Candidates: []domain.ModelCandidate{candidate("p", "m", "eu", "eu-1", 1)}}}
	policies := &fake.TenantRoutingPolicyRepository{Policy: domain.RoutingPolicy{MinQualityClass: 1, AllowedRegions: []string{"eu"}}}
	budgets := &stubBudgetPolicy{limits: domain.BudgetLimits{WindowTokens: 10, WindowCostMinorUnits: 5, Currency: "USD", Window: time.Hour}}
	cache, err := NewSnapshotCache(catalog, policies, budgets, testSigner(t), time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	cache.Now = func() time.Time { return clock }
	var fallbacks []string
	cache.OnFallback = func(kind, _ string, _ error) { fallbacks = append(fallbacks, kind) }
	ctx := context.Background()
	tenant := domain.TenantContext{TenantID: 7}

	if _, err := cache.CandidatesFor(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.RoutingPolicyFor(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.BudgetFor(ctx, 7); err != nil {
		t.Fatal(err)
	}

	outage := errors.New("control plane down")
	catalog.Err, policies.Err = outage, outage
	budgets.err = outage
	clock = clock.Add(30 * time.Second)
	set, err := cache.CandidatesFor(ctx, tenant)
	if err != nil || set.Generation != 3 || len(set.Candidates) != 1 {
		t.Fatalf("cached set = %+v, err = %v", set, err)
	}
	policy, err := cache.RoutingPolicyFor(ctx, 7)
	if err != nil || policy.MinQualityClass != 1 || len(policy.AllowedRegions) != 1 {
		t.Fatalf("cached policy = %+v, err = %v", policy, err)
	}
	if _, err = cache.BudgetFor(ctx, 7); err != nil {
		t.Fatalf("cached budget: %v", err)
	}
	if len(fallbacks) != 3 {
		t.Fatalf("fallbacks = %v", fallbacks)
	}

	clock = clock.Add(31 * time.Second)
	if _, err = cache.CandidatesFor(ctx, tenant); !errors.Is(err, domain.ErrStaleEvidence) || !errors.Is(err, outage) {
		t.Fatalf("expired candidates = %v", err)
	}
	if _, err = cache.BudgetFor(ctx, 7); !errors.Is(err, domain.ErrStaleEvidence) {
		t.Fatalf("expired quota policy must fail closed: %v", err)
	}
}

func TestSnapshotCacheRejectsTamperedAndUnknownSnapshots(t *testing.T) {
	clock := routerNow
	catalog := &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Generation: 1}}
	cache, err := NewSnapshotCache(catalog, nil, nil, testSigner(t), time.Minute, 4)
	if err != nil {
		t.Fatal(err)
	}
	cache.Now = func() time.Time { return clock }
	ctx := context.Background()
	if _, err := cache.CandidatesFor(ctx, domain.TenantContext{TenantID: 7}); err != nil {
		t.Fatal(err)
	}
	exported, ok := cache.Snapshot(SnapshotKindCandidates, 7)
	if !ok {
		t.Fatal("expected an exported snapshot")
	}

	restored, err := NewSnapshotCache(catalog, nil, nil, testSigner(t), time.Minute, 4)
	if err != nil {
		t.Fatal(err)
	}
	restored.Now = func() time.Time { return clock }
	if err := restored.Restore(exported); err != nil {
		t.Fatal(err)
	}
	tampered := exported
	tampered.Payload = append([]byte(nil), exported.Payload...)
	tampered.Payload[0] ^= 0xff
	if err := restored.Restore(tampered); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tampered payload = %v", err)
	}
	extended := exported
	extended.ExpiresAt = clock.Add(time.Hour)
	if err := restored.Restore(extended); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("extended expiry = %v", err)
	}
	stale := exported
	stale.Kind = "other"
	if err := restored.Restore(stale); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown kind = %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if err := restored.Restore(exported); !errors.Is(err, domain.ErrStaleEvidence) {
		t.Fatalf("expired restore = %v", err)
	}
}

func TestSnapshotCacheValidatesConfig(t *testing.T) {
	catalog := &fake.ModelCandidateCatalog{}
	if _, err := NewSnapshotCache(catalog, nil, nil, nil, time.Minute, 1); err == nil {
		t.Fatal("expected signer error")
	}
	if _, err := NewSnapshotCache(catalog, nil, nil, testSigner(t), 0, 1); err == nil {
		t.Fatal("expected TTL error")
	}
	if _, err := NewSnapshotCache(nil, nil, nil, testSigner(t), time.Minute, 1); err == nil {
		t.Fatal("expected source error")
	}
	if _, err := NewHMACSnapshotSigner([]byte("short")); err == nil {
		t.Fatal("expected key length error")
	}
}

func TestSnapshotCacheEvictsBeyondBound(t *testing.T) {
	clock := routerNow
	catalog := &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Generation: 1}}
	cache, err := NewSnapshotCache(catalog, nil, nil, testSigner(t), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	cache.Now = func() time.Time { return clock }
	ctx := context.Background()
	for _, tenantID := range []int64{1, 2} {
		if _, err := cache.CandidatesFor(ctx, domain.TenantContext{TenantID: tenantID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := cache.Snapshot(SnapshotKindCandidates, 1); ok {
		t.Fatal("the earliest-expiring snapshot must be evicted")
	}
	if _, ok := cache.Snapshot(SnapshotKindCandidates, 2); !ok {
		t.Fatal("the newest snapshot must be retained")
	}
}

type stubBudgetPolicy struct {
	limits domain.BudgetLimits
	err    error
}

func (policy *stubBudgetPolicy) BudgetFor(context.Context, int64) (domain.BudgetLimits, error) {
	return policy.limits, policy.err
}
