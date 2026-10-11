package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nauticana/keel/config"
	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
)

func TestStudioCommandsAndApprovalsBoundTheirBodies(t *testing.T) {
	previous := config.Config()
	config.SetConfig(&config.KeelConfig{MaxRequestSize: 64})
	t.Cleanup(func() { config.SetConfig(previous) })
	backend := &pilotBackendFake{}
	body := []byte(`{"agent_name":"writer-a","reason":"` + strings.Repeat("a", 128) + `"}`)
	recorder := httptest.NewRecorder()
	pilotHandler(backend).Routes()[api.StudioPilotAddPath](recorder, authenticatedRequest(http.MethodPost, api.StudioPilotAddPath, body))
	if recorder.Code != http.StatusBadRequest || backend.change.AgentID != "" {
		t.Fatalf("an oversized Studio command: status = %d, change = %+v", recorder.Code, backend.change)
	}
	approvals := &ApprovalHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}}
	recorder = httptest.NewRecorder()
	approvals.Routes()[api.ApprovalResolvePath](recorder, authenticatedRequest(http.MethodPost, api.ApprovalResolvePath, body))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an oversized approval resolve: status = %d", recorder.Code)
	}
}
