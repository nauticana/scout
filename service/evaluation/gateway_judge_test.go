package evaluation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func judgePair() domain.EvaluationPair {
	example := testExample("ex-1")
	example.RubricRef = "rubric://billing/v2"
	example.ExpectedBehavior = []byte(`{"must":"refund"}`)
	return domain.EvaluationPair{
		Example:   example,
		Baseline:  domain.EvaluationCase{Role: domain.RoleBaseline, Output: []byte("answer one"), Retrieval: []domain.KnowledgeMatch{{DocumentID: "d1", Content: []byte("evidence")}}},
		Candidate: domain.EvaluationCase{Role: domain.RoleCandidate, Output: []byte("answer two")},
	}
}

func TestGatewayJudgeNeverSeesCandidateLabel(t *testing.T) {
	var prompt []byte
	gateway := &fake.ModelGateway{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
		prompt = append([]byte(nil), request.Prompt...)
		return domain.ModelResult{Output: []byte(`{"scores":{"A":{"correctness":1},"B":{"correctness":0}},"preferred":"A","confidence":0.9}`)}, nil
	}}
	judge := &GatewayJudge{Gateway: gateway, Selection: domain.ModelSelection{Provider: "p", Model: "m", ModelVersion: "1"}, PromptVersion: "judge-v3", Seed: 42, Metrics: []string{domain.MetricCorrectness}}

	verdict, err := judge.Compare(context.Background(), judgePair())
	if err != nil {
		t.Fatal(err)
	}
	text := string(prompt)
	for _, forbidden := range []string{"candidate", "baseline", "Candidate", "Baseline"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("judge prompt leaks the role label %q: %s", forbidden, text)
		}
	}
	var decoded judgePrompt
	if err := json.Unmarshal(prompt, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Outputs) != 2 || decoded.Outputs[0].Label != "A" || decoded.Outputs[1].Label != "B" {
		t.Fatalf("outputs = %+v", decoded.Outputs)
	}
	if decoded.Rubric != "rubric://billing/v2" || decoded.PromptVersion != "judge-v3" {
		t.Fatalf("rubric and prompt version = %+v", decoded)
	}
	if verdict.Preferred != domain.RoleBaseline && verdict.Preferred != domain.RoleCandidate {
		t.Fatalf("preferred = %q", verdict.Preferred)
	}
	if len(verdict.Baseline) != 1 || len(verdict.Candidate) != 1 || verdict.Confidence != 0.9 || len(verdict.InputDigest) != 64 {
		t.Fatalf("verdict = %+v", verdict)
	}
}

// The judge maps A/B back to roles by presentation order, in both orders.
func TestGatewayJudgeMapsBlindedScoresBackToRoles(t *testing.T) {
	pair := judgePair()
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		gateway := &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{Output: []byte(`{"scores":{"A":{"correctness":1},"B":{"correctness":0}},"preferred":"A","confidence":1}`)}, nil
		}}
		judge := &GatewayJudge{Gateway: gateway, Selection: domain.ModelSelection{Provider: "p", Model: "m", ModelVersion: "1"}, PromptVersion: "v", Seed: seed, Metrics: []string{domain.MetricCorrectness}}
		verdict, err := judge.Compare(context.Background(), pair)
		if err != nil {
			t.Fatal(err)
		}
		winner := verdict.Baseline
		if verdict.Preferred == domain.RoleCandidate {
			winner = verdict.Candidate
		}
		if winner[0].Value != 1 {
			t.Fatalf("seed %d: preferred role did not receive the winning score: %+v", seed, verdict)
		}
	}
}

func TestGatewayJudgeOrderIsStableAndSeedDependent(t *testing.T) {
	pair := judgePair()
	base := &GatewayJudge{Seed: 7}
	if base.candidateFirst(pair) != base.candidateFirst(pair) {
		t.Fatal("presentation order is not deterministic for one seed")
	}
	flipped := pair
	flipped.Baseline, flipped.Candidate = pair.Candidate, pair.Baseline
	if base.candidateFirst(pair) == base.candidateFirst(flipped) {
		// Swapping the arms must swap which arm is shown first, never keep the same arm in front.
		t.Fatal("presentation order follows the role rather than the content")
	}
	differing := 0
	for seed := int64(0); seed < 16; seed++ {
		if (&GatewayJudge{Seed: seed}).candidateFirst(pair) {
			differing++
		}
	}
	if differing == 0 || differing == 16 {
		t.Fatalf("order never varies across seeds: %d/16", differing)
	}
}
