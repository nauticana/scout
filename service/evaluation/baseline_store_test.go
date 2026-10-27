package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func baselineStore(rows map[string][][]any) (*EvaluationBaselineStore, *loggedQuery, *fake.ObjectStorage) {
	query, objects := newLoggedQuery(rows), &fake.ObjectStorage{Name: "baselines"}
	return &EvaluationBaselineStore{DB: loggedDB{query: query}, Storage: objects, Now: fixedClock(testClock)}, query, objects
}

func baselineRows() map[string][][]any {
	return map[string][][]any{
		qBaselineExamples: {{"refund"}, {"greet"}},
		qBaselineCount:    {{int64(0)}},
		qBaselineInsert:   {{int64(1)}},
	}
}

func testBaseline() domain.EvaluationBaseline {
	return domain.EvaluationBaseline{TenantID: 7, GoldenSetID: "support", SetVersion: 3, Name: " Agent desk ", RecordedBy: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}}
}

func baselineOutputs() []domain.BaselineOutput {
	return []domain.BaselineOutput{{CaseID: "refund", Output: []byte("We refund within 5 days.")}, {CaseID: "greet", Output: []byte("Hello!")}}
}

// A baseline answers exactly the cases the recorder can see, content-addressed and digested.
func TestEvaluationBaselineStoreRecordsAnswersToEveryVisibleCase(t *testing.T) {
	ctx := context.Background()
	store, query, objects := baselineStore(baselineRows())
	baseline, err := store.Record(ctx, domain.GoldenScopeDev, testBaseline(), baselineOutputs())
	if err != nil {
		t.Fatal(err)
	}
	if baseline.BaselineID != 1 || baseline.Name != "Agent desk" || baseline.ExampleCount != 2 || baseline.Scope != domain.GoldenScopeDev ||
		!isSHA256Hex(baseline.ContentDigest) || !baseline.RecordedAt.Equal(testClock) {
		t.Fatalf("baseline = %+v", baseline)
	}
	want := sha256Hex([]byte("greet\x00" + sha256Hex([]byte("Hello!")) + "\nrefund\x00" + sha256Hex([]byte("We refund within 5 days.")) + "\n"))
	if baseline.ContentDigest != want {
		t.Fatalf("content digest = %s, want the ordered case and output digests", baseline.ContentDigest)
	}
	if args := query.calls[qBaselineExamples][0]; args[3] != "dev" || args[4] != "dev" {
		t.Fatalf("examples read in scope %v", args)
	}
	outputs := query.calls[qBaselineOutput]
	if len(outputs) != 2 || outputs[0][4] != "greet" || outputs[0][6] != sha256Hex([]byte("Hello!")) ||
		!strings.HasPrefix(outputs[0][5].(string), "object://baselines/evaluation/baseline/7/support/") {
		t.Fatalf("outputs = %v", outputs)
	}
	if len(objects.Objects) != 2 {
		t.Fatalf("stored %d objects", len(objects.Objects))
	}
	if args := query.calls[qBaselineInsert][0]; args[4] != "Agent desk" || args[5] != "dev" || args[8] != "human" || args[9] != "9" {
		t.Fatalf("insert args = %v", args)
	}
}

func TestEvaluationBaselineStoreRefusesIncompleteOrUnboundedBaselines(t *testing.T) {
	ctx := context.Background()
	refuse := func(name string, rows map[string][][]any, scope domain.GoldenScope, baseline domain.EvaluationBaseline, outputs []domain.BaselineOutput, want error) {
		t.Helper()
		store, query, objects := baselineStore(rows)
		store.MaxOutputBytes, store.MaxBaselines = 64, 2
		if _, err := store.Record(ctx, scope, baseline, outputs); !errors.Is(err, want) {
			t.Fatalf("%s = %v, want %v", name, err, want)
		}
		if len(query.calls[qBaselineOutput]) != 0 || len(objects.Uploads) != 0 {
			t.Fatalf("%s wrote outputs or uploaded %v", name, objects.Uploads)
		}
	}
	refuse("a case missing", baselineRows(), domain.GoldenScopeDev, testBaseline(), baselineOutputs()[:1], domain.ErrValidation)
	refuse("a hidden or unknown case", baselineRows(), domain.GoldenScopeDev, testBaseline(),
		append(baselineOutputs(), domain.BaselineOutput{CaseID: "gate-probe", Output: []byte("x")}), domain.ErrValidation)
	refuse("a case twice", baselineRows(), domain.GoldenScopeDev, testBaseline(),
		append(baselineOutputs(), domain.BaselineOutput{CaseID: "refund", Output: []byte("x")}), domain.ErrValidation)
	refuse("an empty answer", baselineRows(), domain.GoldenScopeDev, testBaseline(),
		[]domain.BaselineOutput{{CaseID: "refund"}, {CaseID: "greet", Output: []byte("hi")}}, domain.ErrValidation)
	refuse("an oversized answer", baselineRows(), domain.GoldenScopeDev, testBaseline(),
		[]domain.BaselineOutput{{CaseID: "refund", Output: []byte(strings.Repeat("x", 65))}, {CaseID: "greet", Output: []byte("hi")}}, domain.ErrValidation)
	refuse("an answer that is not text", baselineRows(), domain.GoldenScopeDev, testBaseline(),
		[]domain.BaselineOutput{{CaseID: "refund", Output: []byte{0xff, 0xfe}}, {CaseID: "greet", Output: []byte("hi")}}, domain.ErrValidation)
	blank := testBaseline()
	blank.Name = "  "
	refuse("a blank name", baselineRows(), domain.GoldenScopeDev, blank, baselineOutputs(), domain.ErrValidation)
	anonymous := testBaseline()
	anonymous.RecordedBy = domain.PrincipalRef{}
	refuse("no recorder", baselineRows(), domain.GoldenScopeDev, anonymous, baselineOutputs(), domain.ErrValidation)
	full := baselineRows()
	full[qBaselineCount] = [][]any{{int64(2)}}
	refuse("a full version", full, domain.GoldenScopeDev, testBaseline(), baselineOutputs(), domain.ErrValidation)
	taken := baselineRows()
	taken[qBaselineNamed] = [][]any{{int64(2)}}
	refuse("a taken name", taken, domain.GoldenScopeDev, testBaseline(), baselineOutputs(), domain.ErrConflict)
	store, query, _ := baselineStore(baselineRows())
	delete(query.rows, qBaselineInsert)
	if _, err := store.Record(ctx, domain.GoldenScopeDev, testBaseline(), baselineOutputs()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a name taken while uploading = %v", err)
	}
	unfrozen := baselineRows()
	delete(unfrozen, qBaselineExamples)
	refuse("a version without visible cases", unfrozen, domain.GoldenScopeDev, testBaseline(), baselineOutputs(), domain.ErrNotFound)
	refuse("an unknown scope", baselineRows(), "public", testBaseline(), baselineOutputs(), domain.ErrValidation)
}

func TestEvaluationBaselineStoreListsNewestFirstWithinItsBound(t *testing.T) {
	store, query, _ := baselineStore(map[string][][]any{
		qBaselineList: {{int64(4), "Desk", "dev", int64(2), sha256Hex([]byte("c")), "human", "9", testClock}},
	})
	store.MaxBaselines = 5
	baselines, err := store.List(context.Background(), 7, "support", 3)
	if err != nil || len(baselines) != 1 || baselines[0].BaselineID != 4 || baselines[0].RecordedBy.ID != "9" || baselines[0].SetVersion != 3 {
		t.Fatalf("baselines = %+v, %v", baselines, err)
	}
	if args := query.calls[qBaselineList][0]; args[3] != 5 {
		t.Fatalf("list args = %v", args)
	}
	if _, err := store.List(context.Background(), 7, "support", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no set version = %v", err)
	}
}
