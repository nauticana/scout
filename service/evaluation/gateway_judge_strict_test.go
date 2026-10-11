package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func strictJudge(output string, usage domain.Usage, request *domain.ModelRequest) *GatewayJudge {
	return &GatewayJudge{
		Gateway: &fake.ModelGateway{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, sent domain.ModelRequest) (domain.ModelResult, error) {
			if request != nil {
				*request = sent
			}
			return domain.ModelResult{Output: []byte(output), Usage: usage}, nil
		}},
		Selection: domain.ModelSelection{Provider: "p", Model: "m", ModelVersion: "1"}, PromptVersion: "pair-v1", Seed: 3,
		Metrics: []string{domain.MetricCorrectness},
		Pricer: fake.ModelPricerFunc(func(_ context.Context, _ domain.ModelReference, spend domain.ModelUsage) (int64, string, error) {
			return spend.InputTokens + spend.OutputTokens, "EUR", nil
		}),
	}
}

// The judge charges its principal, prices its spend, constrains its output, and shows the input.
func TestGatewayJudgeChargesThePrincipalAndPricesItsSpend(t *testing.T) {
	var request domain.ModelRequest
	judge := strictJudge(`{"scores":{"A":{"correctness":0.2},"B":{"correctness":0.9}},"preferred":"B","confidence":0.7,"rationale":"B refunds"}`,
		domain.Usage{InputTokens: 30, OutputTokens: 5}, &request)
	judge.Inputs = objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return []byte("ex-1"), nil })
	pair := judgePair()
	pair.Principal = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	verdict, err := judge.Compare(context.Background(), pair)
	if err != nil {
		t.Fatal(err)
	}
	if request.Principal != pair.Principal || request.Output.Mode != domain.OutputModeJSONSchema || request.Output.SchemaName != "pairwise_verdict" ||
		request.Model != (domain.ModelReference{ProviderID: "p", ModelID: "m"}) || request.MaxOutputTokens != defaultJudgeMaxOutputTokens {
		t.Fatalf("request = %+v", request)
	}
	var prompt judgePrompt
	if err := json.Unmarshal(request.Prompt, &prompt); err != nil || prompt.Input != "ex-1" {
		t.Fatalf("prompt input = %q (%v)", prompt.Input, err)
	}
	if verdict.Usage.CostMinorUnits != 35 || verdict.Usage.Currency != "EUR" || verdict.Unpriced || verdict.Rationale != "B refunds" || verdict.Confidence != 0.7 {
		t.Fatalf("verdict = %+v", verdict)
	}
	winner := verdict.Baseline
	if verdict.Preferred == domain.RoleCandidate {
		winner = verdict.Candidate
	}
	if len(winner) != 1 || winner[0].Value != 0.9 {
		t.Fatalf("the preferred role did not receive B's scores: %+v", verdict)
	}
}

func TestGatewayJudgeDefaultsBoundDirectConstruction(t *testing.T) {
	called := false
	judge := strictJudge(`{}`, domain.Usage{}, nil)
	judge.Gateway = &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		called = true
		return domain.ModelResult{}, nil
	}}
	pair := judgePair()
	pair.Baseline.Output = []byte(strings.Repeat("x", defaultJudgeMaxInputBytes))
	if _, err := judge.Compare(context.Background(), pair); !errors.Is(err, domain.ErrValidation) || called {
		t.Fatalf("default input bound = %v (called %v)", err, called)
	}

	judge = strictJudge(`{}`, domain.Usage{}, nil)
	judge.MaxOutputTokens = -1
	if _, err := judge.Compare(context.Background(), judgePair()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative output bound = %v", err)
	}
}

// Anything but a complete verdict is invalid model output, never a verdict, and keeps the spend.
func TestGatewayJudgeRefusesMalformedVerdicts(t *testing.T) {
	for name, output := range map[string]string{
		"not json":         `prefer B`,
		"missing metric":   `{"scores":{"A":{},"B":{"correctness":1}},"preferred":"B","confidence":1}`,
		"score out of 0-1": `{"scores":{"A":{"correctness":2},"B":{"correctness":1}},"preferred":"B","confidence":1}`,
		"unknown winner":   `{"scores":{"A":{"correctness":1},"B":{"correctness":1}},"preferred":"both","confidence":1}`,
		"no confidence":    `{"scores":{"A":{"correctness":1},"B":{"correctness":1}},"preferred":"tie"}`,
		"extra member":     `{"scores":{"A":{"correctness":1},"B":{"correctness":1}},"preferred":"tie","confidence":1,"note":"x"}`,
		"oversized":        `{"scores":{"A":{"correctness":1},"B":{"correctness":1}},"preferred":"tie","confidence":1,"rationale":"` + strings.Repeat("x", 9000) + `"}`,
	} {
		judge := strictJudge(output, domain.Usage{InputTokens: 10}, nil)
		verdict, err := judge.Compare(context.Background(), judgePair())
		if !errors.Is(err, domain.ErrInvalidModelOutput) {
			t.Fatalf("%s = %v, want invalid model output", name, err)
		}
		if verdict.Usage.CostMinorUnits != 10 || verdict.Preferred != "" {
			t.Fatalf("%s kept verdict %+v, want only the spend", name, verdict)
		}
	}
}

func TestGatewayJudgeKeepsHiddenRationaleAndBoundsItsInput(t *testing.T) {
	judge := strictJudge(`{"scores":{"A":{"correctness":1},"B":{"correctness":0}},"preferred":"tie","confidence":0.5,"rationale":"the gate case asks for X"}`, domain.Usage{}, nil)
	pair := judgePair()
	pair.Example.Hidden = true
	verdict, err := judge.Compare(context.Background(), pair)
	if err != nil || verdict.Rationale != "" || verdict.Preferred != "" {
		t.Fatalf("hidden verdict = %+v, %v", verdict, err)
	}
	called := false
	judge = strictJudge(`{}`, domain.Usage{}, nil)
	judge.Gateway = &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		called = true
		return domain.ModelResult{}, nil
	}}
	judge.MaxInputBytes = 64
	if _, err := judge.Compare(context.Background(), judgePair()); !errors.Is(err, domain.ErrValidation) || called {
		t.Fatalf("oversized prompt = %v (called %v)", err, called)
	}
	judge = strictJudge(`{}`, domain.Usage{}, nil)
	judge.Inputs = objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return []byte("tampered"), nil })
	if _, err := judge.Compare(context.Background(), judgePair()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("input not matching its digest = %v", err)
	}
	judge = strictJudge(`{"scores":{"A":{"correctness":1},"B":{"correctness":0}},"preferred":"A","confidence":1}`, domain.Usage{InputTokens: 4}, nil)
	judge.Pricer = nil
	if verdict, err := judge.Compare(context.Background(), judgePair()); err != nil || !verdict.Unpriced || verdict.Usage.InputTokens != 4 {
		t.Fatalf("unpriced spend = %+v, %v", verdict, err)
	}
}
