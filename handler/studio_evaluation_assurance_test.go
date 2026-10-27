package handler

import (
	"context"
	"slices"
	"testing"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type labeledBackendFake struct {
	evaluationBackendFake
}

func (f *labeledBackendFake) ChangeCases(_ context.Context, _ domain.StudioActor, change domain.GoldenCaseChange) (domain.GoldenCaseSet, error) {
	f.change = change
	return domain.GoldenCaseSet{AgentID: change.AgentID, Cases: []domain.GoldenCase{change.Case, {CaseID: "plain", Name: "Plain", Input: []byte("yo")}}}, nil
}

func TestStudioEvaluationHandlerCarriesAssuranceLabelsBothWays(t *testing.T) {
	backend := &labeledBackendFake{}
	h := &StudioEvaluationHandler{Backend: backend}
	forbidden := false
	view, err := h.changeCases(context.Background(), domain.StudioActor{}, api.GoldenCaseChange{
		AgentName: "writer-a", Op: "put", ExpectedRevision: 4,
		Case: api.GoldenCase{CaseID: "refund", Name: "Refund", Input: "hi", RequiredEvidence: []string{"doc-1"},
			ControllingAuthority: []string{"policy-7"}, ExecutionExpected: &forbidden},
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := backend.change.Case.Assurance
	if !slices.Equal(labels.RequiredEvidence, []string{"doc-1"}) || !slices.Equal(labels.ControllingAuthority, []string{"policy-7"}) ||
		labels.ExecutionExpected == nil || *labels.ExecutionExpected {
		t.Fatalf("labels = %+v", labels)
	}
	cases := view.(api.GoldenCaseSet).Cases
	if got := cases[0]; got.RequiredEvidence[0] != "doc-1" || got.ControllingAuthority[0] != "policy-7" || got.ExecutionExpected == nil || *got.ExecutionExpected {
		t.Fatalf("labeled case = %+v", got)
	}
	if got := cases[1]; got.RequiredEvidence != nil || got.ControllingAuthority != nil || got.ExecutionExpected != nil {
		t.Fatalf("an unlabeled case reads as labeled: %+v", got)
	}
}
