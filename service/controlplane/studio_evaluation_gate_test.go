package controlplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

const (
	devGate        = `{"metric":"rubric_score","min_score_bp":8000,"min_samples":2,"required_scopes":["dev"]}`
	regressionGate = `{"metric":"rubric_score","min_score_bp":8000,"max_regression_bp":500,"min_samples":2,"required_scopes":["dev","gate"]}`
	baseDigest     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// gateQueryFake answers the run and result reads per digest and run id; every other query as studioQueryFake does.
type gateQueryFake struct {
	*studioQueryFake
	runs    map[string]int64
	results map[int64][][]any
	// fail makes a query, or a commit under the key "commit", return its error.
	fail map[string]error
}

func (f *gateQueryFake) Query(ctx context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	recorded, err := f.studioQueryFake.Query(ctx, name, args...)
	if failure := f.fail[name]; failure != nil {
		return nil, failure
	}
	switch name {
	case qGateRun:
		if id, ok := f.runs[args[5].(string)]; ok {
			return &keelmodel.QueryResult{Rows: [][]any{{id}}}, nil
		}
		return &keelmodel.QueryResult{}, nil
	case qGateResults:
		return &keelmodel.QueryResult{Rows: f.results[args[1].(int64)]}, nil
	case qGateDecisionLocked:
		return &keelmodel.QueryResult{Rows: f.rows[qGateDecision]}, nil
	}
	return recorded, err
}

func (f *gateQueryFake) Commit(ctx context.Context) error {
	if failure := f.fail["commit"]; failure != nil {
		return failure
	}
	return f.studioQueryFake.Commit(ctx)
}

// QueryService binds the gate's catalog to a publish or restore transaction.
func (f *gateQueryFake) QueryService(string, map[string]string) keelport.QueryService { return f }

type gateDBFake struct {
	keelport.DatabaseRepository
	qs *gateQueryFake
}

func (f gateDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return f.qs
}
func (f gateDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return f.qs, nil
}

var gateDecidedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func gateSetting(value string) domain.ScopedBinding {
	return domain.ScopedBinding{TenantID: 8, ScopeID: "t:writer", ResourceKind: domain.ResourceEvaluationGate,
		ResourceID: string(domain.ResourceEvaluationGate), MergeMode: domain.MergeReplace, Value: []byte(value)}
}

// gateService composes the gate over the publish fixture with policy bound at the type scope; empty binds none.
func gateService(t *testing.T, policy string) (*StudioEvaluationGate, *gateQueryFake, string) {
	t.Helper()
	studio, query := publishService()
	if policy != "" {
		studio.Scopes = studioScopes([]domain.ScopedBinding{gateSetting(policy)})
	}
	queries := &gateQueryFake{studioQueryFake: query, runs: map[string]int64{}, results: map[int64][][]any{}, fail: map[string]error{}}
	studio.DB = gateDBFake{qs: queries}
	query.rows[qGateSet] = [][]any{{"set-1", int64(3)}}
	query.rows[qGateInsert] = [][]any{{gateDecidedAt}}
	frozen, err := studio.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &StudioEvaluationGate{Studio: studio, Audit: &fake.RecordingAuditSink{}}, queries, frozen.Definition.DefinitionDigest
}

func scoresRow(scores string) []any { return []any{scores} }

func policyDigest(t *testing.T, raw string) string {
	t.Helper()
	policy, err := scope.DecodeEvaluationGate([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := scope.EvaluationGateDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// decisionRow is a stored agent_gate_decision row of devGate.
func decisionRow(verdict domain.EvaluationGateVerdict, reason any, digest, inputs string, base any, setVersion any) []any {
	var set any
	if setVersion != nil {
		set = "set-1"
	}
	return []any{string(verdict), reason, digest, devGate, inputs, base, set, setVersion, "human", "9", reviewedAt}
}

func TestEvaluateGatePassesAndRecordsTheRunsItRead(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	query.runs[digest] = 41
	query.results[41] = [][]any{scoresRow(`[{"Metric":"rubric_score","Value":0.9},{"Metric":"pattern_match","Value":0}]`), scoresRow(`[{"Metric":"rubric_score","Value":0.8}]`)}
	query.rows[qGateInsert] = [][]any{{reviewedAt}}

	state, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	decision := state.Decision
	if decision == nil || decision.Verdict != domain.GateVerdictPass || !state.Admitted || len(state.Stale) != 0 || state.CurrentDigest != digest ||
		len(decision.Runs) != 1 || decision.Runs[0].ScoreBP != 8500 || decision.Runs[0].Samples != 2 || decision.Runs[0].Role != domain.GateRunCandidate {
		t.Fatalf("state = %+v, decision = %+v", state, decision)
	}
	inserted := query.args[qGateInsert]
	if inserted[2] != digest || inserted[3] != string(domain.GateVerdictPass) || inserted[4] != nil || inserted[5] != policyDigest(t, devGate) || inserted[8] != nil ||
		inserted[9] != "set-1" || inserted[10] != int64(3) || inserted[11] != "human" || inserted[12] != "9" {
		t.Fatalf("insert args = %v", inserted)
	}
	if run := query.args[qGateInsertRun]; run[3] != int64(41) || run[4] != string(domain.GateRunCandidate) || run[5] != int64(8500) {
		t.Fatalf("run args = %v", run)
	}
	if query.args[qGateAudit][2] != StudioEventGateEvaluate || query.commits != 1 || !slices.Contains(query.queries, qGateDrop) {
		t.Fatalf("audit %v, commits %d, queries %v", query.args[qGateAudit], query.commits, query.queries)
	}
	if query.args[qGateRun][4] != "dev" || query.args[qGateRun][3] != int64(3) || query.args[qGateResults][2] != DefaultGateMaxResults+1 {
		t.Fatalf("run lookup %v, results %v", query.args[qGateRun], query.args[qGateResults])
	}

	// The same inputs replay the stored decision without writing.
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictPass, nil, policyDigest(t, devGate), inputsDigest(*decision), nil, int64(3))}
	query.queries = nil
	if _, err = gate.EvaluateGate(context.Background(), humanActor(8, 10), "writer-a"); err != nil || slices.Contains(query.queries, qGateInsert) || query.commits != 1 {
		t.Fatalf("replay = %v, queries %v, commits %d", err, query.queries, query.commits)
	}
}

func TestEvaluateGateFailsWithEveryReason(t *testing.T) {
	gate, query, digest := gateService(t, regressionGate)
	query.rows[qGateActive] = [][]any{{"2", baseDigest}}
	query.rows[qGateInsert] = [][]any{{reviewedAt}}
	query.runs[digest], query.runs[baseDigest] = 41, 42
	// A critical failure, and a result the metric did not score, which counts 0.
	query.results[41] = [][]any{scoresRow(`[{"Metric":"rubric_score","Value":0.7,"Critical":true}]`), scoresRow(`[{"Metric":"pattern_match","Value":1}]`)}
	query.results[42] = [][]any{scoresRow(`[{"Metric":"rubric_score","Value":1}]`), scoresRow(`[{"Metric":"rubric_score","Value":1}]`)}

	state, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	decision := state.Decision
	if decision.Verdict != domain.GateVerdictFail || state.Admitted || decision.BaseVersion != "2" || len(decision.Runs) != 4 {
		t.Fatalf("decision = %+v", decision)
	}
	for _, want := range []string{"1 critical failures", "3500 bp is below 8000 bp", "regresses 6500 bp from release 2"} {
		if !strings.Contains(decision.Reason, want) {
			t.Errorf("reason %q lacks %q", decision.Reason, want)
		}
	}
	if args := query.args[qGateInsert]; args[3] != string(domain.GateVerdictFail) || args[8] != "2" {
		t.Fatalf("insert args = %v", args)
	}

	delete(query.runs, baseDigest)
	delete(query.runs, digest)
	query.results[41] = nil
	state, err = gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || !strings.Contains(state.Decision.Reason, "no completed dev evaluation of the draft on set-1 version 3") ||
		!strings.Contains(state.Decision.Reason, "no completed gate evaluation") || len(state.Decision.Runs) != 0 {
		t.Fatalf("missing runs = %+v, %v", state.Decision, err)
	}

	query.rows[qGateSet] = nil
	state, err = gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || state.Decision.Reason != "the agent has no frozen golden set version" || query.args[qGateInsert][9] != nil {
		t.Fatalf("no golden set = %+v, %v", state.Decision, err)
	}
}

func TestEvaluateGateRefusesAgentsWithoutAGateAndOversizedRuns(t *testing.T) {
	gate, _, _ := gateService(t, "")
	if _, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("no gate = %v", err)
	}
	gate, query, digest := gateService(t, devGate)
	gate.MaxResults = 1
	query.runs[digest] = 41
	query.results[41] = [][]any{scoresRow(`[]`), scoresRow(`[]`)}
	if _, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrValidation) || query.commits != 0 {
		t.Fatalf("oversized run = %v", err)
	}
	gate.Studio.Roles = denyActions(domain.StudioTest)
	if _, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("without TEST = %v", err)
	}
}

func TestGateGuardRefusesMissingFailedAndStaleDecisions(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	_, candidate, err := gate.Studio.publishCandidate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Definition.DefinitionDigest != digest {
		t.Fatalf("publish digest %s differs from the evaluated %s", candidate.Definition.DefinitionDigest, digest)
	}
	refusal := func(code string) {
		t.Helper()
		var refused *domain.EvaluationGateRefusal
		err := gate.CheckPublish(context.Background(), candidate)
		if !errors.As(err, &refused) || refused.Code != code || !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("guard = %v, want refusal %s", err, code)
		}
	}
	refusal(domain.GateRefusedMissing)

	current := policyDigest(t, devGate)
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictFail, "dev score 10 bp is below 8000 bp", current, baseDigest, nil, int64(3))}
	refusal(domain.GateRefusedFailed)

	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictPass, nil, current, baseDigest, nil, int64(3))}
	if err := gate.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("a current pass = %v", err)
	}
	query.rows[qGateSet] = [][]any{{"set-1", int64(4)}}
	refusal(domain.GateRefusedStale)
	query.rows[qGateSet] = [][]any{{"set-1", int64(3)}}
	query.rows[qGateActive] = [][]any{{"5", baseDigest}}
	refusal(domain.GateRefusedStale)
	query.rows[qGateActive] = nil
	query.rows[qGateDecision][0][2] = baseDigest
	refusal(domain.GateRefusedStale)

	// An override stands while its policy and active release do, whatever the set version.
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictOverride, "hotfix", current, baseDigest, nil, nil)}
	query.rows[qGateSet] = [][]any{{"set-1", int64(9)}}
	if err := gate.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("a current override = %v", err)
	}

	query.rows[qGateDecision] = nil
	query.queries = nil
	activation := candidate
	activation.Activation = true
	if gate.CheckPublish(context.Background(), activation) != nil || gate.CheckRelease(context.Background(), query, activation) != nil || len(query.queries) != 0 {
		t.Fatalf("activations pass unread: %v", query.queries)
	}
	ungated, _, _ := gateService(t, "")
	if err := ungated.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("no gate = %v", err)
	}
}

func TestPublishRunsTheGateAsAGuard(t *testing.T) {
	gate, query, _ := gateService(t, devGate)
	gate.Studio.Guards = append(gate.Studio.Guards, gate)
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	var refused *domain.EvaluationGateRefusal
	if _, err := gate.Studio.Publish(context.Background(), humanActor(8, 9), request); !errors.As(err, &refused) || slices.Contains(query.queries, qStudioInsertVersion) {
		t.Fatalf("ungated publish = %v", err)
	}
}

func TestOverrideGateNeedsItsGrantAReasonAndTheCurrentDigest(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	query.rows[qGateInsert] = [][]any{{reviewedAt}}
	actor := humanActor(8, 11)
	override := domain.EvaluationGateOverride{AgentID: "writer-a", DefinitionDigest: digest, Reason: " hotfix for incident 12 "}

	audit := gate.Audit
	gate.Audit = nil
	if _, err := gate.OverrideGate(context.Background(), actor, override); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no audit sink = %v", err)
	}
	gate.Audit = audit
	var invalid *StudioValidationError
	if _, err := gate.OverrideGate(context.Background(), actor, domain.EvaluationGateOverride{AgentID: "writer-a", DefinitionDigest: "x"}); !errors.As(err, &invalid) || len(invalid.Fields) != 2 {
		t.Fatalf("blank reason and malformed digest = %v", err)
	}
	stale := override
	stale.DefinitionDigest = baseDigest
	if _, err := gate.OverrideGate(context.Background(), actor, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another digest = %v", err)
	}
	gate.Studio.Roles = denyActions(domain.StudioOverrideGate)
	if _, err := gate.OverrideGate(context.Background(), actor, override); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("without OVERRIDE_GATE = %v", err)
	}
	gate.Studio.Roles = studioRoles

	state, err := gate.OverrideGate(context.Background(), actor, override)
	if err != nil {
		t.Fatal(err)
	}
	if state.Decision.Verdict != domain.GateVerdictOverride || state.Decision.Reason != "hotfix for incident 12" || !state.Admitted {
		t.Fatalf("state = %+v", state)
	}
	events := audit.(*fake.RecordingAuditSink).Events
	if len(events) != 1 || events[0].Category != domain.DecisionCategoryPublication || events[0].Action != "override_evaluation_gate" ||
		events[0].Resource != "agent:writer-a" || events[0].Outcome != domain.DecisionAllow || events[0].Principal.ID != "11" ||
		events[0].PolicyVersion != policyDigest(t, devGate) || events[0].DecisionKey != inputsDigest(*state.Decision) {
		t.Fatalf("audit = %+v", events)
	}
	if args := query.args[qGateInsert]; args[3] != string(domain.GateVerdictOverride) || args[4] != "hotfix for incident 12" || args[12] != "11" ||
		query.args[qGateAudit][2] != StudioEventGateOverride {
		t.Fatalf("insert %v, event %v", args, query.args[qGateAudit])
	}

	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictPass, nil, policyDigest(t, devGate), baseDigest, nil, int64(3))}
	if _, err = gate.OverrideGate(context.Background(), actor, override); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("overriding an admitted draft = %v", err)
	}
}

func TestGateReportsTheCurrentDraftAndItsDecision(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictPass, nil, policyDigest(t, devGate), baseDigest, nil, int64(2))}
	query.rows[qGateDecisionRuns] = [][]any{{int64(41), string(domain.GateRunCandidate), "dev", int64(8500), int64(2), int64(0)}}
	state, err := gate.Gate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if state.CurrentDigest != digest || state.Policy == nil || state.Policy.MinScoreBP != 8000 || state.SetVersion != 3 || state.Admitted ||
		!slices.Equal(state.Stale, []string{domain.GateStaleSetVersion}) || len(state.Decision.Runs) != 1 || state.Decision.Runs[0].ScoreBP != 8500 {
		t.Fatalf("state = %+v", state)
	}
	gate.Studio.Roles = denyActions(domain.StudioView)
	if _, err = gate.Gate(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("without VIEW = %v", err)
	}
}

// A stored decision or run outside the closed code lists is refused, not reported.
func TestGateRefusesUnknownStoredCodes(t *testing.T) {
	gate, query, _ := gateService(t, devGate)
	query.rows[qGateDecision] = [][]any{decisionRow("pass", nil, policyDigest(t, devGate), baseDigest, nil, int64(3))}
	if _, err := gate.Gate(context.Background(), humanActor(8, 9), "writer-a"); err == nil || !strings.Contains(err.Error(), "unknown verdict") {
		t.Fatalf("legacy verdict = %v", err)
	}
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictPass, nil, policyDigest(t, devGate), baseDigest, nil, int64(3))}
	query.rows[qGateDecisionRuns] = [][]any{{int64(41), "candidate", "dev", int64(8500), int64(2), int64(0)}}
	if _, err := gate.Gate(context.Background(), humanActor(8, 9), "writer-a"); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("legacy role = %v", err)
	}
}

// The gate decides whether a release may publish; it is not frozen into it, so changing it leaves the digest alone.
func TestTheEvaluationGateIsNotFrozenIntoTheRelease(t *testing.T) {
	plain, _ := publishService()
	without, err := plain.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	gated, _ := publishService()
	gated.Scopes = studioScopes([]domain.ScopedBinding{gateSetting(regressionGate)})
	with, err := gated.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if with.Definition.DefinitionDigest != without.Definition.DefinitionDigest || len(with.Settings.Resources) != 0 {
		t.Fatalf("gate frozen: %s vs %s, settings %+v", with.Definition.DefinitionDigest, without.Definition.DefinitionDigest, with.Settings.Resources)
	}
	settings, err := gated.Settings(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || len(settings.Effective) != 1 || settings.Effective[0].ResourceKind != domain.ResourceEvaluationGate {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	if _, err = settingBinding(humanActor(8, 9), domain.PromptScopes{AgentScopeID: "a:writer-a", TypeScopeID: "t:writer"}, domain.AgentSettingChange{
		AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourceEvaluationGate, ResourceID: "gate", Value: []byte(devGate),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a second gate resource id = %v", err)
	}
}

func TestGateAdmitsARestoreOnlyOfADecidedDigestOrOnOverrideGate(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	_, candidate, err := gate.Studio.publishCandidate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	restore := candidate
	restore.RestoredFrom = "1"
	audit := gate.Audit.(*fake.RecordingAuditSink)

	// A pass or override admits the version however stale it is now.
	for _, verdict := range []domain.EvaluationGateVerdict{domain.GateVerdictPass, domain.GateVerdictOverride} {
		query.rows[qGateDecision] = [][]any{decisionRow(verdict, "x", baseDigest, baseDigest, "9", int64(1))}
		if err := gate.CheckRelease(context.Background(), query, restore); err != nil {
			t.Fatalf("%s restore = %v", verdict, err)
		}
	}

	gate.Studio.Roles = denyActions(domain.StudioOverrideGate)
	var refused *domain.EvaluationGateRefusal
	for _, rows := range [][][]any{nil, {decisionRow(domain.GateVerdictFail, "low", baseDigest, baseDigest, nil, int64(1))}} {
		query.rows[qGateDecision] = rows
		if err := gate.CheckPublish(context.Background(), restore); !errors.As(err, &refused) || !strings.Contains(err.Error(), "OVERRIDE_GATE") {
			t.Fatalf("undecided restore = %v", err)
		}
	}

	gate.Studio.Roles = studioRoles
	query.queries = nil
	if err := gate.CheckPublish(context.Background(), restore); err != nil || len(audit.Events) != 0 || slices.Contains(query.queries, qGateAudit) {
		t.Fatalf("the pre-check records nothing: %v, %v", err, query.queries)
	}
	if err := gate.CheckRelease(context.Background(), query, restore); err != nil {
		t.Fatal(err)
	}
	if len(audit.Events) != 1 || audit.Events[0].Action != "restore_over_evaluation_gate" || audit.Events[0].ReleaseVersion != "1" ||
		query.args[qGateAudit][2] != StudioEventGateRestore || !strings.Contains(query.args[qGateAudit][3].(string), digest) {
		t.Fatalf("restore evidence = %+v, event %v", audit.Events, query.args[qGateAudit])
	}
	gate.Audit = nil
	if err := gate.CheckRelease(context.Background(), query, restore); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("restore evidence without an audit sink = %v", err)
	}
}

func TestPublishDecidesTheGateAgainInsideTheTransaction(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	gate.Studio.Guards = []contract.PublishGuard{gate}
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictPass, nil, policyDigest(t, devGate), baseDigest, nil, int64(3))}
	// A restore deployed another release between the guard and the publish transaction.
	query.rows[qGateActiveLocked] = [][]any{{"7", baseDigest}}
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	var refused *domain.EvaluationGateRefusal
	_, err := gate.Studio.Publish(context.Background(), humanActor(8, 9), request)
	if !errors.As(err, &refused) || refused.Code != domain.GateRefusedStale || slices.Contains(query.queries, qStudioInsertVersion) || query.commits != 0 {
		t.Fatalf("publish over a moved deployment = %v, queries %v", err, query.queries)
	}
	if lock := slices.Index(query.queries, qStudioLockDraft); lock < 0 || lock > slices.Index(query.queries, qGateActiveLocked) {
		t.Fatalf("the gate must decide after the draft lock: %v", query.queries)
	}
	query.rows[qGateActiveLocked] = nil
	if _, err = gate.Studio.Publish(context.Background(), humanActor(8, 9), request); err != nil {
		t.Fatalf("admitted publish of %s = %v", digest, err)
	}
}

func TestPublishAndRestoreRefuseABoundGateNoGuardDecides(t *testing.T) {
	gate, query, _ := gateService(t, devGate)
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	if _, err := gate.Studio.Publish(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrNotReady) || query.commits != 0 {
		t.Fatalf("publish without a gate guard = %v", err)
	}
	query.rows[qStudioGetVersion] = [][]any{{`{"agent_id":"writer-a","agent_type_id":"writer","version":"1","definition_digest":"d"}`}}
	if _, err := gate.Studio.Restore(context.Background(), humanActor(8, 9), domain.AgentRestoreRequest{AgentID: "writer-a", Version: "1"}); !errors.Is(err, domain.ErrNotReady) ||
		slices.Contains(query.queries, qStudioGetSettings) {
		t.Fatalf("restore without a gate guard = %v", err)
	}
}

func TestEvaluationKeepsAnOverrideThatStillHoldsOverAFail(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	query.rows[qGateDecision] = [][]any{decisionRow(domain.GateVerdictOverride, "hotfix", policyDigest(t, devGate), baseDigest, nil, nil)}
	state, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || state.Decision.Verdict != domain.GateVerdictOverride || !state.Admitted || slices.Contains(query.queries, qGateInsert) {
		t.Fatalf("override replaced by a fail: %+v, %v", state.Decision, err)
	}
	// A pass still replaces it, and a stale override is replaced by whatever the gate decides.
	query.runs[digest] = 41
	query.results[41] = [][]any{scoresRow(`[{"Metric":"rubric_score","Value":1}]`), scoresRow(`[{"Metric":"rubric_score","Value":1}]`)}
	query.rows[qGateInsert] = [][]any{{reviewedAt}}
	if state, err = gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a"); err != nil || state.Decision.Verdict != domain.GateVerdictPass {
		t.Fatalf("a pass over an override = %+v, %v", state.Decision, err)
	}
	delete(query.runs, digest)
	query.rows[qGateDecision][0][2] = baseDigest
	if state, err = gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a"); err != nil || state.Decision.Verdict != domain.GateVerdictFail {
		t.Fatalf("a fail over a stale override = %+v, %v", state.Decision, err)
	}
}

func TestGateScoringEdges(t *testing.T) {
	gate, query, digest := gateService(t, regressionGate)
	query.rows[qGateActive] = [][]any{{"2", baseDigest}}
	query.rows[qGateInsert] = [][]any{{reviewedAt}}
	query.runs[digest] = 41
	// Values outside 0..1 count 0 and are reported; of two scores of the metric the lowest counts.
	query.results[41] = [][]any{
		scoresRow(`[{"Metric":"rubric_score","Value":1.5}]`),
		scoresRow(`[{"Metric":"rubric_score","Value":0.9},{"Metric":"rubric_score","Value":0.6}]`),
	}
	state, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	reason := state.Decision.Reason
	for _, want := range []string{"1 rubric_score values outside 0..1", "3000 bp is below 8000 bp", "no completed dev evaluation of active release 2"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
	if runs := state.Decision.Runs; len(runs) != 2 || runs[0].ScoreBP != 3000 || runs[0].Role != domain.GateRunCandidate {
		t.Fatalf("only candidate runs without a baseline: %+v", runs)
	}

	zero, query, digest := gateService(t, `{"metric":"rubric_score","required_scopes":["dev"]}`)
	query.rows[qGateInsert] = [][]any{{reviewedAt}}
	query.runs[digest] = 41
	state, err = zero.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || state.Decision.Verdict != domain.GateVerdictFail || !strings.Contains(state.Decision.Reason, "0 samples, fewer than 1") {
		t.Fatalf("a run without results = %+v, %v", state.Decision, err)
	}
}

func TestGateDecisionWritesRollBackOnFailure(t *testing.T) {
	for _, failing := range []string{qGateDrop, qGateInsert, qGateAudit, "commit", "audit"} {
		gate, query, digest := gateService(t, devGate)
		query.rows[qGateInsert] = [][]any{{reviewedAt}}
		query.runs[digest] = 41
		query.results[41] = [][]any{scoresRow(`[{"Metric":"rubric_score","Value":0.1}]`), scoresRow(`[{"Metric":"rubric_score","Value":0.1}]`)}
		cause := errors.New("store down")
		if failing == "audit" {
			gate.Audit = &fake.RecordingAuditSink{Err: cause}
		} else {
			query.fail[failing] = cause
		}
		_, err := gate.OverrideGate(context.Background(), humanActor(8, 11), domain.EvaluationGateOverride{AgentID: "writer-a", DefinitionDigest: digest, Reason: "hotfix"})
		if !errors.Is(err, cause) || query.commits != 0 {
			t.Fatalf("%s failure = %v, commits %d", failing, err, query.commits)
		}
	}
}

func TestSaveSettingNeedsOverrideGateToRemoveOrLoosenAGate(t *testing.T) {
	gateChange := func(scopeID, value string) domain.AgentSettingChange {
		return domain.AgentSettingChange{AgentID: "writer-a", ScopeID: scopeID, ResourceKind: domain.ResourceEvaluationGate,
			ResourceID: string(domain.ResourceEvaluationGate), Value: []byte(value), Remove: value == "",
			ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	}
	strict := `{"metric":"rubric_score","min_score_bp":9000,"required_scopes":["dev"]}`
	own := gateSetting(strict)
	own.ScopeID = "a:writer-a"
	for name, change := range map[string]domain.AgentSettingChange{
		"remove at the type": gateChange("t:writer", ""),
		"remove the agent's": gateChange("a:writer-a", ""),
		"loosen the agent's": gateChange("a:writer-a", `{"metric":"rubric_score","min_score_bp":8500,"required_scopes":["dev"]}`),
		"loosen at the type": gateChange("t:writer", `{"metric":"rubric_score","min_score_bp":1000,"required_scopes":["dev"]}`),
	} {
		service, query := settingsService(nil)
		service.Scopes = chainScopes([]domain.ScopedBinding{gateSetting(devGate), own})
		service.Roles = denyActions(domain.StudioOverrideGate)
		if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrForbidden) || query.commits != 0 {
			t.Fatalf("%s without OVERRIDE_GATE = %v", name, err)
		}
		service.Roles = studioRoles
		if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); !errors.Is(err, domain.ErrNotReady) || query.commits != 0 {
			t.Fatalf("%s without an audit sink = %v", name, err)
		}
		audit := &fake.RecordingAuditSink{}
		service.Audit = audit
		if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), change); err != nil {
			t.Fatalf("%s with OVERRIDE_GATE = %v", name, err)
		}
		if len(audit.Events) != 1 || audit.Events[0].Action != "loosen_evaluation_gate" || audit.Events[0].ScopeID != change.ScopeID ||
			query.args[qStudioAudit][2] != StudioEventGateLoosen || query.commits != 1 {
			t.Fatalf("%s evidence = %+v, event %v", name, audit.Events, query.args[qStudioAudit])
		}
	}

	// Tightening, and adding a gate where none was, need only EDIT.
	for _, bindings := range [][]domain.ScopedBinding{{gateSetting(devGate)}, nil} {
		service, query := settingsService(nil)
		service.Scopes = chainScopes(bindings)
		service.Roles = denyActions(domain.StudioOverrideGate)
		if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), gateChange("a:writer-a", strict)); err != nil ||
			slices.Contains(query.queries, qStudioAudit) && query.args[qStudioAudit][2] == StudioEventGateLoosen {
			t.Fatalf("tightening = %v", err)
		}
	}
}

func TestImportReplaceNeedsOverrideGateToDropTheAgentsGate(t *testing.T) {
	h := newPortabilityHarness(t)
	agent := h.importable(t)
	own := gateSetting(devGate)
	own.ScopeID = "a:writer-a"
	h.bindings = append(h.bindings, own)

	h.denied[domain.StudioOverrideGate] = true
	if _, err := h.replace(agent, 4); !errors.Is(err, domain.ErrForbidden) || h.db.commits != 0 {
		t.Fatalf("dropping the gate without OVERRIDE_GATE = %v", err)
	}
	h.denied[domain.StudioOverrideGate] = false
	if _, err := h.replace(agent, 4); !errors.Is(err, domain.ErrNotReady) || h.db.commits != 0 {
		t.Fatalf("dropping the gate without an audit sink = %v", err)
	}
	audit := &fake.RecordingAuditSink{}
	h.studio.Audit = audit
	if _, err := h.replace(agent, 4); err != nil {
		t.Fatal(err)
	}
	if len(audit.Events) != 1 || h.db.last(qStudioAudit)[2] != StudioEventGateLoosen || h.db.commits != 1 {
		t.Fatalf("evidence = %+v, event %v", audit.Events, h.db.last(qStudioAudit))
	}
}

func TestAgentPublisherFreezesNoEvaluationGate(t *testing.T) {
	var stored domain.EffectiveRelease
	publisher := &AgentPublisher{
		Compiler: fake.AgentCompilerFunc(func(context.Context, domain.AgentDefinition) (domain.ExecutionGraph, error) { return validGraph(), nil }),
		Store:    fake.AgentPublicationStoreFunc(func(context.Context, int64, domain.AgentDefinition, domain.ExecutionGraph) error { return nil }),
		Effective: fake.EffectiveConfigCompilerFunc(func(_ context.Context, request domain.CompileRequest) (domain.EffectiveRelease, error) {
			return domain.EffectiveRelease{TenantID: request.TenantID, AgentID: request.AgentID, AgentVersion: request.AgentVersion,
				Resources: []domain.EffectiveResource{{ResourceKind: domain.ResourceEvaluationGate, ResourceID: "evaluation_gate", Value: []byte(devGate)},
					{ResourceKind: domain.ResourceTool, ResourceID: "tool", Value: []byte(`["search"]`)}}}, nil
		}),
		Releases: effectiveReleaseSink(func(release domain.EffectiveRelease) error { stored = release; return nil }),
	}
	if err := publisher.Publish(context.Background(), 7, validDefinition()); err != nil {
		t.Fatal(err)
	}
	if len(stored.Resources) != 1 || stored.Resources[0].ResourceKind != domain.ResourceTool || stored.Digest != scope.Digest(stored) {
		t.Fatalf("stored = %+v", stored)
	}
}

// chainScopes is the base layout's chain for writer-a and its type, answering only the bindings of the asked scopes.
func chainScopes(bindings []domain.ScopedBinding) *fake.ScopeRepository {
	return &fake.ScopeRepository{
		ChainFunc: func(_ context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
			chain := domain.ScopeChain{{TenantID: tenantID, ScopeID: RootPromptScopeID, ScopeKind: "tenant"},
				{TenantID: tenantID, ScopeID: "t:writer", ParentScopeID: RootPromptScopeID, ScopeKind: "agent_type"}}
			if scopeID != "t:writer" {
				chain = append(chain, domain.Scope{TenantID: tenantID, ScopeID: scopeID, ParentScopeID: "t:writer", ScopeKind: "agent"})
			}
			return chain, nil
		},
		BindingsFunc: func(_ context.Context, _ int64, ids []string, _ time.Time) ([]domain.ScopedBinding, error) {
			return slices.DeleteFunc(slices.Clone(bindings), func(b domain.ScopedBinding) bool { return !slices.Contains(ids, b.ScopeID) }), nil
		},
	}
}

// A comparison run judges two arms against each other; only a single-subject evaluation is evidence.
func TestGateEvidenceExcludesComparisonRuns(t *testing.T) {
	if sql := gateQueries[qGateRun]; !strings.Contains(sql, "NOT EXISTS") || !strings.Contains(sql, "b.role_code = "+sqlEvaluationBaseline) {
		t.Fatalf("gate run query must skip comparison runs:\n%s", sql)
	}
}

// A first decision recorded concurrently for the same digest is a conflict, not a second row.
func TestEvaluateGateReportsAConcurrentFirstDecision(t *testing.T) {
	gate, query, digest := gateService(t, devGate)
	query.runs[digest] = 41
	query.results[41] = [][]any{scoresRow(`[{"Metric":"rubric_score","Value":0.9}]`)}
	query.rows[qGateInsert] = nil
	if _, err := gate.EvaluateGate(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrConflict) || query.commits != 0 {
		t.Fatalf("error = %v, commits = %d", err, query.commits)
	}
	if !strings.Contains(gateQueries[qGateDecisionLocked], "FOR UPDATE") || strings.Contains(gateQueries[qGateDecisionLocked], "agent_draft") {
		t.Fatalf("recording must lock only the decision row:\n%s", gateQueries[qGateDecisionLocked])
	}
}
