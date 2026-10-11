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
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type testDraftBackendFake struct {
	contract.AgentStudioHTTPBackend
	err error
}

func (f *testDraftBackendFake) TestDraft(context.Context, domain.StudioActor, domain.AgentTestRequest) (domain.AgentTestResult, error) {
	return domain.AgentTestResult{}, f.err
}

// A draft stopped by its own configured limit is a client-meaningful 422 whose body
// names the limit only, never the wrapped detail.
func TestStudioTestRouteMapsTheDraftsExecutionLimit(t *testing.T) {
	h := &StudioHandler{
		AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}},
		Backend:         &testDraftBackendFake{err: fmt.Errorf("%w: draft exceeded its 5s duration: provider detail", domain.ErrExecutionLimit)},
	}
	recorder := httptest.NewRecorder()
	h.Routes()[api.StudioTestPath](recorder, authenticatedRequest(http.MethodPost, api.StudioTestPath, []byte(`{"agent_name":"writer-a","task":"long"}`)))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s; want 422", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, domain.ErrExecutionLimit.Error()) || strings.Contains(body, "provider detail") || strings.Contains(body, "exceeded its 5s") {
		t.Fatalf("body = %s, want only the limit's stable message", body)
	}
}
