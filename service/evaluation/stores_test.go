package evaluation

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func goldenRow(exampleID, scope string) []any {
	return []any{int64(7), "core", int64(3), exampleID, scope, "curated", "internal", "short", "low", "billing", "en-US", "rubric://r", "expected", "object://b/" + exampleID, sha256Hex([]byte(exampleID)), `[{"Reviewer":"eve","Verdict":"accept"}]`}
}

func TestGoldenSetStoreHidesGateExamplesFromDevScope(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qGoldenExampleList: {goldenRow("open", "dev"), goldenRow("secret", "gate")},
		qGoldenExampleGet:  {goldenRow("secret", "gate")},
	})
	store := &GoldenSetStore{keelStore: keelStore{DB: dbFake{query: query}}}

	if _, err := store.ListExamples(ctx, domain.GoldenScopeDev, 7, "core", 3); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("dev list of a gate row = %v", err)
	}
	if args := query.args[qGoldenExampleList]; len(args) != 5 || args[3] != "dev" || args[4] != "dev" {
		t.Fatalf("list args = %v", args)
	}
	if _, err := store.GetExample(ctx, domain.GoldenScopeDev, 7, "core", 3, "secret"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("dev get of a gate example = %v", err)
	}
	example, err := store.GetExample(ctx, domain.GoldenScopeGate, 7, "core", 3, "secret")
	if err != nil || !example.Hidden || example.Payload.URI != "object://b/secret" || len(example.Reviews) != 1 {
		t.Fatalf("gate get = %+v, %v", example, err)
	}
}

func TestGoldenSetStoreWriteArgumentOrder(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(nil)
	store := &GoldenSetStore{keelStore: keelStore{DB: dbFake{query: query}}}
	hidden := testExample("secret")
	hidden.Hidden = true

	if err := store.PutExample(ctx, domain.GoldenScopeDev, hidden); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("hidden write in dev scope = %v", err)
	}
	if err := store.PutExample(ctx, domain.GoldenScopeGate, hidden); err != nil {
		t.Fatal(err)
	}
	args := query.args[qGoldenExampleInsert]
	if len(args) != 16 || args[0] != int64(7) || args[3] != "secret" || args[4] != "gate" || args[14] != hidden.Payload.Digest {
		t.Fatalf("example insert args = %v", args)
	}

	goldenQuery := domain.GoldenQuery{TenantID: 7, GoldenSetID: "core", SetVersion: 3, QueryID: "q1", KnowledgeBaseID: "kb", Query: []byte("refund policy"),
		Principal: "agent@tenant", Entitlements: []byte(`["billing"]`), ExpectedDocumentIDs: []string{"d1", "d2"}, ExpectAbstention: true}
	if err := store.PutQuery(ctx, domain.GoldenScopeGate, goldenQuery); err != nil {
		t.Fatal(err)
	}
	args = query.args[qGoldenQueryInsert]
	if len(args) != 11 || args[4] != "gate" || args[7] != "agent@tenant" || args[9] != `["d1","d2"]` || args[10] != true {
		t.Fatalf("query insert args = %v", args)
	}
	noEntitlements := goldenQuery
	noEntitlements.Entitlements = nil
	if err := store.PutQuery(ctx, domain.GoldenScopeGate, noEntitlements); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil entitlements = %v", err)
	}

	version := domain.GoldenSetVersion{TenantID: 7, GoldenSetID: "core", SetVersion: 3, DatasetRevision: sha256Hex([]byte("d")), ExampleCount: 2, FrozenAt: testClock}
	if err := store.FreezeVersion(ctx, version); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qGoldenVersionInsert]; len(args) != 6 || args[2] != int64(3) || args[4] != 2 {
		t.Fatalf("freeze args = %v", args)
	}
}

func TestManifestStoreDetectsContentConflictAndReplay(t *testing.T) {
	ctx := context.Background()
	manifest := testManifest(t, testExample("a"))
	encoded, err := canonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	replay := newQueryFake(map[string][][]any{qManifestGet: {{string(encoded)}}})
	store := &ManifestStore{keelStore: keelStore{DB: dbFake{query: replay}}}
	if err := store.Put(ctx, manifest); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, inserted := replay.args[qManifestInsert]; inserted {
		t.Fatal("replay inserted a second manifest row")
	}

	conflicting := newQueryFake(map[string][][]any{qManifestGet: {{`{"ManifestID":"x"}`}}})
	if err := (&ManifestStore{keelStore: keelStore{DB: dbFake{query: conflicting}}}).Put(ctx, manifest); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}

	fresh := newQueryFake(nil)
	if err := (&ManifestStore{keelStore: keelStore{DB: dbFake{query: fresh}}}).Put(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	args := fresh.args[qManifestInsert]
	if len(args) != 11 || args[0] != manifest.ManifestID || args[1] != int64(7) || args[3] != "v2" || args[4] != "v1" || args[7] != manifest.DatasetRevision {
		t.Fatalf("manifest insert args = %v", args)
	}

	loaded := newQueryFake(map[string][][]any{qManifestGet: {{string(encoded)}}})
	got, err := (&ManifestStore{keelStore: keelStore{DB: dbFake{query: loaded}}}).Get(ctx, 7, manifest.ManifestID)
	if err != nil || got.ManifestID != manifest.ManifestID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	tampered := newQueryFake(map[string][][]any{qManifestGet: {{`{"ManifestID":"` + manifest.ManifestID + `","TenantID":7}`}}})
	if _, err := (&ManifestStore{keelStore: keelStore{DB: dbFake{query: tampered}}}).Get(ctx, 7, manifest.ManifestID); err == nil {
		t.Fatal("returned a manifest whose id does not match its content")
	}
}

func TestResultStoreRunLifecycleArgumentOrder(t *testing.T) {
	ctx := context.Background()
	manifest := testManifest(t, testExample("a"))
	query := newQueryFake(map[string][][]any{
		qRunFinish: {{int64(1)}},
		qRunGetSet: {{manifest.ManifestID, "core", int64(3)}},
	})
	store := &ResultStore{keelStore: keelStore{DB: dbFake{query: query}}, Now: fixedClock(testClock)}

	runID, err := store.StartRun(ctx, domain.EvaluationRun{TenantID: 7, ManifestID: manifest.ManifestID, Scope: domain.GoldenScopeGate})
	if err != nil || runID != 1 {
		t.Fatalf("start = %d, %v", runID, err)
	}
	args := query.args[qRunInsert]
	if len(args) != 6 || args[0] != int64(1) || args[1] != int64(7) || args[3] != "gate" || args[4] != runStatusRunning {
		t.Fatalf("run insert args = %v", args)
	}

	results := []domain.EvaluationResult{
		{ManifestID: manifest.ManifestID, ExampleID: "a", Role: domain.RoleCandidate, Latency: 250 * time.Millisecond,
			Usage: domain.Usage{InputTokens: 10, OutputTokens: 5, CostMinorUnits: 3, Currency: "USD"}, NeedsHumanReview: true, Reason: "low confidence",
			Scores: []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: 1}}},
	}
	if err := store.PutResults(ctx, 7, runID, results); err != nil {
		t.Fatal(err)
	}
	args = query.args[qResultInsert]
	if len(args) != 14 || args[0] != int64(1) || args[2] != "core" || args[3] != int64(3) || args[5] != "candidate" || args[7] != int64(250) || args[11] != "USD" || args[12] != true {
		t.Fatalf("result insert args = %v", args)
	}
	foreign := []domain.EvaluationResult{{ManifestID: "other", ExampleID: "a", Role: domain.RoleCandidate}}
	if err := store.PutResults(ctx, 7, runID, foreign); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign manifest = %v", err)
	}

	finished := domain.EvaluationRun{RunID: runID, TenantID: 7, Status: runStatusCompleted, Samples: 4, Usage: domain.Usage{InputTokens: 1, OutputTokens: 2, CostMinorUnits: 3, Currency: "USD"}}
	if err := store.FinishRun(ctx, finished); err != nil {
		t.Fatal(err)
	}
	args = query.args[qRunFinish]
	if len(args) != 9 || args[0] != runStatusCompleted || args[2] != 4 || args[6] != "USD" || args[7] != int64(7) || args[8] != int64(1) {
		t.Fatalf("finish args = %v", args)
	}
	if err := store.FinishRun(ctx, domain.EvaluationRun{RunID: 1, TenantID: 7, Status: "weird"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad status = %v", err)
	}
}

func TestGateDecisionStoreRoundTripsSignedPayload(t *testing.T) {
	ctx := context.Background()
	manifest := testManifest(t, testExample("a"))
	issuer := newIssuer(&fake.GateDecisionStore{}, &fake.AuditSink{})
	decision, err := issuer.Issue(ctx, manifest, promotableSummary(manifest.ManifestID), "platform-9", testClock, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := DecisionPayload(decision)
	if err != nil {
		t.Fatal(err)
	}
	query := newQueryFake(map[string][][]any{qGateLatest: {{decision.DecisionID, string(payload), hex.EncodeToString(decision.Signature), decision.SignerKeyID}}})
	store := &GateDecisionStore{keelStore: keelStore{DB: dbFake{query: query}}}

	if err := store.Put(ctx, decision); err != nil {
		t.Fatal(err)
	}
	args := query.args[qGateInsert]
	if len(args) != 13 || args[0] != decision.DecisionID || args[3] != "platform-9" || args[5] != string(domain.RolloutHealthy) {
		t.Fatalf("gate insert args = %v", args)
	}
	loaded, err := store.Latest(ctx, "platform-9")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDecision(ctx, testSigner(), loaded); err != nil {
		t.Fatalf("round-tripped decision does not verify: %v", err)
	}
	tampered := decision
	tampered.Confidence = 0.99
	if err := store.Put(ctx, tampered); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("stored a decision whose id does not match its content: %v", err)
	}
}
