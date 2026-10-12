package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type decisionsBackendFunc func(context.Context, domain.StudioActor, domain.DecisionQuery) (domain.DecisionPage, error)

func (function decisionsBackendFunc) Decisions(ctx context.Context, actor domain.StudioActor, query domain.DecisionQuery) (domain.DecisionPage, error) {
	return function(ctx, actor, query)
}

func TestStudioDecisionsPagesTheTenantsRecords(t *testing.T) {
	var asked domain.DecisionQuery
	handler := &StudioDecisionsHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}},
		Backend: decisionsBackendFunc(func(_ context.Context, actor domain.StudioActor, query domain.DecisionQuery) (domain.DecisionPage, error) {
			if actor.TenantID != 7 || actor.Principal.ID != "42" {
				return domain.DecisionPage{}, domain.ErrForbidden
			}
			asked = query
			return domain.DecisionPage{NextBefore: 54, Records: []domain.DecisionRecord{{
				ID: 55, Category: domain.DecisionCategoryToolInvoke, Action: "publish", Outcome: domain.DecisionDeny,
				Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"},
				Authority: domain.AuthorityRef{Subject: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"}, GrantID: "g-1",
					Grantor: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "lead"}},
				Obligations: []domain.ObligationKind{domain.ObligationRedact},
				Evidence:    domain.ObjectRef{URI: "s3://bucket/decision/7", Digest: "abc"}, OccurredAt: time.Unix(0, 0).UTC(),
			}, {ID: 54, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}}}, nil
		})}
	if feature := handler.Feature(); feature.Object != "AGENT_AUDIT" || feature.Actions[0] != "VIEW" {
		t.Fatalf("feature = %+v", feature)
	}
	recorder := httptest.NewRecorder()
	target := api.StudioDecisionsPath + "?before=60&limit=2&category=tool_invoke&outcome=D"
	handler.Routes()[api.StudioDecisionsPath](recorder, authenticatedRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if asked.Before != 60 || asked.Limit != 2 || asked.Category != "tool_invoke" || asked.Outcome != domain.DecisionDeny {
		t.Fatalf("query = %+v", asked)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"next_before":54`, `"id":55`, `"outcome":"D"`, `"principal":{"kind":"agent","id":"writer"}`,
		`"authority":{"subject":{"kind":"agent","id":"writer"},"grant_id":"g-1","grantor":{"kind":"agent","id":"lead"}}`,
		`"obligations":["redact"]`, `"evidence_digest":"abc"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "s3://") || strings.Count(body, `"authority"`) != 1 {
		t.Fatalf("the evidence location stays server-side and an ungranted decision has no authority: %s", body)
	}

	recorder = httptest.NewRecorder()
	handler.Routes()[api.StudioDecisionsPath](recorder, authenticatedRequest(http.MethodGet, api.StudioDecisionsPath+"?before=x", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed before status = %d", recorder.Code)
	}
}
