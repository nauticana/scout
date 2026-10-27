package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type approvalDeskFake struct {
	verdict domain.ApprovalVerdict
	err     error
}

func (*approvalDeskFake) Pending(context.Context, int64, domain.PrincipalRef, int) ([]domain.ApprovalRequest, error) {
	return nil, nil
}

func (desk *approvalDeskFake) Resolve(_ context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	desk.verdict = verdict
	return domain.ApprovalRequest{RequestID: verdict.RequestKey.RequestID, Status: verdict.Status}, desk.err
}

func (desk *approvalDeskFake) RedeemLink(context.Context, string, domain.ApprovalKey, domain.ApprovalStatus, string) (domain.ApprovalRequest, error) {
	return domain.ApprovalRequest{}, desk.err
}

func TestApprovalResolveDecidesAsTheSessionUser(t *testing.T) {
	desk := &approvalDeskFake{}
	handler := &ApprovalHandler{UserService: conversationUsers{}, Desk: desk}
	recorder := httptest.NewRecorder()
	spoofed := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"` + string(domain.ApprovalStatusApproved) + `","proposed_digest":"d","tenant_id":99}`)
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, spoofed))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("spoofed status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	body := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"` + string(domain.ApprovalStatusApproved) + `","proposed_digest":"d"}`)
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if desk.verdict.RequestKey.TenantID != 7 || desk.verdict.Decider.ID != "42" || desk.verdict.Status != domain.ApprovalStatusApproved {
		t.Fatalf("verdict = %+v", desk.verdict)
	}
}

func TestApprovalResolvePassesAnEditsRevisedArguments(t *testing.T) {
	desk := &approvalDeskFake{}
	handler := &ApprovalHandler{UserService: conversationUsers{}, Desk: desk}
	body := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"` + string(domain.ApprovalStatusEdited) + `","proposed_digest":"d","revised_arguments":{"qty":5}}`)
	recorder := httptest.NewRecorder()
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if desk.verdict.Status != domain.ApprovalStatusEdited || string(desk.verdict.RevisedArguments) != `{"qty":5}` || desk.verdict.Revision.URI != "" {
		t.Fatalf("verdict = %+v", desk.verdict)
	}
	desk.err = domain.ErrValidation
	recorder = httptest.NewRecorder()
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, body))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an invalid revision: status = %d", recorder.Code)
	}
}

func TestApprovalResolveTreatsNullRevisedArgumentsAsAbsent(t *testing.T) {
	desk := &approvalDeskFake{}
	handler := &ApprovalHandler{UserService: conversationUsers{}, Desk: desk}
	body := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"` + string(domain.ApprovalStatusApproved) + `","proposed_digest":"d","revised_arguments":null}`)
	recorder := httptest.NewRecorder()
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, body))
	if recorder.Code != http.StatusOK || desk.verdict.RevisedArguments != nil {
		t.Fatalf("status = %d, revised arguments = %q", recorder.Code, desk.verdict.RevisedArguments)
	}
}

func TestApprovalLinkMapsASpentLinkToNotFound(t *testing.T) {
	handler := &ApprovalHandler{Desk: &approvalDeskFake{err: domain.ErrNotFound}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, api.ApprovalLinkPath, bytes.NewReader([]byte(`{"token":"t","tenant_id":7,"request_id":"r-1","execution_step_id":3,"verdict":"`+string(domain.ApprovalStatusApproved)+`"}`)))
	request.Header.Set("Content-Type", "application/json")
	handler.Routes()[api.ApprovalLinkPath](recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

type confirmationInboxFake struct {
	person  domain.PrincipalRef
	key     domain.MCPConfirmationKey
	approve bool
	outcome domain.MCPConfirmationStatus
	limit   int
	err     error
}

func (inbox *confirmationInboxFake) List(_ context.Context, person domain.PrincipalRef, _ time.Time, limit int) ([]domain.MCPInboxEntry, error) {
	inbox.person, inbox.limit = person, limit
	stored := domain.MCPConfirmation{ID: 5, TenantID: 7, Maker: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, Tool: "refund",
		Payload: json.RawMessage(`{"secret":1}`), Requirements: json.RawMessage(`{"role":"x"}`), Preview: json.RawMessage(`{"amount":"5.00"}`),
		Status: domain.MCPConfirmationPending}
	return []domain.MCPInboxEntry{{MCPConfirmation: stored, CanDecide: true}}, inbox.err
}

func (inbox *confirmationInboxFake) Decide(_ context.Context, person domain.PrincipalRef, key domain.MCPConfirmationKey, approve bool, _ string) (domain.MCPConfirmation, error) {
	inbox.person, inbox.key, inbox.approve = person, key, approve
	return domain.MCPConfirmation{ID: key.ID, TenantID: key.TenantID, Status: domain.MCPConfirmationApproved, Decider: person}, inbox.err
}

func (inbox *confirmationInboxFake) Withdraw(_ context.Context, person domain.PrincipalRef, key domain.MCPConfirmationKey) error {
	inbox.person, inbox.key = person, key
	return inbox.err
}

func (inbox *confirmationInboxFake) Reconcile(_ context.Context, person domain.PrincipalRef, key domain.MCPConfirmationKey, outcome domain.MCPConfirmationStatus, _ string) (domain.MCPConfirmation, error) {
	inbox.person, inbox.key, inbox.outcome = person, key, outcome
	return domain.MCPConfirmation{ID: key.ID, TenantID: key.TenantID, Status: outcome}, inbox.err
}

func TestApprovalConfirmationRoutesActAsTheSessionPerson(t *testing.T) {
	if routes := (&ApprovalHandler{}).Routes(); routes[api.ConfirmationsPath] != nil {
		t.Fatal("confirmation routes need a composed inbox")
	}
	configureConversationRequestLimit(t)
	inbox := &confirmationInboxFake{}
	handler := &ApprovalHandler{UserService: conversationUsers{}, Inbox: inbox}
	serve := func(method, path, target string, body []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.Routes()[path](recorder, authenticatedRequest(method, target, body))
		return recorder
	}

	recorder := serve(http.MethodGet, api.ConfirmationsPath, api.ConfirmationsPath+"?since=2026-10-01T00:00:00Z&limit=3", nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || inbox.person.ID != "42" || inbox.limit != 3 ||
		!strings.Contains(body, `"preview":{"amount":"5.00"}`) || !strings.Contains(body, `"can_decide":true`) ||
		strings.Contains(body, "secret") || strings.Contains(body, "role") {
		t.Fatalf("list status = %d, body = %s", recorder.Code, body)
	}
	if recorder = serve(http.MethodGet, api.ConfirmationsPath, api.ConfirmationsPath+"?since=yesterday", nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed since status = %d", recorder.Code)
	}

	recorder = serve(http.MethodPost, api.ConfirmationDecidePath, api.ConfirmationDecidePath, []byte(`{"tenant_id":8,"id":5,"approve":true}`))
	if recorder.Code != http.StatusOK || inbox.key != (domain.MCPConfirmationKey{TenantID: 8, ID: 5}) || !inbox.approve ||
		!strings.Contains(recorder.Body.String(), `"decider":{"kind":"human","id":"42"}`) {
		t.Fatalf("decide status = %d, key = %+v, body = %s", recorder.Code, inbox.key, recorder.Body.String())
	}
	recorder = serve(http.MethodPost, api.ConfirmationReconcilePath, api.ConfirmationReconcilePath, []byte(`{"tenant_id":8,"id":5,"outcome":"D","note":"checked"}`))
	if recorder.Code != http.StatusOK || inbox.outcome != domain.MCPConfirmationExecuted {
		t.Fatalf("reconcile status = %d, outcome = %q", recorder.Code, inbox.outcome)
	}
	inbox.err = domain.ErrForbidden
	if recorder = serve(http.MethodGet, api.ConfirmationsPath, api.ConfirmationsPath, nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("forbidden list status = %d", recorder.Code)
	}
	inbox.err = domain.ErrNotFound
	if recorder = serve(http.MethodPost, api.ConfirmationWithdrawPath, api.ConfirmationWithdrawPath, []byte(`{"tenant_id":8,"id":6}`)); recorder.Code != http.StatusNotFound || inbox.key.ID != 6 {
		t.Fatalf("withdraw status = %d", recorder.Code)
	}
	if recorder = serve(http.MethodPost, api.ConfirmationDecidePath, api.ConfirmationDecidePath, []byte(`{"tenant_id":8,"id":5,"maker":"x"}`)); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown member status = %d", recorder.Code)
	}
}
