package evaluation

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestSkillSelectionScoresTheFirstSkillLoaded(t *testing.T) {
	selection := SkillSelection{
		Template: domain.GoldenExample{TenantID: 1, GoldenSetID: "rinova", SetVersion: 2}, Revision: "choice-v1",
		Payload:    func(request string) ([]byte, error) { return []byte(request), nil },
		PayloadURI: func(exampleID string) string { return "golden/" + exampleID },
	}
	cases, err := selection.Cases([]domain.SkillDefinition{{SkillID: "audit", Examples: []string{"audit /", "audit /about"}}})
	if err != nil || len(cases) != 2 {
		t.Fatalf("Cases = %d, %v", len(cases), err)
	}
	first := cases[0]
	if first.Example.ExampleID != "skill-audit-1" || first.Example.Payload.URI != "golden/skill-audit-1" || len(first.Example.Payload.Digest) != 64 || first.Example.GoldenSetID != "rinova" {
		t.Fatalf("example = %+v", first.Example)
	}
	scores := map[string][]domain.EvaluationScore{}
	for i, loaded := range []string{`{"skill":"audit"}`, `{"skill":"rewrite"}`} {
		trajectory := []domain.TrajectoryEvent{
			{Kind: domain.TrajectoryToolCall, Name: domain.UseSkillToolID, Payload: []byte(loaded)},
			{Kind: domain.TrajectoryToolCall, Name: domain.UseSkillToolID, Payload: []byte(`{"skill":"audit"}`)},
		}
		score, err := cases[i].Heuristic.Evaluate(context.Background(), domain.EvaluationCase{Trajectory: trajectory})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		scores[cases[i].Example.ExampleID] = score
	}
	scores["prompt-injection"] = []domain.EvaluationScore{{Value: 1}}
	if accuracy := selection.Accuracy(scores); accuracy != 0.5 {
		t.Fatalf("accuracy = %v, want only the first choice counted", accuracy)
	}
}
