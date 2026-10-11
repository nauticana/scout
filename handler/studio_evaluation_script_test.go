package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type scriptedBackendFake struct {
	evaluationBackendFake
	role domain.EvaluationRole
	runs int
}

func (f *scriptedBackendFake) Cases(_ context.Context, _ domain.StudioActor, agentID string) (domain.GoldenCaseSet, error) {
	return domain.GoldenCaseSet{AgentID: agentID, Cases: []domain.GoldenCase{{CaseID: "refund", Name: "Refund", Input: []byte("hi"),
		Script: domain.CaseScript{Turns: []domain.CaseTurn{{Input: "and then?"}},
			Tools: []domain.CaseTool{{ToolID: "orders.get", Output: json.RawMessage(`{"id":1}`)}, {ToolID: "refund.issue", IsError: true}}}}}}, nil
}

func (f *scriptedBackendFake) ChangeCases(ctx context.Context, actor domain.StudioActor, change domain.GoldenCaseChange) (domain.GoldenCaseSet, error) {
	f.change = change
	return f.Cases(ctx, actor, change.AgentID)
}

func (f *scriptedBackendFake) Trajectory(_ context.Context, _ domain.StudioActor, runID int64, exampleID string, role domain.EvaluationRole) (domain.CaseTrajectory, error) {
	f.role, f.runs = role, f.runs+1
	return domain.CaseTrajectory{RunID: runID, ExampleID: exampleID, Role: role, Events: []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToolCall, Name: "orders.get", Payload: []byte(`{"id":1}`), Offset: 1500 * time.Millisecond,
			Usage: domain.Usage{InputTokens: 9, SearchQueries: 3, CostMinorUnits: 2, Currency: "EUR"}},
		{Sequence: 2, Kind: domain.TrajectoryState, Name: "turn", Payload: []byte(`{"turn":2}`)},
	}}, nil
}

func TestStudioEvaluationHandlerCarriesACaseScriptBothWays(t *testing.T) {
	backend := &scriptedBackendFake{}
	h := &StudioEvaluationHandler{Backend: backend}
	view, err := h.changeCases(context.Background(), domain.StudioActor{}, api.GoldenCaseChange{
		AgentName: "writer-a", Op: "put", ExpectedRevision: 4,
		Case: api.GoldenCase{CaseID: "refund", Name: "Refund", Input: "hi", Turns: []string{"and then?"},
			Tools: []api.GoldenCaseTool{{ToolID: "orders.get", Output: json.RawMessage(`{"id":1}`)}, {ToolID: "refund.issue", IsError: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := backend.change.Case.Script
	if len(script.Turns) != 1 || script.Turns[0].Input != "and then?" || len(script.Tools) != 2 || string(script.Tools[0].Output) != `{"id":1}` || !script.Tools[1].IsError {
		t.Fatalf("script = %+v", script)
	}
	got := view.(api.GoldenCaseSet).Cases[0]
	if len(got.Turns) != 1 || got.Turns[0] != "and then?" || len(got.Tools) != 2 || got.Tools[0].ToolID != "orders.get" || !got.Tools[1].IsError {
		t.Fatalf("case = %+v", got)
	}
}

func TestStudioEvaluationHandlerReadsATrajectoryByArm(t *testing.T) {
	backend := &scriptedBackendFake{}
	h := &StudioEvaluationHandler{Backend: backend}
	view, err := h.trajectory(context.Background(), domain.StudioActor{}, url.Values{"run_id": {"11"}, "case_id": {"refund"}})
	if err != nil {
		t.Fatal(err)
	}
	trajectory := view.(api.CaseTrajectory)
	if backend.role != domain.RoleCandidate || trajectory.Arm != "b" || trajectory.RunID != 11 || trajectory.CaseID != "refund" || len(trajectory.Events) != 2 {
		t.Fatalf("trajectory = %+v as %s", trajectory, backend.role)
	}
	first := trajectory.Events[0]
	if first.OffsetMs != 1500 || string(first.Payload) != `{"id":1}` || first.Usage == nil || first.Usage.CostMinorUnits != 2 ||
		first.Usage.SearchQueries != 3 || trajectory.Events[1].Usage != nil {
		t.Fatalf("events = %+v", trajectory.Events)
	}
	if view, err = h.trajectory(context.Background(), domain.StudioActor{}, url.Values{"run_id": {"11"}, "case_id": {"refund"}, "arm": {"a"}}); err != nil ||
		backend.role != domain.RoleBaseline || view.(api.CaseTrajectory).Arm != "a" {
		t.Fatalf("arm a = %v as %s", err, backend.role)
	}
	for _, query := range []url.Values{{"run_id": {"x"}}, {"run_id": {"11"}, "arm": {"c"}}} {
		_, err := h.trajectory(context.Background(), domain.StudioActor{}, query)
		var apiErr *keelhandler.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("%v = %v, want 400", query, err)
		}
	}
	if backend.runs != 2 {
		t.Fatalf("a malformed request reached the backend: %d calls", backend.runs)
	}
}
