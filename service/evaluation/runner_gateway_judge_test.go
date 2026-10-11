package evaluation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/evaluation"
	"github.com/nauticana/scout/service/modelgateway"
)

// judgeGateway is the governed model gateway over one judge route that declares capabilities.
func judgeGateway(t *testing.T, capabilities []domain.ModelCapability, requests *[]domain.ModelRequest) *modelgateway.Gateway {
	t.Helper()
	registry := modelgateway.NewProviderRegistry()
	if err := registry.Register("judges", &fake.ModelProvider{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
		*requests = append(*requests, request)
		return domain.ModelResult{FinishReason: "stop", Usage: domain.Usage{InputTokens: 40, OutputTokens: 10},
			Output: []byte(`{"scores":{"A":{"correctness":0.5},"B":{"correctness":0.5}},"preferred":"tie","confidence":0.9}`)}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	return &modelgateway.Gateway{
		RateLimiter: &fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error { return nil }},
		Providers:   registry,
		Capacity: fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			return &fake.CapacityLease{PoolValue: "evaluation", ReleaseFunc: func(context.Context, domain.Usage) error { return nil }}, nil
		}),
		Catalog: &fake.ModelCandidateCatalog{Set: domain.ModelCandidateSet{Candidates: []domain.ModelCandidate{
			{Provider: "judges", Model: "pair", Capabilities: capabilities},
		}}},
	}
}

func gatewayRunner(gateway contract.ModelGateway, principal domain.PrincipalRef) *evaluation.Runner {
	return &evaluation.Runner{
		Executor: fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, _ domain.GoldenExample, _ []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
			return domain.EvaluationCase{Output: []byte("answer of " + subject.AgentVersion), Latency: time.Millisecond}, nil
		}),
		Judge: &evaluation.GatewayJudge{Gateway: gateway, Selection: domain.ModelSelection{Provider: "judges", Model: "pair", ModelVersion: "1"},
			PromptVersion: "pair-v1", Metrics: []string{domain.MetricCorrectness}, MaxOutputTokens: 256},
		Principal: principal,
		Scorer:    &evaluation.PairedScorer{Seed: 1, Resamples: 50, Policy: domain.PromotionPolicy{MinSamples: 1, Tolerances: map[string]float64{domain.MetricCorrectness: 0.5}}},
	}
}

func gatewayManifest(t *testing.T) (domain.EvaluationManifest, []domain.GoldenExample) {
	t.Helper()
	examples := []domain.GoldenExample{{TenantID: 7, ExampleID: "refund", GoldenSetID: "core", SetVersion: 1, RubricRef: "rubric://refund"}}
	manifest, err := (&evaluation.ManifestBuilder{}).Build(domain.EvaluationManifest{
		TenantID: 7, GoldenSetID: "core", GoldenSetVersion: 1, DatasetRevision: evaluation.DatasetRevision(examples),
		Baseline:   domain.EvaluationSubject{AgentID: "agent", AgentVersion: "v1"},
		Candidate:  domain.EvaluationSubject{AgentID: "agent", AgentVersion: "v2"},
		Evaluators: []domain.EvaluatorVersion{{Kind: "judge", Version: "pair-v1@judges/pair/1"}}, SafetyPolicyVersion: "safety-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest, examples
}

// A manifest Runner judges through the governed gateway when it names a principal and the
// judge route declares structured_output; otherwise it fails closed.
func TestRunnerJudgesThroughTheGovernedGateway(t *testing.T) {
	manifest, examples := gatewayManifest(t)
	principal := domain.PrincipalRef{Kind: domain.PrincipalService, ID: "release-gate"}
	var requests []domain.ModelRequest
	summary, err := gatewayRunner(judgeGateway(t, []domain.ModelCapability{domain.CapabilityTools, domain.CapabilityStructuredOutput}, &requests), principal).Run(context.Background(), manifest, examples)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Principal != principal || requests[0].Output.Mode != domain.OutputModeJSONSchema || summary.Samples != 1 {
		t.Fatalf("requests = %+v, summary = %+v", requests, summary)
	}

	requests = nil
	if _, err := gatewayRunner(judgeGateway(t, []domain.ModelCapability{domain.CapabilityTools}, &requests), principal).Run(context.Background(), manifest, examples); err == nil || len(requests) != 0 {
		t.Fatalf("a judge route without structured_output = %v (called %d times)", err, len(requests))
	}
	if _, err := gatewayRunner(judgeGateway(t, []domain.ModelCapability{domain.CapabilityStructuredOutput}, &requests), domain.PrincipalRef{}).Run(context.Background(), manifest, examples); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a judging runner without a principal = %v", err)
	}
}
