package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	keelhandler "github.com/nauticana/keel/handler"

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
	handler := &ApprovalHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Desk: desk}
	recorder := httptest.NewRecorder()
	spoofed := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"approved","proposed_digest":"d","tenant_id":99}`)
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, spoofed))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("spoofed status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	body := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"approved","proposed_digest":"d"}`)
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
	handler := &ApprovalHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Desk: desk}
	body := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"edited","proposed_digest":"d","revised_arguments":{"qty":5}}`)
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
	handler := &ApprovalHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Desk: desk}
	body := []byte(`{"request_id":"r-1","execution_step_id":3,"verdict":"approved","proposed_digest":"d","revised_arguments":null}`)
	recorder := httptest.NewRecorder()
	handler.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, body))
	if recorder.Code != http.StatusOK || desk.verdict.RevisedArguments != nil {
		t.Fatalf("status = %d, revised arguments = %q", recorder.Code, desk.verdict.RevisedArguments)
	}
}

func TestApprovalLinkMapsASpentLinkToNotFound(t *testing.T) {
	handler := &ApprovalHandler{Desk: &approvalDeskFake{err: domain.ErrNotFound}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, api.ApprovalLinkPath, bytes.NewReader([]byte(`{"token":"t","tenant_id":7,"request_id":"r-1","execution_step_id":3,"verdict":"approved"}`)))
	request.Header.Set("Content-Type", "application/json")
	handler.Routes()[api.ApprovalLinkPath](recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
