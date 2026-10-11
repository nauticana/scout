package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func limitsHandler(report domain.AgentLimitsReport, err error, asked *string) *StudioLimitsHandler {
	return &StudioLimitsHandler{
		AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}},
		Backend: fake.StudioLimitsBackendFunc(func(_ context.Context, actor domain.StudioActor, agentID string) (domain.AgentLimitsReport, error) {
			*asked = fmt.Sprintf("%d/%s:%s/%s", actor.TenantID, actor.Principal.Kind, actor.Principal.ID, agentID)
			return report, err
		}),
	}
}

func TestStudioLimitsRouteServesTheReport(t *testing.T) {
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	report := domain.AgentLimitsReport{
		AgentID: "writer-a", Release: "4", TurnsLastMinute: 3,
		Limits: domain.ExecutionLimits{TurnsPerMinute: 30, MaxSteps: 6, MaxToolCalls: 4, MaxDuration: 90 * time.Second},
		Budget: &domain.BudgetLimits{WindowTokens: 50_000, WindowCostMinorUnits: 2_000, Currency: "EUR", Window: 24 * time.Hour},
		Spend:  &domain.BudgetSpend{PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1), Tokens: 1200, CostMinorUnits: 450, Currency: "EUR", Exponent: 2},
	}
	var asked string
	recorder := httptest.NewRecorder()
	limitsHandler(report, nil, &asked).Routes()[api.StudioLimitsPath](recorder,
		authenticatedRequest(http.MethodGet, api.StudioLimitsPath+"?agent_name=writer-a", nil))
	if recorder.Code != http.StatusOK || asked != "7/human:42/writer-a" {
		t.Fatalf("status = %d, asked %q, body = %s", recorder.Code, asked, recorder.Body.String())
	}
	var envelope struct{ Data api.AgentLimits }
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	body := envelope.Data
	if body.AgentName != "writer-a" || body.Version != "4" || body.TurnsLastMinute != 3 ||
		body.Limits != (api.AgentExecutionLimits{TurnsPerMinute: 30, MaxSteps: 6, MaxToolCalls: 4, MaxDurationSeconds: 90}) {
		t.Fatalf("body = %+v", body)
	}
	if body.Budget == nil || *body.Budget != (api.AgentBudget{Tokens: 50_000, CostMinorUnits: 2_000, Currency: "EUR", WindowSeconds: 86_400}) {
		t.Fatalf("budget = %+v", body.Budget)
	}
	if body.Spend == nil || !body.Spend.PeriodStart.Equal(start) || !body.Spend.PeriodEnd.Equal(start.AddDate(0, 0, 1)) ||
		body.Spend.Tokens != 1200 || body.Spend.CostMinorUnits != 450 || body.Spend.Currency != "EUR" || body.Spend.Exponent != 2 {
		t.Fatalf("spend = %+v", body.Spend)
	}
}

func TestStudioLimitsRouteOmitsAnAbsentBudgetAndMapsRefusals(t *testing.T) {
	var asked string
	recorder := httptest.NewRecorder()
	limitsHandler(domain.AgentLimitsReport{AgentID: "writer-a", Release: "4"}, nil, &asked).Routes()[api.StudioLimitsPath](recorder,
		authenticatedRequest(http.MethodGet, api.StudioLimitsPath+"?agent_name=writer-a", nil))
	var envelope struct{ Data map[string]any }
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Data["budget"] != nil || envelope.Data["spend"] != nil || envelope.Data["limits"] == nil {
		t.Fatalf("body = %s, error = %v", recorder.Body.String(), err)
	}
	for status, err := range map[int]error{
		http.StatusNotFound:   fmt.Errorf("%w: agent %q has no published release", domain.ErrNotFound, "writer-a"),
		http.StatusForbidden:  domain.ErrForbidden,
		http.StatusBadRequest: domain.ErrValidation,
	} {
		recorder = httptest.NewRecorder()
		limitsHandler(domain.AgentLimitsReport{}, err, &asked).Routes()[api.StudioLimitsPath](recorder,
			authenticatedRequest(http.MethodGet, api.StudioLimitsPath+"?agent_name=writer-a", nil))
		if recorder.Code != status {
			t.Errorf("%v: status = %d, want %d", err, recorder.Code, status)
		}
	}
	recorder = httptest.NewRecorder()
	limitsHandler(domain.AgentLimitsReport{}, nil, &asked).Routes()[api.StudioLimitsPath](recorder,
		authenticatedRequest(http.MethodPost, api.StudioLimitsPath, []byte(`{}`)))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
	}
}

func TestStudioLimitsFeatureIsAViewFeature(t *testing.T) {
	feature := (&StudioLimitsHandler{}).Feature()
	if feature.ID != StudioFeatureLimits || feature.Version != 1 || len(feature.Actions) != 1 || feature.Actions[0] != string(domain.StudioView) {
		t.Fatalf("feature = %+v", feature)
	}
}
