package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

var routerNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func candidate(provider, model, region, route string, quality int) domain.ModelCandidate {
	return domain.ModelCandidate{Provider: provider, Model: model, Region: region, RouteID: route, QualityClass: quality,
		Capabilities: []string{"text"}, MaxContextTokens: 8_000, MaxOutputTokens: 2_000}
}

func healthy(provider, model, region, route string, queue time.Duration) domain.CapacitySnapshot {
	return domain.CapacitySnapshot{Provider: provider, Model: model, Region: region, RouteID: route, Healthy: true, Warm: true,
		PredictedQueueDelay: queue, TimeToFirstToken: 100 * time.Millisecond, TimePerOutputToken: time.Millisecond, ObservedAt: routerNow, Generation: 9}
}

type routerFixture struct {
	catalog  *fake.ModelCandidateCatalog
	capacity *fake.CapacitySnapshotSource
	policies *fake.TenantRoutingPolicyRepository
	audit    *fake.AuditSink
	events   []domain.AuditEvent
	router   *PolicyRouter
}

func newRouterFixture(t *testing.T) *routerFixture {
	t.Helper()
	fixture := &routerFixture{
		catalog: &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Generation: 5, Candidates: []domain.ModelCandidate{
			candidate("p", "fast", "eu", "eu-1", 1),
			candidate("p", "smart", "eu", "eu-2", 2),
			candidate("p", "smart", "us", "us-1", 2),
		}}},
		capacity: &fake.CapacitySnapshotSource{Items: []domain.CapacitySnapshot{
			healthy("p", "fast", "eu", "eu-1", 0),
			healthy("p", "smart", "eu", "eu-2", 0),
			healthy("p", "smart", "us", "us-1", 0),
		}},
		policies: &fake.TenantRoutingPolicyRepository{},
	}
	fixture.audit = &fake.AuditSink{RecordFunc: func(_ context.Context, event domain.AuditEvent) error {
		fixture.events = append(fixture.events, event)
		return nil
	}}
	router, err := NewPolicyRouter(fixture.catalog, fixture.capacity, fixture.policies, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router.Audit = fixture.audit
	router.Now = func() time.Time { return routerNow }
	fixture.router = router
	return fixture
}

func routerRequest() domain.ModelRequest {
	return domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 7, Region: "eu"}, RequestID: "req", MaxOutputTokens: 100,
		Prompt: []byte("hello"), RequiredCapabilities: []string{"text"}}
}

func TestPolicyRouterRanksQualityThenLocality(t *testing.T) {
	fixture := newRouterFixture(t)
	selection, err := fixture.router.Select(context.Background(), routerRequest())
	if err != nil {
		t.Fatal(err)
	}
	if selection.RouteID != "eu-2" || selection.Model != "smart" || selection.Region != "eu" {
		t.Fatalf("selection = %+v", selection)
	}
	if selection.RoutingGeneration != combineGenerations(5, 9) || !strings.HasPrefix(selection.Reason, "preferred quality=2") {
		t.Fatalf("selection = %+v", selection)
	}
	if len(fixture.events) != 1 || fixture.events[0].Category != AuditCategoryModelRoute || fixture.events[0].TenantID != 7 {
		t.Fatalf("events = %+v", fixture.events)
	}
	var payload routeAuditPayload
	if err := json.Unmarshal(fixture.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CatalogGeneration != 5 || payload.SnapshotGeneration != 9 || payload.RoutingGeneration != selection.RoutingGeneration || payload.RouteID != "eu-2" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestPolicyRouterFilters(t *testing.T) {
	stale := routerNow.Add(-2 * time.Minute)
	tests := []struct {
		name      string
		policy    domain.RoutingPolicy
		snapshots func([]domain.CapacitySnapshot) []domain.CapacitySnapshot
		request   func(*domain.ModelRequest)
		ctx       func() (context.Context, context.CancelFunc)
		wantRoute string
		wantErr   error
	}{
		{name: "residency excludes other regions", policy: domain.RoutingPolicy{AllowedRegions: []string{"us"}}, wantRoute: "us-1"},
		{name: "stale snapshot rejected", snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot {
			items[1].ObservedAt = stale
			return items
		}, wantRoute: "us-1"},
		{name: "missing snapshot rejected unless allowed", snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot {
			return items[:1]
		}, wantRoute: "eu-1"},
		{name: "unknown capacity admitted by policy after known", policy: domain.RoutingPolicy{AllowUnknownCapacity: true},
			snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot { return items[2:] }, wantRoute: "us-1"},
		{name: "draining route excluded", snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot {
			items[1].Draining = true
			return items
		}, wantRoute: "us-1"},
		{name: "unhealthy route excluded", snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot {
			items[1].Healthy = false
			return items
		}, wantRoute: "us-1"},
		{name: "deadline infeasible route skipped", snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot {
			items[1].PredictedQueueDelay = 5 * time.Second
			return items
		}, ctx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), routerNow.Add(time.Second))
		}, wantRoute: "us-1"},
		{name: "every route infeasible", snapshots: func(items []domain.CapacitySnapshot) []domain.CapacitySnapshot {
			for i := range items {
				items[i].PredictedQueueDelay = 5 * time.Second
			}
			return items
		}, ctx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), routerNow.Add(time.Second))
		}, wantErr: domain.ErrDeadlineInfeasible},
		{name: "capability required", request: func(request *domain.ModelRequest) { request.RequiredCapabilities = []string{"vision"} }, wantErr: domain.ErrNoRoute},
		{name: "context limit", request: func(request *domain.ModelRequest) { request.MaxOutputTokens = 3_000 }, wantErr: domain.ErrNoRoute},
		{name: "excluded route", request: func(request *domain.ModelRequest) { request.ExcludedRouteIDs = []string{"eu-2"} }, wantRoute: "us-1"},
		{name: "min quality falls to fallback", policy: domain.RoutingPolicy{MinQualityClass: 3, Fallbacks: []domain.ModelReference{{ProviderID: "p", ModelID: "fast"}}}, wantRoute: "eu-1"},
		{name: "min quality without fallback", policy: domain.RoutingPolicy{MinQualityClass: 3}, wantErr: domain.ErrNoRoute},
		{name: "fallback never violates residency", policy: domain.RoutingPolicy{AllowedRegions: []string{"eu"}, MinQualityClass: 3,
			Fallbacks: []domain.ModelReference{{ProviderID: "p", ModelID: "smart"}}}, wantRoute: "eu-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRouterFixture(t)
			fixture.policies.Policy = test.policy
			if test.snapshots != nil {
				fixture.capacity.Items = test.snapshots(fixture.capacity.Items)
			}
			request := routerRequest()
			if test.request != nil {
				test.request(&request)
			}
			ctx := context.Background()
			if test.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = test.ctx()
				defer cancel()
			}
			selection, err := fixture.router.Select(ctx, request)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				if len(fixture.events) != 1 {
					t.Fatalf("audit events = %d", len(fixture.events))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if selection.RouteID != test.wantRoute {
				t.Fatalf("route = %q (%s), want %q", selection.RouteID, selection.Reason, test.wantRoute)
			}
		})
	}
}

func TestPolicyRouterFallbackReasonIsExplicit(t *testing.T) {
	fixture := newRouterFixture(t)
	fixture.policies.Policy = domain.RoutingPolicy{MinQualityClass: 3, Fallbacks: []domain.ModelReference{{ProviderID: "x", ModelID: "none"}, {ProviderID: "p", ModelID: "fast"}}}
	selection, err := fixture.router.Select(context.Background(), routerRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(selection.Reason, "fallback[1]") {
		t.Fatalf("reason = %q", selection.Reason)
	}
}

func TestPolicyRouterCostTiebreak(t *testing.T) {
	fixture := newRouterFixture(t)
	fixture.catalog.Set.Candidates = []domain.ModelCandidate{candidate("p", "smart", "eu", "eu-2", 2), candidate("q", "smart", "eu", "eu-3", 2)}
	fixture.capacity.Items = []domain.CapacitySnapshot{healthy("p", "smart", "eu", "eu-2", 0), healthy("q", "smart", "eu", "eu-3", 0)}
	fixture.router.Pricer = fake.ModelPricerFunc(func(_ context.Context, reference domain.ModelReference, usage domain.ModelUsage) (int64, string, error) {
		if usage.OutputTokens != 100 || usage.InputTokens != 2 {
			t.Fatalf("usage = %+v", usage)
		}
		if reference.ProviderID == "q" {
			return 5, "USD", nil
		}
		return 9, "USD", nil
	})
	selection, err := fixture.router.Select(context.Background(), routerRequest())
	if err != nil {
		t.Fatal(err)
	}
	if selection.RouteID != "eu-3" {
		t.Fatalf("route = %q", selection.RouteID)
	}
	fixture.router.Pricer = fake.ModelPricerFunc(func(_ context.Context, reference domain.ModelReference, _ domain.ModelUsage) (int64, string, error) {
		if reference.ProviderID == "q" {
			return 0, "", domain.ErrNotFound
		}
		return 9, "USD", nil
	})
	selection, err = fixture.router.Select(context.Background(), routerRequest())
	if err != nil || selection.RouteID != "eu-2" {
		t.Fatalf("unpriced route must be skipped: %+v %v", selection, err)
	}
}

func TestPolicyRouterAffinity(t *testing.T) {
	fixture := newRouterFixture(t)
	fixture.catalog.Set.Candidates = []domain.ModelCandidate{candidate("p", "smart", "eu", "eu-2", 2), candidate("p", "smart", "eu", "eu-3", 2)}
	fixture.capacity.Items = []domain.CapacitySnapshot{healthy("p", "smart", "eu", "eu-2", 0), healthy("p", "smart", "eu", "eu-3", 0)}
	fixture.policies.Policy = domain.RoutingPolicy{PreferAffinity: true}
	affinity := &fake.RouteAffinity{Bindings: map[string]domain.ModelSelection{"session": {Provider: "p", Model: "smart", Region: "eu", RouteID: "eu-3"}}}
	fixture.router.Affinity = affinity
	request := routerRequest()
	request.AffinityKey = "session"

	selection, err := fixture.router.Select(context.Background(), request)
	if err != nil || selection.RouteID != "eu-3" {
		t.Fatalf("affinity respected: %+v %v", selection, err)
	}
	fixture.capacity.Items[1].PredictedQueueDelay = 5 * time.Second
	ctx, cancel := context.WithDeadline(context.Background(), routerNow.Add(time.Second))
	defer cancel()
	selection, err = fixture.router.Select(ctx, request)
	if err != nil || selection.RouteID != "eu-2" {
		t.Fatalf("affinity overridden: %+v %v", selection, err)
	}
	if affinity.Bindings["session"].RouteID != "eu-2" {
		t.Fatalf("affinity not re-bound: %+v", affinity.Bindings)
	}
}

func TestPolicyRouterFailsWhenAuditFails(t *testing.T) {
	fixture := newRouterFixture(t)
	fixture.audit.RecordFunc = func(context.Context, domain.AuditEvent) error { return errors.New("audit down") }
	if _, err := fixture.router.Select(context.Background(), routerRequest()); err == nil || !strings.Contains(err.Error(), "audit down") {
		t.Fatalf("error = %v", err)
	}
}

func TestPolicyRouterValidatesConfig(t *testing.T) {
	if _, err := NewPolicyRouter(nil, &fake.CapacitySnapshotSource{}, &fake.TenantRoutingPolicyRepository{}, time.Minute); err == nil {
		t.Fatal("expected missing catalog error")
	}
	if _, err := NewPolicyRouter(&fake.ModelCandidateCatalog{}, &fake.CapacitySnapshotSource{}, &fake.TenantRoutingPolicyRepository{}, 0); err == nil {
		t.Fatal("expected max snapshot age error")
	}
	fixture := newRouterFixture(t)
	if _, err := fixture.router.Select(context.Background(), domain.ModelRequest{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestCombineGenerationsIsDeterministic(t *testing.T) {
	if combineGenerations(1, 2) != combineGenerations(1, 2) || combineGenerations(1, 2) == combineGenerations(2, 1) || combineGenerations(1, 2) < 0 {
		t.Fatal("generation combination must be deterministic, order-sensitive, and non-negative")
	}
}
