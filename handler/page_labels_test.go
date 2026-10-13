package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type labelerFunc func(context.Context, domain.Labels) error

func (function labelerFunc) Label(ctx context.Context, labels domain.Labels) error {
	return function(ctx, labels)
}

func captionAll(_ context.Context, labels domain.Labels) error {
	for id := range labels.Tenants {
		labels.Tenants[id] = "Workspace"
	}
	for ref := range labels.Principals {
		labels.Principals[ref] = fmt.Sprintf("name of %d/%s", ref.TenantID, ref.ID)
	}
	for ref := range labels.Clients {
		labels.Clients[ref] = "App"
	}
	return nil
}

func TestConfirmationsAndDecisionsShowTheLabelersCaptions(t *testing.T) {
	users := keelhandler.AbstractHandler{UserService: conversationUsers{}}
	approvals := &ApprovalHandler{AbstractHandler: users, Inbox: &confirmationInboxFake{}, Labels: labelerFunc(captionAll)}
	recorder := httptest.NewRecorder()
	approvals.Routes()[api.ConfirmationsPath](recorder, authenticatedRequest(http.MethodGet, api.ConfirmationsPath, nil))
	if body := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(body, `"tenant_label":"Workspace"`) ||
		!strings.Contains(body, `"maker":{"kind":"human","id":"9","label":"name of 7/9"}`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}

	decisions := &StudioDecisionsHandler{AbstractHandler: users, Labels: labelerFunc(captionAll),
		Backend: decisionsBackendFunc(func(context.Context, domain.StudioActor, domain.DecisionQuery) (domain.DecisionPage, error) {
			return domain.DecisionPage{Records: []domain.DecisionRecord{{ID: 1, ClientRef: "client-1",
				Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"},
				Authority: domain.AuthorityRef{Subject: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"}, GrantID: "g-1",
					Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}}}}, nil
		})}
	recorder = httptest.NewRecorder()
	decisions.Routes()[api.StudioDecisionsPath](recorder, authenticatedRequest(http.MethodGet, api.StudioDecisionsPath, nil))
	body := recorder.Body.String()
	for _, want := range []string{`"client_label":"App"`, `"principal":{"kind":"agent","id":"writer","label":"name of 7/writer"}`,
		`"grantor":{"kind":"human","id":"42","label":"name of 7/42"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %s: %s", want, body)
		}
	}

	approvals.Labels = labelerFunc(func(context.Context, domain.Labels) error {
		return fmt.Errorf("names table: %w", domain.ErrForbidden)
	})
	recorder = httptest.NewRecorder()
	approvals.Routes()[api.ConfirmationsPath](recorder, authenticatedRequest(http.MethodGet, api.ConfirmationsPath, nil))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "names table") {
		t.Fatalf("a failed labeler: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
