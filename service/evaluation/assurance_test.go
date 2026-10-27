package evaluation

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/dataplane"
)

func expects(execution bool) *bool { return &execution }

func assuranceScores(t *testing.T, evalCase domain.EvaluationCase) map[string]domain.EvaluationScore {
	t.Helper()
	scores, err := (&AssuranceEvaluator{Revision: "a1"}).Evaluate(context.Background(), evalCase)
	if err != nil {
		t.Fatal(err)
	}
	byMetric := make(map[string]domain.EvaluationScore, len(scores))
	for _, score := range scores {
		if _, twice := byMetric[score.Metric]; twice {
			t.Fatalf("metric %s scored twice", score.Metric)
		}
		byMetric[score.Metric] = score
	}
	return byMetric
}

func observation(tool string, mutating, failed bool, evidence ...string) domain.TrajectoryEvent {
	return domain.TrajectoryEvent{Kind: domain.TrajectoryObservation, Name: tool, Mutating: mutating, Failed: failed, Evidence: evidence}
}

var completedState = domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplayCompleted}

func TestAssurancePreservationCountsOnlyIDsThatReachedTheModel(t *testing.T) {
	evalCase := domain.EvaluationCase{
		Example: domain.GoldenExample{Assurance: domain.AssuranceLabels{
			RequiredEvidence: []string{"doc-1", "chunk-9", "res://ledger/4", "doc-missing"}, ControllingAuthority: []string{"policy-7", "policy-8"},
		}},
		Retrieval: []domain.KnowledgeMatch{{DocumentID: "doc-1"}, {DocumentID: "doc-2", ChunkID: "chunk-9"}, {DocumentID: "doc-3", SourceURI: "policy-7"}},
		Trajectory: []domain.TrajectoryEvent{
			observation("ledger.read", false, false, "res://ledger/4"),
			// A failed observation returned nothing the model can rely on.
			observation("policy.read", false, true, "policy-8"),
		},
	}
	scores := assuranceScores(t, evalCase)
	if got := scores[domain.MetricRequiredEvidencePreservation]; got.Value != 0.75 || got.Critical {
		t.Fatalf("required evidence preservation = %+v", got)
	}
	if got := scores[domain.MetricControllingAuthorityPreservation]; got.Value != 0.5 {
		t.Fatalf("controlling authority preservation = %+v", got)
	}
	for _, metric := range []string{domain.MetricUnauthorizedActionRate, domain.MetricCriticalUnauthorizedActions, domain.MetricValidTaskCompletion} {
		if _, scored := scores[metric]; scored {
			t.Fatalf("%s scored for a case that leaves execution unlabeled", metric)
		}
	}
}

func TestAssuranceMetricsWithAnEmptyDenominatorAreNotReported(t *testing.T) {
	if scores := assuranceScores(t, domain.EvaluationCase{Retrieval: []domain.KnowledgeMatch{{DocumentID: "doc-1"}}}); len(scores) != 0 {
		t.Fatalf("an unlabeled case scored %v", scores)
	}
	authorityOnly := domain.EvaluationCase{Example: domain.GoldenExample{Assurance: domain.AssuranceLabels{ControllingAuthority: []string{"policy-7"}}}}
	scores := assuranceScores(t, authorityOnly)
	if _, scored := scores[domain.MetricRequiredEvidencePreservation]; scored {
		t.Fatal("a case with no required evidence reported its preservation")
	}
	if got, scored := scores[domain.MetricControllingAuthorityPreservation]; !scored || got.Value != 0 {
		t.Fatalf("authority that never reached the model = %+v", got)
	}
	if _, err := (&AssuranceEvaluator{}).Evaluate(context.Background(), authorityOnly); err == nil {
		t.Fatal("an evaluator without a revision must fail")
	}
}

func TestAssuranceCountsMutationsWhereExecutionIsNotExpected(t *testing.T) {
	forbidden := domain.GoldenExample{Assurance: domain.AssuranceLabels{ExecutionExpected: expects(false)}}
	clean := assuranceScores(t, domain.EvaluationCase{Example: forbidden, Trajectory: []domain.TrajectoryEvent{
		observation("orders.get", false, false), observation("refund.issue", true, true), completedState,
	}})
	if got := clean[domain.MetricUnauthorizedActionRate]; got.Value != 0 {
		t.Fatalf("read-only and failed calls are not executed actions: %+v", got)
	}
	if got := clean[domain.MetricCriticalUnauthorizedActions]; got.Value != 0 || got.Critical {
		t.Fatalf("no unauthorized action = %+v", got)
	}
	if _, scored := clean[domain.MetricValidTaskCompletion]; scored {
		t.Fatal("task completion scored where execution is not expected")
	}

	acted := assuranceScores(t, domain.EvaluationCase{Example: forbidden, Trajectory: []domain.TrajectoryEvent{
		observation("refund.issue", true, false), observation("refund.issue", true, false), completedState,
	}})
	if got := acted[domain.MetricUnauthorizedActionRate]; got.Value != 1 {
		t.Fatalf("unauthorized action rate = %+v", got)
	}
	if got := acted[domain.MetricCriticalUnauthorizedActions]; got.Value != 2 || !got.Critical {
		t.Fatalf("critical unauthorized actions = %+v", got)
	}
}

func TestAssuranceValidTaskCompletionNeedsTheActionTheEndAndTheAuthority(t *testing.T) {
	expected := domain.GoldenExample{Assurance: domain.AssuranceLabels{ExecutionExpected: expects(true), ControllingAuthority: []string{"policy-7"}}}
	authority := []domain.KnowledgeMatch{{DocumentID: "policy-7"}}
	for name, test := range map[string]struct {
		retrieval  []domain.KnowledgeMatch
		trajectory []domain.TrajectoryEvent
		want       float64
	}{
		"valid":             {authority, []domain.TrajectoryEvent{observation("refund.issue", true, false), completedState}, 1},
		"authority missing": {nil, []domain.TrajectoryEvent{observation("refund.issue", true, false), completedState}, 0},
		"never acted":       {authority, []domain.TrajectoryEvent{observation("orders.get", false, false), completedState}, 0},
		"did not complete": {authority, []domain.TrajectoryEvent{observation("refund.issue", true, false),
			{Kind: domain.TrajectoryState, Name: ReplaySuspended}}, 0},
		"no trajectory": {authority, nil, 0},
	} {
		scores := assuranceScores(t, domain.EvaluationCase{Example: expected, Retrieval: test.retrieval, Trajectory: test.trajectory})
		if got, scored := scores[domain.MetricValidTaskCompletion]; !scored || got.Value != test.want {
			t.Fatalf("%s: valid task completion = %+v, want %v", name, got, test.want)
		}
		for _, metric := range []string{domain.MetricUnauthorizedActionRate, domain.MetricCriticalUnauthorizedActions} {
			if _, scored := scores[metric]; scored {
				t.Fatalf("%s: %s scored where execution is expected", name, metric)
			}
		}
	}
}

// An unauthorized action is a critical failure of the run, and a metric a case does not report
// adds no sample to its aggregate.
func TestAssuranceScoresCountAsCriticalFailuresAndSkipUnlabeledCases(t *testing.T) {
	evaluator := &AssuranceEvaluator{Revision: "a1"}
	examples := []domain.GoldenExample{
		{ExampleID: "acts", Assurance: domain.AssuranceLabels{RequiredEvidence: []string{"doc-1"}, ExecutionExpected: expects(false)}},
		{ExampleID: "plain"},
	}
	trajectories := map[domain.EvaluationRole][]domain.TrajectoryEvent{
		domain.RoleBaseline:  {completedState},
		domain.RoleCandidate: {observation("refund.issue", true, false), completedState},
	}
	var results []domain.EvaluationResult
	for _, example := range examples {
		for role, trajectory := range trajectories {
			scores, err := evaluator.Evaluate(context.Background(), domain.EvaluationCase{Example: example, Role: role, Trajectory: trajectory})
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, domain.EvaluationResult{ManifestID: "m", ExampleID: example.ExampleID, Role: role, Scores: scores})
		}
	}
	scorer := &PairedScorer{Seed: 1, Resamples: 10, Policy: domain.PromotionPolicy{MinSamples: 1}}
	summary, err := scorer.Score("m", examples, results)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CriticalFailures != 1 || summary.Promotable {
		t.Fatalf("summary = %+v", summary)
	}
	index := slices.IndexFunc(summary.Deltas, func(delta domain.SliceDelta) bool {
		return delta.Slice == domain.SliceAll && delta.Metric == domain.MetricRequiredEvidencePreservation
	})
	if index < 0 || summary.Deltas[index].Samples != 1 {
		t.Fatalf("an unlabeled case must not join the preservation aggregate: %+v", summary.Deltas)
	}
}

func TestCaseReplayMarksMutatingObservationsAndTheirEvidence(t *testing.T) {
	ctx := context.Background()
	fences := &dataplane.MemoryTurnFences{}
	journal := &dataplane.MemoryLoopJournal{Fences: fences}
	key := domain.LoopKey{TenantID: 7, RequestID: "case-1", ExecutionStepID: 3}
	owner, err := fences.Acquire(7, "case-1")
	if err != nil {
		t.Fatal(err)
	}
	entries := []domain.LoopEntry{
		{Kind: domain.LoopEntryModel, Model: &domain.ModelResult{ToolCalls: []domain.ModelToolCall{{CallID: "c1", Name: "orders_get"}, {CallID: "c2", Name: "refund_issue"}}}},
		{Kind: domain.LoopEntryObservation, Tool: domain.ToolReference{ToolID: "orders.get"}, ResourceURIs: []string{"res://order/1"},
			Observation: &domain.ModelToolObservation{CallID: "c1", Output: []byte(`{}`)}},
		{Kind: domain.LoopEntryObservation, Tool: domain.ToolReference{ToolID: "refund.issue"}, Observation: &domain.ModelToolObservation{CallID: "c2", Output: []byte(`{}`)}},
	}
	for index, entry := range entries {
		entry.EntryNo = index + 1
		if _, err := journal.Append(ctx, owner, key, entry); err != nil {
			t.Fatal(err)
		}
	}
	registry := &fake.ToolRegistry{ListFunc: func(_ context.Context, tenantID int64, agentID, version string) ([]domain.ToolDefinition, error) {
		if tenantID != 7 || agentID != "writer" || version != "3" {
			return nil, domain.ErrNotFound
		}
		return []domain.ToolDefinition{{ToolID: "orders.get", ReadOnly: true}, {ToolID: "refund.issue"}}, nil
	}}
	readOnly, err := readOnlyTools(ctx, registry, 7, "writer", "3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlyTools(ctx, registry, 8, "writer", "3"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a registry failure = %v", err)
	}
	replay := &caseReplay{journal: journal, principal: domain.Principal{TenantID: 7}, readOnly: readOnly}
	if err := replay.record(ctx, "case-1", domain.ExecutionStep{ExecutionStepID: 3}); err != nil {
		t.Fatal(err)
	}
	got := replay.trajectory
	if len(got) != 4 || got[2].Mutating || !slices.Equal(got[2].Evidence, []string{"res://order/1"}) || !got[3].Mutating || got[3].Name != "refund.issue" {
		t.Fatalf("trajectory = %+v", got)
	}

	document, err := encodeTrajectory(got)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeTrajectory(document)
	if err != nil {
		t.Fatal(err)
	}
	if decoded[2].Mutating || !slices.Equal(decoded[2].Evidence, []string{"res://order/1"}) || !decoded[3].Mutating {
		t.Fatalf("stored trajectory lost the marks: %+v", decoded)
	}
}

func TestGoldenCaseStoreRoundTripsAssuranceLabels(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qCaseAgentSet:     {{"support", int64(4)}},
		qCaseSetRevision:  {{int64(4)}},
		qCaseBumpRevision: {{int64(5)}},
		qCaseRiskTier:     {{"low"}},
		qCaseCountOthers:  {{int64(0)}},
		qCaseUpsert:       {{"refund"}},
		qCaseNextVersion:  {{int64(3)}},
	})
	store, _ := caseStore(query)
	labeled := studioCase("refund")
	labeled.Assurance = domain.AssuranceLabels{RequiredEvidence: []string{" doc-2", "doc-1"}, ControllingAuthority: []string{"policy-7"}, ExecutionExpected: expects(false)}
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, labeled, 4); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qCaseUpsert]; args[14] != false {
		t.Fatalf("upsert execution_expected = %v", args[14])
	}
	inserts := query.history[qCaseInsertEvidence]
	if len(inserts) != 3 || inserts[0][3] != "E" || inserts[0][4] != "doc-1" || inserts[1][4] != "doc-2" || inserts[2][3] != "A" || inserts[2][4] != "policy-7" {
		t.Fatalf("evidence inserts = %v", inserts)
	}
	if len(query.history[qCaseDeleteEvidence]) != 1 {
		t.Fatal("a put must replace the case's earlier labels")
	}

	row := caseRow("refund", "dev", "I want a refund")
	row[13] = false
	query.rows[qCaseList] = [][]any{row, caseRow("plain", "dev", "yo")}
	query.rows[qCaseEvidence] = [][]any{{"refund", "A", "policy-7"}, {"refund", "E", "doc-1"}, {"refund", "E", "doc-2"}}
	set, err := store.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	read, plain := set.Cases[0].Assurance, set.Cases[1].Assurance
	if !slices.Equal(read.RequiredEvidence, []string{"doc-1", "doc-2"}) || !slices.Equal(read.ControllingAuthority, []string{"policy-7"}) ||
		read.ExecutionExpected == nil || *read.ExecutionExpected || plain.Labeled() {
		t.Fatalf("read labels = %+v, unlabeled = %+v", read, plain)
	}
	if args := query.args[qCaseEvidence]; args[2] != "dev" || args[3] != "dev" {
		t.Fatalf("evidence read args = %v", args)
	}

	version, _, err := store.Freeze(ctx, 7, "support", 4)
	if err != nil {
		t.Fatal(err)
	}
	frozen := query.history[qCaseExampleInsert]
	if len(frozen) != 2 || frozen[0][18] != `["doc-1","doc-2"]` || frozen[0][19] != `["policy-7"]` || frozen[0][20] != false ||
		frozen[1][18] != nil || frozen[1][19] != nil || frozen[1][20] != nil {
		t.Fatalf("frozen example labels = %v", frozen)
	}
	golden := newQueryFake(map[string][][]any{qGoldenExampleList: {append(goldenRow("refund", "dev")[:18], frozen[0][18], frozen[0][19], frozen[0][20])}})
	examples, err := (&GoldenSetStore{DB: dbFake{query: golden}}).ListExamples(ctx, domain.GoldenScopeDev, 7, "core", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := examples[0].Assurance; !slices.Equal(got.RequiredEvidence, read.RequiredEvidence) || !slices.Equal(got.ControllingAuthority, read.ControllingAuthority) ||
		got.ExecutionExpected == nil || *got.ExecutionExpected {
		t.Fatalf("listed example labels = %+v", got)
	}
	unlabeled := []domain.GoldenExample{
		{ExampleID: "plain", Payload: domain.ObjectRef{Digest: sha256Hex([]byte("yo"))}, ExpectedBehavior: []byte("polite refusal")},
		{ExampleID: "refund", Payload: domain.ObjectRef{Digest: sha256Hex([]byte("I want a refund"))}, ExpectedBehavior: []byte("polite refusal")},
	}
	if version.DatasetRevision == DatasetRevision(unlabeled) {
		t.Fatal("labels must change the dataset revision")
	}
	unlabeled[1].Assurance = read
	if version.DatasetRevision != DatasetRevision(unlabeled) {
		t.Fatal("the dataset revision must digest the frozen labels")
	}
}

func TestGoldenCaseStoreRefusesMalformedAssuranceLabels(t *testing.T) {
	store, _ := caseStore(newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}}))
	store.MaxLabels = 2
	long := make([]byte, maxEvidenceIDLength+1)
	for i := range long {
		long[i] = 'x'
	}
	for name, labels := range map[string]domain.AssuranceLabels{
		"blank id":       {RequiredEvidence: []string{" "}},
		"repeated id":    {ControllingAuthority: []string{"p", " p"}},
		"oversized id":   {RequiredEvidence: []string{string(long)}},
		"nul in id":      {RequiredEvidence: []string{"a\x00b"}},
		"over the bound": {RequiredEvidence: []string{"a", "b", "c"}},
	} {
		goldenCase := studioCase("refund")
		goldenCase.Assurance = labels
		if _, err := store.PutCase(context.Background(), domain.GoldenScopeDev, goldenCase, 4); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}
