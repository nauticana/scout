package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// loggedQuery keeps every call's arguments, where queryFake keeps only the last per query.
type loggedQuery struct {
	*queryFake
	calls map[string][][]any
}

func newLoggedQuery(rows map[string][][]any) *loggedQuery {
	return &loggedQuery{queryFake: newQueryFake(rows), calls: map[string][][]any{}}
}

func (query *loggedQuery) Query(ctx context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	query.calls[name] = append(query.calls[name], append([]any(nil), args...))
	return query.queryFake.Query(ctx, name, args...)
}

type loggedDB struct {
	keelport.DatabaseRepository
	query *loggedQuery
}

func (db loggedDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db loggedDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

func comparisonStore(rows map[string][][]any) (*AgentEvaluationStore, *loggedQuery) {
	query := newLoggedQuery(rows)
	return &AgentEvaluationStore{keelStore: keelStore{DB: loggedDB{query: query}}}, query
}

func releaseArm(version string) *domain.EvaluationArm {
	return &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: version}}
}

func TestAgentEvaluationStorePinsBothArmsOfAComparison(t *testing.T) {
	ctx := context.Background()
	store, query := comparisonStore(enqueueRows())
	request := draftEnqueue()
	model := domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}
	request.Draft.Models.Text, request.Subject.Model = &model, &model
	request.Baseline = releaseArm("v2")
	run, err := store.Enqueue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if run.Baseline == nil || run.Baseline.DefinitionDigest != sha256Hex([]byte("v2")) || run.Subject.Model == nil {
		t.Fatalf("run = %+v", run)
	}
	subjects := query.calls[qAgentEvalInsertSubj]
	if len(subjects) != 2 {
		t.Fatalf("pinned %d subjects, want both arms", len(subjects))
	}
	if b := subjects[0]; b[2] != string(domain.SubjectDraft) || b[8] != string(domain.RoleCandidate) || b[9] != nil || b[10] != "provider-b" || b[11] != "model-b" {
		t.Fatalf("arm b = %v", b)
	}
	if a := subjects[1]; a[2] != string(domain.SubjectRelease) || a[4] != "v2" || a[5] != sha256Hex([]byte("v2")) || a[6] != nil || a[8] != string(domain.RoleBaseline) || a[10] != nil {
		t.Fatalf("arm a = %v", a)
	}

	baseline := draftEnqueue()
	baseline.Baseline = &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, AgentID: "writer-a", BaselineID: 5}}
	rows := enqueueRows()
	rows[qAgentEvalBaseline] = [][]any{{"dev"}}
	store, query = comparisonStore(rows)
	if run, err = store.Enqueue(ctx, baseline); err != nil || run.Baseline.BaselineID != 5 || run.Baseline.DefinitionDigest != "" {
		t.Fatalf("baseline run = %+v, %v", run, err)
	}
	if a := query.calls[qAgentEvalInsertSubj][1]; a[2] != string(domain.SubjectBaseline) || a[5] != nil || a[9] != int64(5) || a[8] != string(domain.RoleBaseline) {
		t.Fatalf("baseline arm = %v", a)
	}
	if args := query.calls[qAgentEvalBaseline][0]; args[1] != int64(5) || args[2] != "support" || args[3] != int64(3) {
		t.Fatalf("baseline lookup = %v", args)
	}
}

func TestAgentEvaluationStoreRefusesUnpinnableComparisons(t *testing.T) {
	ctx := context.Background()
	refuse := func(name string, rows map[string][][]any, request domain.AgentEvaluationEnqueue, want error) {
		t.Helper()
		store, query := comparisonStore(rows)
		if _, err := store.Enqueue(ctx, request); !errors.Is(err, want) {
			t.Fatalf("%s = %v, want %v", name, err, want)
		}
		if len(query.calls[qAgentEvalInsertRun]) != 0 {
			t.Fatalf("%s queued a run", name)
		}
	}
	same := draftEnqueue()
	same.Subject, same.Draft, same.DraftSettings = releaseArm("v2").Subject, nil, nil
	same.Baseline = releaseArm("v2")
	refuse("the same release twice", enqueueRows(), same, domain.ErrValidation)

	candidateBaseline := draftEnqueue()
	candidateBaseline.Subject, candidateBaseline.Draft, candidateBaseline.DraftSettings =
		domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, AgentID: "writer-a", BaselineID: 5}, nil, nil
	candidateBaseline.Baseline = releaseArm("v2")
	refuse("a baseline as arm b", enqueueRows(), candidateBaseline, domain.ErrValidation)

	baseline := draftEnqueue()
	baseline.Baseline = &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, AgentID: "writer-a", BaselineID: 5}}
	refuse("a baseline of another version or tenant", enqueueRows(), baseline, domain.ErrNotFound)
	gateRun := draftEnqueue()
	gateRun.Scope, gateRun.Baseline = domain.GoldenScopeGate, baseline.Baseline
	rows := enqueueRows()
	rows[qAgentEvalBaseline] = [][]any{{"dev"}}
	refuse("a dev baseline in a gate run", rows, gateRun, domain.ErrValidation)

	wrongModel := draftEnqueue()
	wrongModel.Subject.Model = &domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}
	wrongModel.Baseline = releaseArm("v2")
	refuse("a model the frozen draft does not carry", enqueueRows(), wrongModel, domain.ErrValidation)

	modelRelease := draftEnqueue()
	modelRelease.Baseline = releaseArm("v2")
	modelRelease.Baseline.Subject.Model = &domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}
	refuse("a release naming a model", enqueueRows(), modelRelease, domain.ErrValidation)

	missing := draftEnqueue()
	missing.Baseline = releaseArm("v9")
	rows = enqueueRows()
	delete(rows, qAgentEvalRelease)
	refuse("an unknown release", rows, missing, domain.ErrNotFound)
}

func comparisonRunRow(token any) []any {
	row := runRow(domain.EvaluationRunning, token)
	copy(row[23:], []any{nil, nil, string(domain.SubjectBaseline), nil, nil, int64(5), nil, nil})
	return row
}

func TestAgentEvaluationStoreLoadsAComparisonClaim(t *testing.T) {
	ctx := context.Background()
	lease := domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99, TTL: 90 * time.Second}
	definition, _ := json.Marshal(draftEnqueue().Draft)
	settings, _ := json.Marshal(draftSettings())
	store, query := comparisonStore(map[string][][]any{
		qAgentEvalRun:             {comparisonRunRow(int64(99))},
		qAgentEvalDraft:           {{string(definition), string(settings)}},
		qAgentEvalBaselineOutputs: {{"refund", "object://b/x", sha256Hex([]byte("x"))}},
		qAgentEvalCompared:        {{"probe", int64(9), int64(3), int64(12), "EUR", nil}},
	})
	claim, err := store.Load(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Run.Baseline == nil || claim.Baseline == nil || claim.Baseline.Subject.BaselineID != 5 ||
		claim.Baseline.Outputs["refund"].Digest != sha256Hex([]byte("x")) || claim.Draft == nil {
		t.Fatalf("claim = %+v", claim)
	}
	if claim.Recorded["probe"].CostMinorUnits != 12 || len(query.calls[qAgentEvalRecorded]) != 0 {
		t.Fatalf("a comparison must read what it recorded from its verdicts: %+v", claim.Recorded)
	}
	if args := query.calls[qAgentEvalDraft][0]; args[2] != string(domain.RoleCandidate) {
		t.Fatalf("draft args = %v", args)
	}
	if recorded := agentEvaluationQueries[qAgentEvalCompared]; !strings.Contains(recorded, "c.evaluator_cost_minor_units + COALESCE(SUM(x.cost_minor_units), 0)") {
		t.Fatalf("recorded comparison spend must include both arms and the judge: %s", recorded)
	}
}

func testComparison() domain.CaseComparison {
	return domain.CaseComparison{
		ExampleID: "refund", Verdict: domain.ComparisonPrefersB, Confidence: 0.8,
		Evaluator: domain.EvaluatorVersion{Kind: "judge", Version: "pair@p/m/1"}, InputDigest: sha256Hex([]byte("in")), Rationale: "b refunds",
		A: domain.EvaluationResult{ExampleID: "refund", Role: domain.RoleBaseline, Latency: time.Second, Usage: domain.Usage{InputTokens: 10, CostMinorUnits: 4, Currency: "EUR"},
			Output: domain.ObjectRef{URI: "object://out/a", Digest: sha256Hex([]byte("a"))}},
		B: domain.EvaluationResult{ExampleID: "refund", Role: domain.RoleCandidate, Latency: 2 * time.Second, Usage: domain.Usage{InputTokens: 20, CostMinorUnits: 6, Currency: "EUR"},
			Output: domain.ObjectRef{URI: "object://out/b", Digest: sha256Hex([]byte("b"))}},
		EvaluatorUsage: domain.Usage{InputTokens: 30, OutputTokens: 5, CostMinorUnits: 2, Currency: "EUR"},
	}
}

func TestAgentEvaluationStoreRecordsComparisonsUnderTheLease(t *testing.T) {
	ctx := context.Background()
	lease := domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99, TTL: 90 * time.Second}
	store, query := comparisonStore(nil)
	if err := store.RecordComparison(ctx, lease, testComparison()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("record without the lease = %v", err)
	}
	query.rows[qAgentEvalRenew] = [][]any{{"support", int64(3)}}
	if err := store.RecordComparison(ctx, lease, testComparison()); err != nil {
		t.Fatalf("replayed comparison = %v", err)
	}
	if len(query.calls[qAgentEvalPutArm]) != 0 || len(query.calls[qAgentEvalAddTotals]) != 0 {
		t.Fatal("a replayed comparison wrote its arms or totals again")
	}
	query.rows[qAgentEvalPutComparison] = [][]any{{"refund"}}
	if err := store.RecordComparison(ctx, lease, testComparison()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a comparison in another currency than the run = %v", err)
	}
	query.rows[qAgentEvalAddTotals] = [][]any{{int64(11)}}
	query.calls = map[string][][]any{}
	if err := store.RecordComparison(ctx, lease, testComparison()); err != nil {
		t.Fatal(err)
	}
	verdict := query.calls[qAgentEvalPutComparison][0]
	if verdict[5] != string(domain.ComparisonPrefersB) || verdict[6] != 0.8 || verdict[7] != "judge" || verdict[10] != "b refunds" || verdict[13] != int64(2) || verdict[14] != "EUR" || verdict[15] != nil {
		t.Fatalf("verdict args = %v", verdict)
	}
	arms := query.calls[qAgentEvalPutArm]
	if len(arms) != 2 || arms[0][5] != string(domain.RoleBaseline) || arms[0][10] != int64(4) || arms[0][12] != "object://out/a" ||
		arms[1][5] != string(domain.RoleCandidate) || arms[1][7] != int64(2000) || arms[1][13] != sha256Hex([]byte("b")) {
		t.Fatalf("arm args = %v", arms)
	}
	if totals := query.calls[qAgentEvalAddTotals][0]; totals[0] != int64(60) || totals[1] != int64(5) || totals[2] != int64(12) || totals[3] != "EUR" {
		t.Fatalf("the run's totals must count both arms and the judge: %v", totals)
	}

	failed := testComparison()
	failed.Verdict, failed.Evaluator, failed.InputDigest, failed.Reason = "", domain.EvaluatorVersion{}, "", "judging failed: invalid model output"
	if err := store.RecordComparison(ctx, lease, failed); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.CaseComparison){
		"mixed currencies":     func(c *domain.CaseComparison) { c.B.Usage.Currency = "USD" },
		"negative spend":       func(c *domain.CaseComparison) { c.EvaluatorUsage.InputTokens = -1 },
		"unknown verdict":      func(c *domain.CaseComparison) { c.Verdict = "both" },
		"verdict with failure": func(c *domain.CaseComparison) { c.Reason = "judging failed" },
		"silent failure":       func(c *domain.CaseComparison) { c.Verdict = "" },
		"confidence above one": func(c *domain.CaseComparison) { c.Confidence = 1.5 },
		"half an output":       func(c *domain.CaseComparison) { c.A.Output.Digest = "" },
		"half an evaluator":    func(c *domain.CaseComparison) { c.Evaluator.Version = "" },
		"no example":           func(c *domain.CaseComparison) { c.ExampleID = " " },
	} {
		comparison := testComparison()
		mutate(&comparison)
		if err := store.RecordComparison(ctx, lease, comparison); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}

func TestAgentEvaluationStorePagesComparisonsInTheReadersScope(t *testing.T) {
	ctx := context.Background()
	scores := `[{"Metric":"correctness","Value":0.9,"Evaluator":{"Kind":"judge","Version":"j"}}]`
	store, query := comparisonStore(map[string][][]any{
		qAgentEvalComparisons: {{"refund", string(domain.ComparisonPrefersB), "0.8000", "judge", "j", "b refunds", nil, int64(30), int64(5), int64(2), "EUR", int64(2),
			"[]", int64(1000), int64(10), int64(0), int64(4), "EUR", int64(2), "object://out/a", sha256Hex([]byte("a")),
			scores, int64(2000), int64(20), int64(0), int64(6), "EUR", int64(2), "object://out/b", sha256Hex([]byte("b"))}},
		qAgentEvalTally: {{string(domain.ComparisonPrefersB), int64(3)}, {string(domain.ComparisonPrefersA), int64(1)}, {string(domain.ComparisonTie), int64(2)}, {"", int64(1)}},
	})
	comparisons, err := store.Comparisons(ctx, domain.GoldenScopeDev, 7, 11, "zeta", 51)
	if err != nil {
		t.Fatal(err)
	}
	got := comparisons[0]
	if got.Verdict != domain.ComparisonPrefersB || got.Confidence != 0.8 || got.EvaluatorUsage.CostMinorUnits != 2 || got.Exponent != 2 ||
		got.A.Latency != time.Second || got.B.Usage.CostMinorUnits != 6 || len(got.B.Scores) != 1 || got.A.Output.URI != "object://out/a" {
		t.Fatalf("comparison = %+v", got)
	}
	if args := query.calls[qAgentEvalComparisons][0]; args[2] != "dev" || args[3] != "dev" || args[4] != "zeta" || args[6] != 51 {
		t.Fatalf("comparison args = %v", args)
	}
	tally, err := store.Tally(ctx, domain.GoldenScopeDev, 7, 11)
	if err != nil || tally != (domain.ComparisonTally{A: 1, B: 3, Ties: 2, Failed: 1}) {
		t.Fatalf("tally = %+v, %v", tally, err)
	}
	if args := query.calls[qAgentEvalTally][0]; args[2] != "dev" {
		t.Fatalf("tally args = %v", args)
	}
	for _, sql := range []string{agentEvaluationQueries[qAgentEvalComparisons], agentEvaluationQueries[qAgentEvalTally]} {
		if !strings.Contains(sql, "(e.scope_code = ? OR ? = 'gate')") {
			t.Fatalf("a comparison read must hide gate examples from a dev reader: %s", sql)
		}
	}

	query.rows[qAgentEvalComparisons][0][1] = "candidate"
	if _, err := store.Comparisons(ctx, domain.GoldenScopeDev, 7, 11, "zeta", 51); err == nil || !strings.Contains(err.Error(), "unknown verdict") {
		t.Fatalf("legacy verdict = %v", err)
	}
}
