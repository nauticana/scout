package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

var draftDigest = sha256Hex([]byte("draft"))

func draftSettings() []domain.EffectiveResource {
	return []domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_duration_seconds":30}`)}}
}

func draftEnqueue() domain.AgentEvaluationEnqueue {
	definition := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", DefinitionDigest: draftDigest, SettingsDigest: scope.SettingsDigest(draftSettings())}
	return domain.AgentEvaluationEnqueue{
		TenantID: 7, AgentID: "writer-a", SetVersion: 3, Scope: domain.GoldenScopeDev,
		Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft, AgentID: "writer-a", DefinitionDigest: draftDigest},
		Draft:   &definition, DraftSettings: draftSettings(), RequestedBy: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, MaxOpenRuns: 2,
	}
}

func enqueueRows() map[string][][]any {
	return map[string][][]any{
		qAgentEvalLockSet:   {{"support"}},
		qAgentEvalOpenRuns:  {{int64(1)}},
		qAgentEvalVersion:   {{int64(4)}},
		qAgentEvalRelease:   {{sha256Hex([]byte("v2"))}},
		qAgentEvalInsertRun: {{testClock}},
	}
}

func TestAgentEvaluationStorePinsTheDraftAndTheSetVersion(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(enqueueRows())
	store := &AgentEvaluationStore{keelStore: keelStore{DB: dbFake{query: query}}}

	run, err := store.Enqueue(ctx, draftEnqueue())
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != 1 || run.GoldenSetID != "support" || run.Status != domain.EvaluationQueued || !run.QueuedAt.Equal(testClock) || run.Subject.DefinitionDigest != draftDigest {
		t.Fatalf("run = %+v", run)
	}
	if args := query.args[qAgentEvalInsertRun]; args[3] != "support" || args[4] != int64(3) || args[5] != "dev" || args[6] != "human" || args[7] != "9" {
		t.Fatalf("run args = %v", args)
	}
	args := query.args[qAgentEvalInsertSubj]
	var pinned domain.AgentDefinition
	if err := json.Unmarshal([]byte(args[6].(string)), &pinned); err != nil || pinned.DefinitionDigest != draftDigest {
		t.Fatalf("pinned draft = %v (%v)", args[6], err)
	}
	if args[2] != string(domain.SubjectDraft) || args[4] != nil || args[5] != draftDigest {
		t.Fatalf("subject args = %v", args)
	}
	var settings []domain.EffectiveResource
	if err := json.Unmarshal([]byte(args[7].(string)), &settings); err != nil || scope.SettingsDigest(settings) != pinned.SettingsDigest {
		t.Fatalf("pinned draft settings = %v (%v)", args[7], err)
	}
	if args := query.args[qAgentEvalVersion]; args[0] != "dev" || args[2] != int64(7) || args[3] != "support" || args[4] != int64(3) {
		t.Fatalf("version args = %v", args)
	}

	release := draftEnqueue()
	release.Subject, release.Draft, release.DraftSettings = domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v2"}, nil, nil
	run, err = store.Enqueue(ctx, release)
	if err != nil || run.Subject.DefinitionDigest != sha256Hex([]byte("v2")) {
		t.Fatalf("release run = %+v, %v", run, err)
	}
	if args := query.args[qAgentEvalInsertSubj]; args[2] != string(domain.SubjectRelease) || args[4] != "v2" || args[6] != nil || args[7] != nil {
		t.Fatalf("release subject args = %v", args)
	}
}

func TestAgentEvaluationStoreRefusesUnpinnableRuns(t *testing.T) {
	ctx := context.Background()
	refuse := func(name string, rows map[string][][]any, request domain.AgentEvaluationEnqueue, want error) {
		t.Helper()
		query := newQueryFake(rows)
		store := &AgentEvaluationStore{keelStore: keelStore{DB: dbFake{query: query}}}
		if _, err := store.Enqueue(ctx, request); !errors.Is(err, want) {
			t.Fatalf("%s = %v, want %v", name, err, want)
		}
		if _, queued := query.args[qAgentEvalInsertRun]; queued {
			t.Fatalf("%s queued a run", name)
		}
	}
	tampered := draftEnqueue()
	tampered.Subject.DefinitionDigest = sha256Hex([]byte("other"))
	refuse("digest of another definition", enqueueRows(), tampered, domain.ErrValidation)
	both := draftEnqueue()
	both.Subject.Version = "v2"
	refuse("draft naming a release", enqueueRows(), both, domain.ErrValidation)
	widened := draftEnqueue()
	widened.DraftSettings = []domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_duration_seconds":3600}`)}}
	refuse("settings the definition does not digest", enqueueRows(), widened, domain.ErrValidation)
	undigested := draftEnqueue()
	undigested.Draft.SettingsDigest, undigested.DraftSettings = "", nil
	refuse("draft without settings", enqueueRows(), undigested, domain.ErrValidation)
	releaseWithSettings := draftEnqueue()
	releaseWithSettings.Subject, releaseWithSettings.Draft = domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v2"}, nil
	refuse("release carrying draft settings", enqueueRows(), releaseWithSettings, domain.ErrValidation)
	capped := draftEnqueue()
	capped.MaxOpenRuns = 1
	refuse("open-run cap", enqueueRows(), capped, domain.ErrRateLimited)
	zero := draftEnqueue()
	zero.MaxOpenRuns = 0
	refuse("zero cap", enqueueRows(), zero, domain.ErrRateLimited)

	unassociated := enqueueRows()
	delete(unassociated, qAgentEvalLockSet)
	refuse("agent without a set", unassociated, draftEnqueue(), domain.ErrNotFound)
	noVersion := enqueueRows()
	delete(noVersion, qAgentEvalVersion)
	refuse("unknown set version", noVersion, draftEnqueue(), domain.ErrNotFound)
	gateOnly := enqueueRows()
	gateOnly[qAgentEvalVersion] = [][]any{{int64(0)}}
	refuse("version with only gate examples", gateOnly, draftEnqueue(), domain.ErrValidation)
	noRelease := enqueueRows()
	delete(noRelease, qAgentEvalRelease)
	release := draftEnqueue()
	release.Subject, release.Draft, release.DraftSettings = domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v9"}, nil, nil
	refuse("unknown release", noRelease, release, domain.ErrNotFound)
}

func runRow(status domain.EvaluationRunStatus, token any) []any {
	started := testClock
	return []any{int64(11), "writer-a", "support", int64(3), "dev", string(status), testClock, started, nil, "human", "9",
		int64(2), int64(100), int64(40), int64(12), "EUR", int64(2), nil, string(domain.SubjectDraft), nil, draftDigest, token, int64(1),
		nil, nil, nil, nil, nil, nil, nil, nil}
}

func TestAgentEvaluationStoreReadsRunsAndPagesInTheReadersScope(t *testing.T) {
	ctx := context.Background()
	scores := `[{"Metric":"correctness","Value":1,"Evaluator":{"Kind":"heuristic","Version":"h1"},"Rationale":"ok"}]`
	query := newQueryFake(map[string][][]any{
		qAgentEvalRun:     {runRow(domain.EvaluationCompleted, nil)},
		qAgentEvalResults: {{"refund", scores, int64(250), int64(50), int64(20), int64(6), "EUR", int64(2), false, nil, int64(30), int64(4), int64(2), nil, nil}},
	})
	store := &AgentEvaluationStore{keelStore: keelStore{DB: dbFake{query: query}}}
	run, err := store.Run(ctx, domain.GoldenScopeDev, 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if run.Usage.CostMinorUnits != 12 || run.Usage.Currency != "EUR" || run.Exponent != 2 || run.HiddenResults != 1 || run.StartedAt == nil || run.CompletedAt != nil ||
		run.Subject.Kind != domain.SubjectDraft || run.RequestedBy.ID != "9" {
		t.Fatalf("run = %+v", run)
	}
	if args := query.args[qAgentEvalRun]; args[0] != "dev" || args[1] != int64(7) || args[2] != int64(11) {
		t.Fatalf("run args = %v", args)
	}
	results, err := store.Results(ctx, domain.GoldenScopeDev, 7, 11, "zeta", 51)
	if err != nil || len(results) != 1 {
		t.Fatalf("results = %+v, %v", results, err)
	}
	if got := results[0]; got.Latency != 250*time.Millisecond || got.Exponent != 2 || got.Scores[0].Evaluator.Version != "h1" || got.Scores[0].Rationale != "ok" {
		t.Fatalf("result = %+v", got)
	}
	if got := results[0]; got.Usage.CostMinorUnits != 6 || !reflect.DeepEqual(got.EvaluatorUsage, domain.Usage{InputTokens: 30, OutputTokens: 4, CostMinorUnits: 2, Currency: "EUR"}) {
		t.Fatalf("result usage = %+v and %+v, want the subject's and the evaluators' apart", got.Usage, got.EvaluatorUsage)
	}
	if args := query.args[qAgentEvalResults]; args[2] != "dev" || args[3] != "dev" || args[4] != "zeta" || args[5] != "zeta" || args[6] != 51 {
		t.Fatalf("results args = %v", args)
	}
	if _, err := (&AgentEvaluationStore{keelStore: keelStore{DB: dbFake{query: newQueryFake(nil)}}}).Run(ctx, domain.GoldenScopeDev, 7, 12); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's or a manifest run = %v", err)
	}
	legacy := newQueryFake(map[string][][]any{qAgentEvalRun: {runRow("running", nil)}})
	if _, err := (&AgentEvaluationStore{keelStore: keelStore{DB: dbFake{query: legacy}}}).Run(ctx, domain.GoldenScopeDev, 7, 11); err == nil ||
		!strings.Contains(err.Error(), "unknown status") {
		t.Fatalf("a stored word status = %v, want refused", err)
	}
}

func TestAgentEvaluationStoreFencesEveryWorkerWrite(t *testing.T) {
	ctx := context.Background()
	lease := domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99, TTL: 90 * time.Second}
	definition, _ := json.Marshal(domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: draftDigest, SettingsDigest: scope.SettingsDigest(draftSettings())})
	settings, _ := json.Marshal(draftSettings())
	query := newQueryFake(map[string][][]any{
		qAgentEvalRun:      {runRow(domain.EvaluationRunning, int64(99))},
		qAgentEvalDraft:    {{string(definition), string(settings)}},
		qAgentEvalRecorded: {{"refund", int64(5), int64(2), int64(3), "EUR", nil}, {"probe", int64(40), int64(4), int64(7), "EUR", "judging failed: canceled"}},
	})
	store := &AgentEvaluationStore{keelStore: keelStore{DB: dbFake{query: query}}}
	claim, err := store.Load(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Draft == nil || claim.Draft.DefinitionDigest != draftDigest || claim.Recorded["refund"].CostMinorUnits != 3 || claim.Recorded["probe"].CostMinorUnits != 7 ||
		len(claim.DraftSettings) != 1 || string(claim.DraftSettings[0].Value) != `{"max_duration_seconds":30}` {
		t.Fatalf("claim = %+v", claim)
	}
	if len(claim.Unscored) != 1 || claim.Unscored["probe"] != "judging failed: canceled" {
		t.Fatalf("unscored = %v, want only the result recorded with a scoring failure", claim.Unscored)
	}
	if recorded := agentEvaluationQueries[qAgentEvalRecorded]; !strings.Contains(recorded, "cost_minor_units + evaluator_cost_minor_units") {
		t.Fatalf("recorded spend must include the evaluators': %s", recorded)
	}
	widened, _ := json.Marshal([]domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_duration_seconds":3600}`)}})
	query.rows[qAgentEvalDraft] = [][]any{{string(definition), string(widened)}}
	if _, err := store.Load(ctx, lease); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("pinned settings the definition does not digest = %v", err)
	}
	query.rows[qAgentEvalDraft] = [][]any{{string(definition), string(settings)}}
	stolen := lease
	stolen.Token = 98
	if _, err := store.Load(ctx, stolen); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another lease's load = %v", err)
	}
	swapped, _ := json.Marshal(domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: sha256Hex([]byte("other"))})
	query.rows[qAgentEvalDraft] = [][]any{{string(swapped), string(settings)}}
	if _, err := store.Load(ctx, lease); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a pinned draft not matching its digest = %v", err)
	}

	result := domain.EvaluationResult{ExampleID: "late", Role: domain.RoleCandidate, Latency: time.Second,
		Usage: domain.Usage{InputTokens: 4, OutputTokens: 2, CostMinorUnits: 3, Currency: "EUR"}}
	if err := store.Record(ctx, lease, result); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("record without the lease = %v", err)
	}
	if _, inserted := query.args[qAgentEvalPutResult]; inserted {
		t.Fatal("a lost lease wrote a result")
	}
	query.rows[qAgentEvalRenew] = [][]any{{"support", int64(3)}}
	if err := store.Record(ctx, lease, result); err != nil {
		t.Fatalf("replayed result = %v", err)
	}
	if _, added := query.args[qAgentEvalAddTotals]; added {
		t.Fatal("a replayed result was added to the totals twice")
	}
	if args := query.args[qAgentEvalRenew]; args[0] != int64(90) || args[3] != int64(99) {
		t.Fatalf("renew args = %v", args)
	}
	query.rows[qAgentEvalPutResult] = [][]any{{"late"}}
	if err := store.Record(ctx, lease, result); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a result in another currency than the run = %v, want validation, not a lost lease", err)
	}
	free := result
	free.Usage.CostMinorUnits, free.Usage.Currency = 0, "USD"
	if err := store.Record(ctx, lease, free); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("free result with a refusing totals fake = %v", err)
	}
	if args := query.args[qAgentEvalPutResult]; args[10] != nil {
		t.Fatalf("a free result stored currency %v", args[10])
	}
	if args := query.args[qAgentEvalAddTotals]; args[3] != nil || args[6] != nil || args[7] != nil {
		t.Fatalf("a free result can fix the run's currency: %v", args)
	}
	query.rows[qAgentEvalAddTotals] = [][]any{{int64(11)}}
	if err := store.Record(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qAgentEvalAddTotals]; args[2] != int64(3) || args[3] != "EUR" || args[6] != "EUR" {
		t.Fatalf("totals args = %v", args)
	}
	judged := result
	judged.EvaluatorUsage = domain.Usage{InputTokens: 40, OutputTokens: 4, CostMinorUnits: 7, Currency: "EUR"}
	if err := store.Record(ctx, lease, judged); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qAgentEvalPutResult]; args[9] != int64(3) || args[10] != "EUR" || args[13] != int64(40) || args[14] != int64(4) || args[15] != int64(7) {
		t.Fatalf("a judged result did not keep the subject's and the evaluators' spend apart: %v", args)
	}
	if args := query.args[qAgentEvalAddTotals]; args[0] != int64(44) || args[1] != int64(6) || args[2] != int64(10) || args[3] != "EUR" {
		t.Fatalf("the run's totals must count the evaluators' spend: %v", args)
	}
	evaluatorOnly := judged
	evaluatorOnly.Usage = domain.Usage{InputTokens: 4}
	if err := store.Record(ctx, lease, evaluatorOnly); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qAgentEvalPutResult]; args[10] != "EUR" {
		t.Fatalf("priced evaluator spend on a free subject stored currency %v", args[10])
	}
	for name, bad := range map[string]domain.Usage{
		"another currency":  {CostMinorUnits: 7, Currency: "USD"},
		"no currency":       {CostMinorUnits: 7},
		"negative":          {InputTokens: -1},
		"negative searches": {SearchQueries: -1},
		"overflow":          {CostMinorUnits: math.MaxInt64, Currency: "EUR"},
	} {
		refused := result
		refused.EvaluatorUsage = bad
		if err := store.Record(ctx, lease, refused); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s evaluator spend = %v, want validation", name, err)
		}
	}
	priceless := result
	priceless.Usage.Currency = ""
	if err := store.Record(ctx, lease, priceless); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a priced result without currency = %v", err)
	}

	if err := store.Finish(ctx, lease, domain.EvaluationRunning, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("non-terminal finish = %v", err)
	}
	if err := store.Finish(ctx, lease, domain.EvaluationCompleted, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("finish without the lease = %v", err)
	}
	query.rows[qAgentEvalFinish] = [][]any{{int64(11)}}
	if err := store.Finish(ctx, lease, domain.EvaluationFailed, strings.Repeat("é", 300)); err != nil {
		t.Fatal(err)
	}
	if reason := query.args[qAgentEvalFinish][1].(string); len(reason) > 400 || !strings.HasPrefix(reason, "é") {
		t.Fatalf("reason was not truncated on a rune boundary: %d bytes", len(reason))
	}
	if err := store.Finish(ctx, domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99}, domain.EvaluationCompleted, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a lease without TTL = %v", err)
	}
}

func TestAgentEvaluationQueueQueriesBindTokenFirstAndBoundAttempts(t *testing.T) {
	queries, pending, claim, reclaim := AgentEvaluationQueueQueries(90*time.Second, 5, 3)
	if !strings.Contains(queries[pending], "LIMIT 5") || !strings.Contains(queries[pending], "agent_id IS NOT NULL") {
		t.Fatalf("pending = %s", queries[pending])
	}
	if !strings.Contains(queries[claim], "lease_token = ?") || strings.Index(queries[claim], "lease_token = ?") > strings.Index(queries[claim], "id = ?") ||
		!strings.Contains(queries[claim], "INTERVAL '90 seconds'") || !strings.Contains(queries[claim], "RETURNING id, partner_id, lease_token") {
		t.Fatalf("claim = %s", queries[claim])
	}
	if !strings.Contains(queries[reclaim], "attempt >= 3") || !strings.Contains(queries[reclaim], "lease_token IS NOT NULL") {
		t.Fatalf("reclaim must leave manifest runs alone and bound attempts: %s", queries[reclaim])
	}
}
