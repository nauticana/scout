package evaluation

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestStudioEvaluationConfiguredPageSizes(t *testing.T) {
	ctx := context.Background()
	service, _, runs, _ := studioEvaluation(grants{domain.StudioView: true})
	service.PageSize, service.MaxPageSize = 2, 5
	runs.results = []domain.AgentCaseResult{{ExampleID: "c"}, {ExampleID: "b"}, {ExampleID: "a"}}
	if page, err := service.Evaluation(ctx, studioActor, 11, "", 0); err != nil || len(page.Results) != 2 || runs.limit != 3 {
		t.Fatalf("default page = %+v (asked for %d), %v", page, runs.limit, err)
	}
	if _, err := service.Evaluation(ctx, studioActor, 11, "", 5); err != nil {
		t.Fatalf("a page at the configured cap: %v", err)
	}
	if _, err := service.Evaluation(ctx, studioActor, 11, "", 6); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a page above the configured cap: %v", err)
	}
	for name, sizes := range map[string][2]int{
		"negative page":       {-1, 0},
		"negative cap":        {0, -1},
		"page above cap":      {10, 5},
		"default above a cap": {0, DefaultStudioPageSize - 1},
	} {
		bad, _, _, _ := studioEvaluation(grants{domain.StudioView: true})
		bad.PageSize, bad.MaxPageSize = sizes[0], sizes[1]
		if _, err := bad.Evaluation(ctx, studioActor, 11, "", 1); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want a validation refusal", name, err)
		}
	}
}

func TestRubricJudgeVerdictBytes(t *testing.T) {
	gateway := &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		return domain.ModelResult{}, nil
	}}
	inputs := objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return nil, domain.ErrNotFound })
	pricer := fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 0, "EUR", nil
	})
	for name, tc := range map[string]struct{ configured, want int }{
		"default":    {want: DefaultRubricVerdictMaxBytes},
		"configured": {configured: 512, want: 512},
	} {
		settings := rubricSettings
		settings.MaxVerdictBytes = tc.configured
		judge, err := NewGatewayRubricJudge(gateway, inputs, pricer, settings)
		if err != nil || judge.settings.MaxVerdictBytes != tc.want {
			t.Fatalf("%s: verdict bytes = %d, %v; want %d", name, judge.settings.MaxVerdictBytes, err, tc.want)
		}
	}
	settings := rubricSettings
	settings.MaxVerdictBytes = -1
	if _, err := NewGatewayRubricJudge(gateway, inputs, pricer, settings); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative verdict bound must be refused, got %v", err)
	}

	judge, _, _ := rubricJudge(t, `{"score":4,"pass":true,"rationale":"Refunds and apologizes."}`, domain.Usage{InputTokens: 1}, nil)
	judge.settings.MaxVerdictBytes = 16
	if _, err := judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample())); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("a verdict above the configured bound must be refused, got %v", err)
	}
}
