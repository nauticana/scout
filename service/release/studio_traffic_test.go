package release

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type studioAuthorizerFunc func(context.Context, domain.StudioActor, domain.StudioAction, string) error

func (function studioAuthorizerFunc) Authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
	return function(ctx, actor, action, agentID)
}

type restoreSeparation struct {
	required bool
	err      error
}

func (r restoreSeparation) Required(_ context.Context, _ int64, _ string, kind domain.PublishReviewKind, _ time.Time) (bool, error) {
	return kind == domain.PublishReviewOfRestore && r.required, r.err
}

func (restoreSeparation) Excuse(context.Context, int64, string, domain.PublishReviewKind, string, domain.PrincipalRef, time.Time) error {
	return domain.ErrForbidden
}

var trafficActor = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}

type basicDeploymentStore struct {
	store *fake.AgentDeploymentStore
}

func (store basicDeploymentStore) Get(ctx context.Context, tenantID int64, agentID string) (domain.AgentDeployment, error) {
	return store.store.Get(ctx, tenantID, agentID)
}

func (store basicDeploymentStore) SetCanary(ctx context.Context, tenantID int64, agentID, version string, percentage int) error {
	return store.store.SetCanary(ctx, tenantID, agentID, version, percentage)
}

func (store basicDeploymentStore) Promote(ctx context.Context, tenantID int64, agentID, version string) error {
	return store.store.Promote(ctx, tenantID, agentID, version)
}

func (store basicDeploymentStore) RestorePrevious(ctx context.Context, tenantID int64, agentID string) (string, error) {
	return store.store.RestorePrevious(ctx, tenantID, agentID)
}

var _ contract.AgentDeploymentStore = basicDeploymentStore{}

func studioTraffic(query *queryFake, allowed domain.StudioAction) *StudioTraffic {
	deployments := &fake.AgentDeploymentStore{Deployments: map[string]domain.AgentDeployment{
		"7|writer-a": {TenantID: 7, AgentID: "writer-a", StableVersion: "3"},
	}}
	return &StudioTraffic{
		DB: dbFake{query: query}, Deployments: deployments,
		Traffic: &PinnedTrafficManager{Deployments: deployments}, Separation: restoreSeparation{},
		Authorizer: studioAuthorizerFunc(func(_ context.Context, _ domain.StudioActor, action domain.StudioAction, _ string) error {
			if action != allowed {
				return domain.ErrForbidden
			}
			return nil
		}),
	}
}

func TestStudioCanaryNeedsPublishAndRecordsTheActor(t *testing.T) {
	query := newQueryFake(nil)
	deployment, err := studioTraffic(query, domain.StudioPublish).Canary(context.Background(), trafficActor, "writer-a", "4", 10)
	if err != nil || deployment.CanaryVersion != "4" || deployment.CanaryPercentage != 10 {
		t.Fatalf("deployment = %+v, error = %v", deployment, err)
	}
	requireArgs(t, query, qTrafficEvent, int64(7), "writer-a", "CANARY", "version 4 at 10%", "human", "42")
	if _, err = studioTraffic(newQueryFake(nil), domain.StudioView).Promote(context.Background(), trafficActor, "writer-a", "4"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("promote without publish = %v, want forbidden", err)
	}
}

func TestStudioExplainReportsTheRecordedRelease(t *testing.T) {
	started := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	query := newQueryFake(map[string][][]any{qTrafficConversation: {{"writer-a", "writer", "4", "canary", "", "digest", started}}})
	explained, err := studioTraffic(query, domain.StudioView).Explain(context.Background(), trafficActor, "conversation-1")
	if err != nil || explained.Version != "4" || explained.Source != domain.VersionFromCanary || explained.AliasID != "writer" {
		t.Fatalf("explained = %+v, error = %v", explained, err)
	}
	if _, err = studioTraffic(query, domain.StudioPublish).Explain(context.Background(), trafficActor, "conversation-1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("explain without view = %v, want forbidden", err)
	}
	if _, err = studioTraffic(newQueryFake(nil), domain.StudioView).Explain(context.Background(), trafficActor, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing conversation = %v, want not found", err)
	}
}

func reviewedTraffic(query *queryFake, deployment domain.AgentDeployment) *StudioTraffic {
	traffic := studioTraffic(query, domain.StudioPublish)
	traffic.Separation = restoreSeparation{required: true}
	traffic.Deployments.(*fake.AgentDeploymentStore).Deployments["7|writer-a"] = deployment
	return traffic
}

func TestReviewedRestoresKeepTrafficOnTheStableAndCanaryReleases(t *testing.T) {
	ctx := context.Background()
	withCanary := domain.AgentDeployment{TenantID: 7, AgentID: "writer-a", StableVersion: "3", CanaryVersion: "4", CanaryPercentage: 10}
	stableOnly := domain.AgentDeployment{TenantID: 7, AgentID: "writer-a", StableVersion: "3"}

	if _, err := reviewedTraffic(newQueryFake(nil), stableOnly).Promote(ctx, trafficActor, "writer-a", "2"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("promoting an older release = %v, want conflict", err)
	}
	if _, err := reviewedTraffic(newQueryFake(nil), withCanary).Canary(ctx, trafficActor, "writer-a", "2", 20); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a canary of an older release = %v, want conflict", err)
	}
	if _, err := reviewedTraffic(newQueryFake(nil), stableOnly).Rollback(ctx, trafficActor, "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("rolling back the stable release = %v, want conflict", err)
	}

	query := newQueryFake(nil)
	deployment, err := reviewedTraffic(query, withCanary).Canary(ctx, trafficActor, "writer-a", "4", 50)
	if err != nil || deployment.CanaryVersion != "4" || deployment.CanaryPercentage != 50 {
		t.Fatalf("reweighting the canary = %+v, %v", deployment, err)
	}
	if deployment, err = reviewedTraffic(newQueryFake(nil), withCanary).Promote(ctx, trafficActor, "writer-a", "4"); err != nil || deployment.StableVersion != "4" {
		t.Fatalf("promoting the canary = %+v, %v", deployment, err)
	}
	if deployment, err = reviewedTraffic(newQueryFake(nil), withCanary).Rollback(ctx, trafficActor, "writer-a"); err != nil || deployment.StableVersion != "3" || deployment.CanaryVersion != "" {
		t.Fatalf("ending the canary = %+v, %v", deployment, err)
	}
}

func TestReviewedTrafficFailsClosedWithoutAtomicDeploymentChanges(t *testing.T) {
	traffic := reviewedTraffic(newQueryFake(nil), domain.AgentDeployment{
		TenantID: 7, AgentID: "writer-a", StableVersion: "3", CanaryVersion: "4", CanaryPercentage: 10,
	})
	deployments := traffic.Deployments.(*fake.AgentDeploymentStore)
	traffic.Traffic = &PinnedTrafficManager{Deployments: basicDeploymentStore{store: deployments}}

	if _, err := traffic.Promote(context.Background(), trafficActor, "writer-a", "4"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("promote without atomic traffic support = %v, want not ready", err)
	}
	deployment, err := deployments.Get(context.Background(), 7, "writer-a")
	if err != nil || deployment.StableVersion != "3" || deployment.CanaryVersion != "4" {
		t.Fatalf("deployment changed on refusal: %+v, %v", deployment, err)
	}
}

func TestTrafficFailsClosedWithoutARestoreSeparation(t *testing.T) {
	traffic := studioTraffic(newQueryFake(nil), domain.StudioPublish)
	traffic.Separation = restoreSeparation{err: domain.ErrNotReady}
	if _, err := traffic.Promote(context.Background(), trafficActor, "writer-a", "3"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("separation failure = %v, want not ready", err)
	}
	traffic.Separation = nil
	if _, err := traffic.Promote(context.Background(), trafficActor, "writer-a", "3"); err == nil {
		t.Fatal("a traffic surface without a publish separation must refuse")
	}
}
