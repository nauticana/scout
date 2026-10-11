package release

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type studioAuthorizerFunc func(context.Context, domain.StudioActor, domain.StudioAction, string) error

func (function studioAuthorizerFunc) Authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
	return function(ctx, actor, action, agentID)
}

var trafficActor = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}

func studioTraffic(query *queryFake, allowed domain.StudioAction) *StudioTraffic {
	deployments := &fake.AgentDeploymentStore{Deployments: map[string]domain.AgentDeployment{
		"7|writer-a": {TenantID: 7, AgentID: "writer-a", StableVersion: "3"},
	}}
	return &StudioTraffic{
		DB: dbFake{query: query}, Deployments: deployments,
		Traffic: &PinnedTrafficManager{Deployments: deployments},
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
