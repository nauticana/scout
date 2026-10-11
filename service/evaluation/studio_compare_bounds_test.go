package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func answeredComparisons(answers map[string]string) []domain.AgentCaseComparison {
	ref := func(uri string) domain.ObjectRef {
		return domain.ObjectRef{URI: uri, Digest: sha256Hex([]byte(answers[uri]))}
	}
	return []domain.AgentCaseComparison{
		{ExampleID: "c", A: domain.AgentCaseResult{Output: ref("object://out/c-a")}, B: domain.AgentCaseResult{Output: ref("object://out/c-b")}},
		{ExampleID: "b", A: domain.AgentCaseResult{Output: ref("object://out/b-a")}, B: domain.AgentCaseResult{}},
	}
}

// A page returns stored answers up to its byte budget and marks the ones it leaves out.
func TestStudioComparisonBoundsThePageAnswers(t *testing.T) {
	ctx := context.Background()
	service, _, runs, _ := studioEvaluation(grants{domain.StudioView: true})
	answers := map[string]string{"object://out/c-a": "1234", "object://out/c-b": strings.Repeat("x", 9), "object://out/b-a": "56"}
	reads := 0
	service.Outputs = objectReaderFunc(func(_ context.Context, ref domain.ObjectRef) ([]byte, error) {
		reads++
		return []byte(answers[ref.URI]), nil
	})
	service.MaxPageAnswerBytes = 6
	runs.run = domain.AgentEvaluationRun{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft},
		Baseline: &domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, BaselineID: 4}}
	runs.comparisons = answeredComparisons(answers)
	page, err := service.Comparison(ctx, studioActor, 11, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	first, second := page.Cases[0], page.Cases[1]
	if string(first.A.Answer) != "1234" || first.A.AnswerOmitted || first.B.Answer != nil || !first.B.AnswerOmitted ||
		string(second.A.Answer) != "56" || second.B.AnswerOmitted {
		t.Fatalf("answers = %+v / %+v", first, second)
	}
	service.MaxPageAnswerBytes = 4
	runs.comparisons = answeredComparisons(answers)
	reads = 0
	if page, err = service.Comparison(ctx, studioActor, 11, "", 10); err != nil {
		t.Fatal(err)
	}
	if !page.Cases[1].A.AnswerOmitted || !page.Cases[0].B.AnswerOmitted || reads != 1 {
		t.Fatalf("a spent budget must not read further answers: omitted %v after %d reads", page.Cases[1].A.AnswerOmitted, reads)
	}
	service.MaxPageAnswerBytes = -1
	if _, err := service.Comparison(ctx, studioActor, 11, "", 10); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative budget = %v", err)
	}
}

// An actor of another tenant reads another tenant's run id as not found.
func TestStudioComparisonOfAnotherTenantIsNotFound(t *testing.T) {
	service, _, runs, _ := studioEvaluation(grants{domain.StudioView: true})
	service.Outputs = objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return nil, nil })
	runs.run = domain.AgentEvaluationRun{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft},
		Baseline: &domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, BaselineID: 4}}
	stranger := studioActor
	stranger.TenantID = 8
	if _, err := service.Comparison(context.Background(), stranger, 11, "", 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's comparison = %v, want not found", err)
	}
	if runs.limit != 0 {
		t.Fatal("another tenant's comparison was paged")
	}
}
