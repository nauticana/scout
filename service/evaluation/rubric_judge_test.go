package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var rubricSettings = domain.RubricJudgeSettings{
	Model:    domain.ModelSelection{Provider: "judge-co", Model: "grader", ModelVersion: "2026-01"},
	MinScore: 1, MaxScore: 5, PassScore: 4, MaxOutputTokens: 256, MaxInputBytes: 8192,
}

const rubricInput = "Refund order 42, it arrived broken."

var rubricRequester = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}

func rubricExample() domain.GoldenExample {
	example := testExample("refund")
	example.RubricRef = "Offers a refund and apologizes."
	example.ExpectedBehavior = []byte("refund issued")
	example.Payload = domain.ObjectRef{URI: "object://cases/refund", Digest: sha256Hex([]byte(rubricInput))}
	return example
}

type rubricCall struct {
	selection domain.ModelSelection
	request   domain.ModelRequest
}

func rubricJudge(t *testing.T, output string, usage domain.Usage, callErr error) (*GatewayRubricJudge, *[]rubricCall, *int) {
	t.Helper()
	var calls []rubricCall
	gateway := &fake.ModelGateway{GenerateFunc: func(_ context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
		calls = append(calls, rubricCall{selection: selection, request: request})
		return domain.ModelResult{Output: []byte(output), Usage: usage}, callErr
	}}
	priced := 0
	pricer := fake.ModelPricerFunc(func(_ context.Context, reference domain.ModelReference, spend domain.ModelUsage) (int64, string, error) {
		priced++
		if reference != (domain.ModelReference{ProviderID: "judge-co", ModelID: "grader"}) {
			t.Errorf("priced on %+v, want the judge model", reference)
		}
		return spend.InputTokens + 2*spend.OutputTokens, "EUR", nil
	})
	inputs := objectReaderFunc(func(_ context.Context, ref domain.ObjectRef) ([]byte, error) {
		if ref.URI != "object://cases/refund" {
			return nil, domain.ErrNotFound
		}
		return []byte(rubricInput), nil
	})
	judge, err := NewGatewayRubricJudge(gateway, inputs, pricer, rubricSettings)
	if err != nil {
		t.Fatal(err)
	}
	judge.entropy = bytes.NewReader(bytes.Repeat([]byte{7}, 64))
	return judge, &calls, &priced
}

func gradedCase(example domain.GoldenExample) domain.EvaluationCase {
	return domain.EvaluationCase{Example: example, Role: domain.RoleCandidate, Output: []byte("Sorry! I refunded order 42.")}
}

func TestGatewayRubricJudgeGradesThroughThePinnedModel(t *testing.T) {
	judge, calls, priced := rubricJudge(t, `{"score":4,"pass":true,"rationale":"Refunds and apologizes."}`, domain.Usage{InputTokens: 100, OutputTokens: 10}, nil)
	verdict, err := judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample()))
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || *priced != 1 {
		t.Fatalf("calls = %d, priced = %d", len(*calls), *priced)
	}
	call := (*calls)[0]
	request := call.request
	if call.selection != rubricSettings.Model || request.Model != (domain.ModelReference{ProviderID: "judge-co", ModelID: "grader"}) {
		t.Fatalf("judge ran on %+v pinned to %+v", call.selection, request.Model)
	}
	if request.TenantContext.TenantID != 7 || request.Principal != rubricRequester || request.MaxOutputTokens != 256 || !request.Idempotent || !strings.HasPrefix(request.RequestID, "rubric-") {
		t.Fatalf("request = %+v", request)
	}
	if request.Output.Mode != domain.OutputModeJSONSchema || !bytes.Contains(request.Output.Schema, []byte(`"minimum":1,"maximum":5`)) {
		t.Fatalf("output constraint = %+v", request.Output)
	}
	if !bytes.Contains(request.Instructions, []byte("from 1 (worst) to 5 (best)")) || !bytes.Contains(request.Instructions, []byte("4 or higher")) {
		t.Fatalf("instructions = %s", request.Instructions)
	}
	var prompt rubricPrompt
	if err := json.Unmarshal(request.Prompt, &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt != (rubricPrompt{Input: rubricInput, ExpectedBehavior: "refund issued", Rubric: "Offers a refund and apologizes.", Answer: "Sorry! I refunded order 42."}) {
		t.Fatalf("prompt = %+v", prompt)
	}
	version := judge.Version()
	if version.Kind != "rubric_judge" || !strings.HasPrefix(version.Version, "judge-co/grader/2026-01@sha256:") || len(version.Version) != len("judge-co/grader/2026-01@sha256:")+64 {
		t.Fatalf("version = %+v", version)
	}
	if len(verdict.Scores) != 2 || verdict.Unpriced {
		t.Fatalf("verdict = %+v", verdict)
	}
	score, pass := verdict.Scores[0], verdict.Scores[1]
	if score.Metric != domain.MetricRubricScore || score.Value != 0.75 || score.Evaluator != version || score.Rationale != "grade 4 of 1..5: Refunds and apologizes." {
		t.Fatalf("rubric score = %+v", score)
	}
	if pass.Metric != domain.MetricRubricPass || pass.Value != 1 || pass.Evaluator != version {
		t.Fatalf("rubric pass = %+v", pass)
	}
	if usage := verdict.Usage; usage.CostMinorUnits != 120 || usage.Currency != "EUR" || len(usage.Models) != 1 || usage.Models[0].Model.ModelID != "grader" {
		t.Fatalf("judge spend = %+v, want 120 EUR attributed to the judge model", usage)
	}
}

func TestGatewayRubricJudgeVersionChangesWithTheScale(t *testing.T) {
	first, _, _ := rubricJudge(t, "", domain.Usage{}, nil)
	wider := rubricSettings
	wider.MaxScore = 10
	second, err := NewGatewayRubricJudge(first.gateway, first.inputs, first.pricer, wider)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version() == second.Version() {
		t.Fatalf("a different score scale kept version %q", first.Version().Version)
	}
}

func TestGatewayRubricJudgeSkipsAnExampleWithoutARubric(t *testing.T) {
	judge, calls, _ := rubricJudge(t, "", domain.Usage{}, nil)
	example := rubricExample()
	example.RubricRef = "  "
	verdict, err := judge.Grade(context.Background(), rubricRequester, gradedCase(example))
	if err != nil || len(verdict.Scores) != 0 || len(*calls) != 0 {
		t.Fatalf("verdict %+v, err %v, calls %d", verdict, err, len(*calls))
	}
}

// Malformed or out-of-range output is an evaluator error, never a grade, and the spend survives.
func TestGatewayRubricJudgeRefusesMalformedVerdicts(t *testing.T) {
	for name, output := range map[string]string{
		"not json":           `four out of five`,
		"empty":              ``,
		"trailing value":     `{"score":4,"pass":true,"rationale":"ok"} {}`,
		"extra field":        `{"score":4,"pass":true,"rationale":"ok","confidence":1}`,
		"missing pass":       `{"score":4,"rationale":"ok"}`,
		"above range":        `{"score":6,"pass":true,"rationale":"ok"}`,
		"below range":        `{"score":0,"pass":false,"rationale":"ok"}`,
		"fractional":         `{"score":4.5,"pass":true,"rationale":"ok"}`,
		"string score":       `{"score":"4","pass":true,"rationale":"ok"}`,
		"pass disagrees":     `{"score":3,"pass":true,"rationale":"ok"}`,
		"fail disagrees":     `{"score":5,"pass":false,"rationale":"ok"}`,
		"blank rationale":    `{"score":4,"pass":true,"rationale":"   "}`,
		"long rationale":     `{"score":4,"pass":true,"rationale":"` + strings.Repeat("x", rubricRationaleMaxRunes+1) + `"}`,
		"oversized response": `{"score":4,"pass":true,"rationale":"ok"}` + strings.Repeat(" ", DefaultRubricVerdictMaxBytes),
	} {
		judge, _, _ := rubricJudge(t, output, domain.Usage{InputTokens: 50, OutputTokens: 5}, nil)
		verdict, err := judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample()))
		if !errors.Is(err, domain.ErrInvalidModelOutput) || len(verdict.Scores) != 0 {
			t.Fatalf("%s: verdict %+v, err %v", name, verdict, err)
		}
		if verdict.Usage.CostMinorUnits != 60 || verdict.Usage.Currency != "EUR" {
			t.Fatalf("%s: spend %+v was lost", name, verdict.Usage)
		}
		if strings.Contains(err.Error(), "four out of five") {
			t.Fatalf("%s: error %q echoes model output", name, err)
		}
	}

	judge, calls, _ := rubricJudge(t, "", domain.Usage{}, nil)
	judge.gateway = &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		*calls = append(*calls, rubricCall{})
		return domain.ModelResult{Output: []byte(`{"score":4,"pass":true,"rationale":"ok"}`), ToolCalls: []domain.ModelToolCall{{CallID: "c1"}}}, nil
	}}
	if _, err := judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample())); !errors.Is(err, domain.ErrInvalidModelOutput) {
		t.Fatalf("a verdict with tool calls: err %v", err)
	}
}

func TestGatewayRubricJudgeBoundsAndVerifiesItsInput(t *testing.T) {
	judge, calls, _ := rubricJudge(t, `{"score":4,"pass":true,"rationale":"ok"}`, domain.Usage{}, nil)
	long := gradedCase(rubricExample())
	long.Output = bytes.Repeat([]byte("a"), rubricSettings.MaxInputBytes)
	if _, err := judge.Grade(context.Background(), rubricRequester, long); !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "exceeds 8192") {
		t.Fatalf("an oversized judge input: err %v", err)
	}

	tampered := rubricExample()
	tampered.Payload.Digest = sha256Hex([]byte("another input"))
	if _, err := judge.Grade(context.Background(), rubricRequester, gradedCase(tampered)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an input not matching its digest: err %v", err)
	}

	unscoped := rubricExample()
	unscoped.TenantID = 0
	if _, err := judge.Grade(context.Background(), rubricRequester, gradedCase(unscoped)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an example without a tenant: err %v", err)
	}

	missing := rubricExample()
	missing.Payload.URI = "object://cases/other"
	if _, err := judge.Grade(context.Background(), rubricRequester, gradedCase(missing)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unreadable input: err %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("the judge model ran %d times on refused input", len(*calls))
	}
}

// A gate example's grade never carries model text, which could quote its hidden content.
func TestGatewayRubricJudgeOmitsTheRationaleOfAHiddenExample(t *testing.T) {
	judge, _, _ := rubricJudge(t, `{"score":2,"pass":false,"rationale":"It never says: Refund order 42"}`, domain.Usage{}, nil)
	hidden := rubricExample()
	hidden.Hidden = true
	verdict, err := judge.Grade(context.Background(), rubricRequester, gradedCase(hidden))
	if err != nil {
		t.Fatal(err)
	}
	for _, score := range verdict.Scores {
		if strings.Contains(score.Rationale, "Refund order") {
			t.Fatalf("score %+v leaks the hidden example", score)
		}
	}
	if verdict.Scores[0].Rationale != "grade 2 of 1..5" || verdict.Scores[1].Value != 0 {
		t.Fatalf("scores = %+v", verdict.Scores)
	}
}

func TestGatewayRubricJudgePricesItsSpend(t *testing.T) {
	judge, _, priced := rubricJudge(t, `{"score":5,"pass":true,"rationale":"ok"}`, domain.Usage{InputTokens: 10, CostMinorUnits: 3, Currency: "EUR"}, nil)
	verdict, err := judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample()))
	if err != nil || *priced != 0 || verdict.Usage.CostMinorUnits != 3 || verdict.Unpriced {
		t.Fatalf("a provider-priced verdict: %+v, priced %d, err %v", verdict, *priced, err)
	}

	judge, _, _ = rubricJudge(t, `{"score":5,"pass":true,"rationale":"ok"}`, domain.Usage{InputTokens: 10}, nil)
	judge.pricer = fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 0, "", errors.New("no price for grader")
	})
	verdict, err = judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample()))
	if err != nil || !verdict.Unpriced || verdict.Usage.InputTokens != 10 || verdict.Usage.CostMinorUnits != 0 {
		t.Fatalf("an unpriceable verdict: %+v, err %v", verdict, err)
	}

	judge, _, _ = rubricJudge(t, "", domain.Usage{InputTokens: 40}, errors.Join(domain.ErrRateLimited, errors.New("provider text")))
	verdict, err = judge.Grade(context.Background(), rubricRequester, gradedCase(rubricExample()))
	if !errors.Is(err, domain.ErrRateLimited) || verdict.Usage.CostMinorUnits != 40 || len(verdict.Scores) != 0 {
		t.Fatalf("a failed call keeps its spend: %+v, err %v", verdict, err)
	}
}

func TestGatewayRubricJudgeRefusesAnIncompleteComposition(t *testing.T) {
	judge, _, _ := rubricJudge(t, "", domain.Usage{}, nil)
	if _, err := NewGatewayRubricJudge(nil, judge.inputs, judge.pricer, rubricSettings); err == nil {
		t.Fatal("a judge without a gateway was built")
	}
	if _, err := NewGatewayRubricJudge(judge.gateway, judge.inputs, nil, rubricSettings); err == nil {
		t.Fatal("a judge without a pricer was built")
	}
	unpinned := rubricSettings
	unpinned.Model.ModelVersion = ""
	if _, err := NewGatewayRubricJudge(judge.gateway, judge.inputs, judge.pricer, unpinned); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unpinned judge model: err %v", err)
	}
	if _, err := (&GatewayRubricJudge{}).Grade(context.Background(), rubricRequester, gradedCase(rubricExample())); err == nil {
		t.Fatal("a zero-value judge graded")
	}
}

func TestGatewayRubricJudgeRefusesToGradeWithoutAPrincipal(t *testing.T) {
	judge, calls, _ := rubricJudge(t, `{"score":4,"pass":true,"rationale":"ok"}`, domain.Usage{}, nil)
	for _, principal := range []domain.PrincipalRef{{}, {Kind: domain.PrincipalHuman}, {ID: "9"}, {Kind: domain.PrincipalHuman, ID: " "}} {
		if _, err := judge.Grade(context.Background(), principal, gradedCase(rubricExample())); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("principal %+v: err %v, want forbidden", principal, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("the judge model ran %d times without a principal to charge", len(*calls))
	}
}
