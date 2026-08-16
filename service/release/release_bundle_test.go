package release

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func testBundle() domain.ReleaseBundle {
	return domain.ReleaseBundle{
		PlatformVersion: "2026.08.1",
		Versions: domain.ComponentVersions{
			Agent: "3", Model: "sonnet-4.6", Prompt: "p-12", Knowledge: "kb-4", Index: "idx-9",
			Tool: "tools-2", Guardrail: "g-7", Evaluator: "eval-3", Release: "2026.08.1",
		},
		ProviderVersion: "anthropic-2026-07-01", Tokenizer: "cl-200k", Runtime: "scout-1.4.0",
		DecodingDefaults: []byte(`{"temperature":0.2,"top_p":0.9}`), Embedding: "embed-3", Reranker: "rerank-2",
		IndexGeneration: "gen-41", ToolVersions: map[string]string{"search": "2", "crm": "5"},
		SafetyPolicyVersion: "safety-2026.08", MigrationSet: []string{"0041", "0042"}, RollbackTarget: "2026.07.9",
		ResidencyPolicy: "eu-only", Provenance: "slsa-3://build/9912", Signature: "sig", SignerKeyID: "key-1",
		CompatibilityConstraints: []byte(`{"min_agent_schema":4}`),
	}
}

func TestCanonicalBundleIsStableAndCoversEveryComponent(t *testing.T) {
	content, digest, err := CanonicalBundle(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 {
		t.Fatalf("digest = %q", digest)
	}
	for _, want := range []string{"sonnet-4.6", "cl-200k", "embed-3", "gen-41", "safety-2026.08", "0042", "2026.07.9", "eu-only", "slsa-3", "min_agent_schema"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("canonical bundle omits %q", want)
		}
	}
	if strings.Contains(string(content), `"sig"`) {
		t.Fatal("signature must not be inside the signed content")
	}
	reordered := testBundle()
	reordered.ToolVersions = map[string]string{"crm": "5", "search": "2"}
	if _, second, err := CanonicalBundle(reordered); err != nil || second != digest {
		t.Fatalf("digest = %q, second = %q, err = %v", digest, second, err)
	}
}

func TestTableReleaseBundleStorePutRejectsBadInput(t *testing.T) {
	store := &TableReleaseBundleStore{DB: dbFake{query: newQueryFake(nil)}}
	cases := map[string]func(*domain.ReleaseBundle){
		"unsigned":         func(bundle *domain.ReleaseBundle) { bundle.Signature = "" },
		"self rollback":    func(bundle *domain.ReleaseBundle) { bundle.RollbackTarget = bundle.PlatformVersion },
		"digest mismatch":  func(bundle *domain.ReleaseBundle) { bundle.Digest = strings.Repeat("a", 64) },
		"invalid decoding": func(bundle *domain.ReleaseBundle) { bundle.DecodingDefaults = []byte("not json") },
		"missing policy":   func(bundle *domain.ReleaseBundle) { bundle.SafetyPolicyVersion = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := testBundle()
			mutate(&bundle)
			if err := store.Put(context.Background(), bundle); err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
}

func TestTableReleaseBundleStoreRoundTripVerifiesDigest(t *testing.T) {
	bundle := testBundle()
	content, digest, err := CanonicalBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	query := newQueryFake(map[string][][]any{qBundleGet: {{digest, "sig", "key-1", "2026.07.9", string(content)}}})
	store := &TableReleaseBundleStore{DB: dbFake{query: query}}
	if err := store.Put(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	requireArgs(t, query, qBundlePut, "2026.08.1", digest, "sig", "key-1", "2026.07.9", string(content))

	loaded, err := store.Get(context.Background(), "2026.08.1")
	if err != nil || loaded.Digest != digest || loaded.RollbackTarget != "2026.07.9" || loaded.Versions.Model != "sonnet-4.6" {
		t.Fatalf("loaded = %+v, err = %v", loaded, err)
	}

	query.rows[qBundleGet] = [][]any{{digest, "sig", "key-1", "2026.07.9", strings.Replace(string(content), "sonnet-4.6", "sonnet-4.7", 1)}}
	if _, err = store.Get(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tampered bundle err = %v", err)
	}
}

func TestBoundedShadowSamplerRespectsAmplificationBound(t *testing.T) {
	now := testStart
	sampler := &BoundedShadowSampler{Percentage: 100, MaxAmplification: 0.5, Window: time.Hour, Now: func() time.Time { return now }}
	sampled := 0
	for index := range 10 {
		request := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 8}, RequestID: string(rune('a' + index))}
		mirrored, err := sampler.Sample(context.Background(), "2026.08.1", request)
		if err != nil {
			t.Fatal(err)
		}
		if mirrored {
			sampled++
		}
	}
	if sampled != 5 {
		t.Fatalf("sampled = %d, want 5", sampled)
	}
	ratio, err := sampler.Amplification(context.Background(), "2026.08.1")
	if err != nil || ratio != 0.5 {
		t.Fatalf("ratio = %v, err = %v", ratio, err)
	}
	// A new window starts clean.
	now = now.Add(2 * time.Hour)
	if ratio, err = sampler.Amplification(context.Background(), "2026.08.1"); err != nil || ratio != 0 {
		t.Fatalf("new window ratio = %v, err = %v", ratio, err)
	}
}

func TestBoundedShadowSamplerRequiresAuthenticatedRequest(t *testing.T) {
	sampler := &BoundedShadowSampler{Percentage: 100, MaxAmplification: 1, Window: time.Hour}
	if _, err := sampler.Sample(context.Background(), "2026.08.1", domain.TurnRequest{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v", err)
	}
}

func TestRollbackDrillReportsEveryCheck(t *testing.T) {
	states := fake.NewRolloutStateStore()
	for _, version := range []string{"2026.08.1", "2026.07.9"} {
		if err := states.Create(context.Background(), domain.RolloutState{PlatformVersion: version, Stage: domain.StageGlobalDefault, Ring: "global"}); err != nil {
			t.Fatal(err)
		}
	}
	drainer, releases, _ := drainFixture(t, domain.StageGlobalDefault, testStart, domain.SessionDrainPolicy{Window: time.Hour})
	harness := &RollbackDrillHarness{
		Bundles:  &fake.ReleaseBundleStore{Bundles: map[string]domain.ReleaseBundle{"2026.08.1": testBundle()}},
		States:   states,
		Aliases:  &fake.PlatformAliasSwitcher{},
		Capacity: &fake.CapacityRestorer{},
		Drain:    drainer,
		Alerts: fake.AlertOwnershipCheckerFunc(func(context.Context, string) (string, error) {
			return "runtime-oncall", nil
		}),
		SampleConversation: releases.Releases["8|conversation-a"],
		Now:                func() time.Time { return testStart },
	}
	report, err := harness.Run(context.Background(), "2026.08.1")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || len(report.Checks) != 5 || report.RollbackTarget != "2026.07.9" {
		t.Fatalf("report = %+v", report)
	}

	harness.Alerts = fake.AlertOwnershipCheckerFunc(func(context.Context, string) (string, error) { return "", nil })
	if report, err = harness.Run(context.Background(), "2026.08.1"); err != nil || report.Passed {
		t.Fatalf("unowned report = %+v, err = %v", report, err)
	}
}

func TestProbeRunnerRunsHoldoutAgainstRollbackTarget(t *testing.T) {
	executed := map[string]string{}
	runner := &ProbeRunner{
		Executor: fake.ContractTestExecutorFunc(func(_ context.Context, platformVersion string, testCase domain.ContractTestCase) (domain.TurnResult, error) {
			executed[testCase.TestCaseID] = platformVersion
			return domain.TurnResult{}, nil
		}),
		Evaluator: fake.ContractAssertionEvaluatorFunc(func(_ context.Context, testCase domain.ContractTestCase, _ domain.TurnResult) ([]string, error) {
			if testCase.TestCaseID == "probe-2" {
				return []string{"tone regression"}, nil
			}
			return nil, nil
		}),
		Bundles: &fake.ReleaseBundleStore{Bundles: map[string]domain.ReleaseBundle{"2026.08.1": testBundle()}},
		Probes: []SyntheticProbe{
			{Case: domain.ContractTestCase{TestCaseID: "probe-1", AgentID: "writer-a", AgentVersion: "3"}},
			{Case: domain.ContractTestCase{TestCaseID: "probe-2", AgentID: "writer-a", AgentVersion: "3"}, Holdout: true},
		},
		Now: func() time.Time { return testStart },
	}
	results, err := runner.Probe(context.Background(), "2026.08.1")
	if err != nil || len(results) != 2 {
		t.Fatalf("results = %+v, err = %v", results, err)
	}
	if executed["probe-1"] != "2026.08.1" || executed["probe-2"] != "2026.07.9" {
		t.Fatalf("executed = %v", executed)
	}
	if !results[0].Passed || results[1].Passed || !results[1].Holdout {
		t.Fatalf("results = %+v", results)
	}
}
