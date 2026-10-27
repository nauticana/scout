package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func approvalRoutingHandler(view domain.ApprovalRoutingView, err error, asked *string) *StudioApprovalRoutingHandler {
	return &StudioApprovalRoutingHandler{
		UserService: conversationUsers{},
		Backend: fake.StudioApprovalRoutingBackendFunc(func(_ context.Context, actor domain.StudioActor, agentID string) (domain.ApprovalRoutingView, error) {
			*asked = fmt.Sprintf("%d/%s:%s/%s", actor.TenantID, actor.Principal.Kind, actor.Principal.ID, agentID)
			return view, err
		}),
	}
}

func TestStudioApprovalRoutingRouteServesTheView(t *testing.T) {
	cfo := domain.ApproverReference{Kind: domain.ApproverRole, Ref: "cfo-office"}
	view := domain.ApprovalRoutingView{
		AgentID: "writer-a", ResolverComposed: true, Problems: []string{"tool \"search\" can ask for approval"},
		Routes: []domain.ApprovalRouteOrigin{{ScopeID: "t:writer", Inherited: true, Route: domain.ApprovalRoute{
			ID: "pay", Match: domain.ApprovalRouteMatch{Tools: []string{"pay.*"}, Sources: []domain.ApprovalSource{domain.ApprovalSourcePolicy}, MinRiskTier: domain.RiskHigh},
			Approver: domain.ApproverReference{Kind: domain.ApproverPosition, Ref: "controller"}, DeadlineSeconds: 3600,
			Escalation:  []domain.ApprovalEscalation{{Approver: &cfo, ExtensionSeconds: 600}, {ExtensionSeconds: 60}},
			OnExhausted: domain.ApprovalExhaustHold, AuthenticatedOnly: true,
		}}},
		Coverage: []domain.ApprovalCoverage{
			{ToolID: "pay.wire", Source: domain.ApprovalSourcePolicy, RiskTier: domain.RiskHigh, RouteID: "pay"},
			{ToolID: "search", Source: domain.ApprovalSourceAutonomy, RiskTier: domain.RiskHigh},
		},
	}
	var asked string
	recorder := httptest.NewRecorder()
	approvalRoutingHandler(view, nil, &asked).Routes()[api.StudioApprovalRoutingPath](recorder,
		authenticatedRequest(http.MethodGet, api.StudioApprovalRoutingPath+"?agent_name=writer-a", nil))
	if recorder.Code != http.StatusOK || asked != "7/human:42/writer-a" {
		t.Fatalf("status = %d, asked %q, body = %s", recorder.Code, asked, recorder.Body.String())
	}
	var envelope struct{ Data api.AgentApprovalRouting }
	if err := common.DecodeJSON(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	body := envelope.Data
	if body.AgentName != "writer-a" || !body.ResolverComposed || len(body.Problems) != 1 || len(body.Routes) != 1 || len(body.Coverage) != 2 {
		t.Fatalf("body = %+v", body)
	}
	route := body.Routes[0]
	if route.ID != "pay" || route.ScopeID != "t:writer" || !route.Inherited || route.OnExhausted != "hold" || !route.AuthenticatedOnly || route.Approver.Ref != "controller" ||
		route.Match.Sources[0] != "policy" || route.Match.MinRiskTier != "high" || len(route.Escalation) != 2 ||
		route.Escalation[0].Approver == nil || route.Escalation[0].Approver.Ref != "cfo-office" || route.Escalation[1].Approver != nil {
		t.Fatalf("route = %+v", route)
	}
	if body.Coverage[0].RouteID != "pay" || body.Coverage[1].RouteID != "" || !strings.Contains(recorder.Body.String(), `"tool_id":"search","source":"autonomy","risk_tier":"high"}`) {
		t.Fatalf("coverage = %s", recorder.Body.String())
	}
}

func TestStudioApprovalRoutingRouteMapsRefusals(t *testing.T) {
	var asked string
	for status, err := range map[int]error{
		http.StatusNotFound:   domain.ErrNotFound,
		http.StatusForbidden:  domain.ErrForbidden,
		http.StatusBadRequest: domain.ErrValidation,
	} {
		recorder := httptest.NewRecorder()
		approvalRoutingHandler(domain.ApprovalRoutingView{}, err, &asked).Routes()[api.StudioApprovalRoutingPath](recorder,
			authenticatedRequest(http.MethodGet, api.StudioApprovalRoutingPath+"?agent_name=writer-a", nil))
		if recorder.Code != status {
			t.Errorf("%v: status = %d, want %d", err, recorder.Code, status)
		}
	}
	feature := (&StudioApprovalRoutingHandler{}).Feature()
	if feature.ID != "approval_routing" || feature.Version != 1 || len(feature.Actions) != 1 || feature.Actions[0] != "VIEW" {
		t.Fatalf("feature = %+v", feature)
	}
}

func TestApprovalResponsesCarryTheRoutedAuthorityButNotItsEvidence(t *testing.T) {
	routed := apiApproval(domain.ApprovalRequest{
		RequestID: "r-1", Source: domain.ApprovalSourcePolicy,
		Route: domain.ApprovalRouteAuthority{RouteID: "pay", Step: 2, Resolution: "directory:holder/77",
			Reference: domain.ApproverReference{Kind: domain.ApproverPosition, Ref: "controller"}},
	})
	encoded, err := json.Marshal(routed)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"rule_source":"policy"`) || !strings.Contains(text, `"route_id":"pay"`) || !strings.Contains(text, `"route_step":2`) ||
		!strings.Contains(text, `"approver_reference":{"kind":"position","ref":"controller"}`) || strings.Contains(text, "directory") {
		t.Fatalf("routed = %s", text)
	}
	unrouted, err := json.Marshal(apiApproval(domain.ApprovalRequest{RequestID: "r-2", RuleID: "autonomy", Source: domain.ApprovalSourceAutonomy}))
	text = string(unrouted)
	if err != nil || !strings.Contains(text, `"rule_id":"autonomy"`) || !strings.Contains(text, `"rule_source":"autonomy"`) ||
		strings.Contains(text, "approver_reference") || strings.Contains(text, "route_id") {
		t.Fatalf("a request routed by scope = %s, %v", text, err)
	}
}
