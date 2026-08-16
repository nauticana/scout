package evaluation

import (
	"context"
	"regexp"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestRegexHeuristicScoresMatchAndForbidden(t *testing.T) {
	required := &RegexHeuristic{Revision: "r1", Pattern: regexp.MustCompile(`(?i)refund`)}
	forbidden := &RegexHeuristic{Revision: "r1", Pattern: regexp.MustCompile(`sk-[a-z0-9]+`), Metric: domain.MetricSafety, Forbidden: true, Critical: true}
	ctx := context.Background()

	scores, err := required.Evaluate(ctx, domain.EvaluationCase{Output: []byte("your Refund is queued")})
	if err != nil || len(scores) != 1 || scores[0].Value != 1 || scores[0].Metric != domain.MetricPatternMatch {
		t.Fatalf("match = %+v, %v", scores, err)
	}
	scores, err = forbidden.Evaluate(ctx, domain.EvaluationCase{Output: []byte("token sk-abc123")})
	if err != nil || scores[0].Value != 0 || !scores[0].Critical || scores[0].Metric != domain.MetricSafety {
		t.Fatalf("forbidden = %+v, %v", scores, err)
	}
	scores, _ = forbidden.Evaluate(ctx, domain.EvaluationCase{Output: []byte("no secret here")})
	if scores[0].Value != 1 || scores[0].Critical {
		t.Fatalf("clean output = %+v", scores)
	}
	if _, err := (&RegexHeuristic{}).Evaluate(ctx, domain.EvaluationCase{}); err == nil {
		t.Fatal("unconfigured heuristic accepted")
	}
}

func TestSchemaHeuristicChecksRequiredKeysAndTypes(t *testing.T) {
	heuristic := &SchemaHeuristic{Revision: "s1", Required: []string{"amount", "currency"}, Types: map[string]string{"amount": "number", "currency": "string"}}
	ctx := context.Background()
	for name, testCase := range map[string]struct {
		output string
		want   float64
	}{
		"valid":       {`{"amount":12,"currency":"EUR"}`, 1},
		"missing key": {`{"amount":12}`, 0},
		"wrong type":  {`{"amount":"12","currency":"EUR"}`, 0},
		"not object":  {`[1,2]`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			scores, err := heuristic.Evaluate(ctx, domain.EvaluationCase{Output: []byte(testCase.output)})
			if err != nil || scores[0].Value != testCase.want {
				t.Fatalf("%s = %+v, %v", name, scores, err)
			}
		})
	}
}

func TestCitationSupportHeuristicScoresSupportedFraction(t *testing.T) {
	heuristic := &CitationSupportHeuristic{Revision: "c1"}
	evalCase := domain.EvaluationCase{
		Output:    []byte("see [doc:a] and [doc:zz] and [doc:a]"),
		Retrieval: []domain.KnowledgeMatch{{DocumentID: "a"}, {DocumentID: "b"}},
	}
	scores, err := heuristic.Evaluate(context.Background(), evalCase)
	if err != nil || scores[0].Value != 0.5 || scores[0].Metric != domain.MetricCitationSupport {
		t.Fatalf("citations = %+v, %v", scores, err)
	}

	strict := &CitationSupportHeuristic{Revision: "c1", RequireCitations: true}
	scores, _ = strict.Evaluate(context.Background(), domain.EvaluationCase{Output: []byte("no citations")})
	if scores[0].Value != 0 {
		t.Fatalf("uncited under strict = %+v", scores)
	}
}
