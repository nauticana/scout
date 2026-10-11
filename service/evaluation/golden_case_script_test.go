package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestGoldenCaseStoreReplacesAScriptWithItsCase(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qCaseBumpRevision: {{int64(5)}}, qCaseRiskTier: {{"low"}}, qCaseCountOthers: {{int64(0)}}, qCaseUpsert: {{"refund"}},
	})
	store, _ := caseStore(query)
	scripted := studioCase("refund")
	scripted.Script = refundScript()
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, scripted, 4); err != nil {
		t.Fatal(err)
	}
	key := []any{int64(7), "support", "refund"}
	for _, name := range []string{qCaseDeleteTurns, qCaseDeleteTools} {
		if args := query.args[name]; len(args) != 3 || args[0] != key[0] || args[1] != key[1] || args[2] != key[2] {
			t.Fatalf("%s args = %v", name, args)
		}
	}
	if turns := query.history[qCaseInsertTurn]; len(turns) != 1 || turns[0][3] != 2 || turns[0][4] != "and the second order?" {
		t.Fatalf("turns written = %v", turns)
	}
	tools := query.history[qCaseInsertTool]
	if len(tools) != 2 || tools[0][3] != "orders.get" || tools[0][4] != `{"id":1}` || tools[0][5] != false ||
		tools[1][3] != "refund.issue" || tools[1][4] != nil || tools[1][5] != true {
		t.Fatalf("tools written = %v", tools)
	}

	// An unscripted replace, as a review promotion writes, clears the script.
	plain := newQueryFake(map[string][][]any{
		qCaseBumpRevision: {{int64(6)}}, qCaseRiskTier: {{"low"}}, qCaseCountOthers: {{int64(0)}}, qCaseUpsert: {{"refund"}},
	})
	plainStore, _ := caseStore(plain)
	if _, err := plainStore.PutCase(ctx, domain.GoldenScopeDev, studioCase("refund"), 5); err != nil {
		t.Fatal(err)
	}
	if _, deleted := plain.args[qCaseDeleteTurns]; !deleted || len(plain.history[qCaseInsertTurn]) != 0 || len(plain.history[qCaseInsertTool]) != 0 {
		t.Fatalf("an unscripted replace must clear the old script: %v", plain.history)
	}

	gate := newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}, qCaseRiskTier: {{"low"}}, qCaseCountOthers: {{int64(0)}}})
	gateStore, _ := caseStore(gate)
	if _, err := gateStore.PutCase(ctx, domain.GoldenScopeDev, scripted, 4); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a dev write over a gate case = %v", err)
	}
	if _, written := gate.args[qCaseDeleteTurns]; written {
		t.Fatal("a refused case write touched the script")
	}

	invalid := scripted
	invalid.Script.Tools = append(invalid.Script.Tools, domain.CaseTool{ToolID: "x", Output: json.RawMessage(`nope`)})
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, invalid, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an invalid tool output = %v", err)
	}
	store.MaxTurns = 1
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, scripted, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("over the turn bound = %v", err)
	}
	store.MaxTurns = -1
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, scripted, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative bound = %v", err)
	}

	removed := newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}, qCaseDelete: {{"refund"}}})
	removeStore, _ := caseStore(removed)
	if _, err := removeStore.RemoveCase(ctx, domain.GoldenScopeDev, 7, "support", "refund", 4); err != nil {
		t.Fatal(err)
	}
	if _, deleted := removed.args[qCaseDeleteTools]; !deleted {
		t.Fatal("removing a case must remove its script")
	}
}

func TestGoldenCaseStoreReadsScriptsInTheReadersScope(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qCaseAgentSet: {{"support", int64(4)}},
		qCaseList:     {caseRow("refund", "dev", "hi"), caseRow("plain", "dev", "yo")},
		qCaseTurns:    {{"refund", int64(2), "second"}, {"refund", int64(3), "third"}},
		qCaseTools:    {{"refund", "orders.get", `{"id":1}`, false}, {"refund", "refund.issue", nil, true}},
	})
	store, _ := caseStore(query)
	set, err := store.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	script := set.Cases[0].Script
	if len(script.Turns) != 2 || script.Turns[1].Input != "third" || len(script.Tools) != 2 || string(script.Tools[0].Output) != `{"id":1}` ||
		!script.Tools[1].IsError || script.Tools[1].Output != nil || set.Cases[1].Script.Scripted() {
		t.Fatalf("cases = %+v", set.Cases)
	}
	for _, name := range []string{qCaseTurns, qCaseTools} {
		if args := query.args[name]; args[2] != "dev" || args[3] != "dev" {
			t.Fatalf("%s args = %v", name, args)
		}
	}
	query.rows[qCaseTurns] = [][]any{{"refund", int64(3), "third"}}
	if _, err := store.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a gap in the turns = %v", err)
	}
	query.rows[qCaseTurns] = [][]any{{"secret", int64(2), "probe"}}
	if _, err := store.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a turn of an invisible case = %v", err)
	}
}

func TestGoldenCaseStoreFreezesAScriptUnderItsContentDigest(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qCaseSetRevision:  {{int64(4)}},
		qCaseList:         {caseRow("plain", "dev", "yo"), caseRow("refund", "dev", "I want a refund")},
		qCaseTurns:        {{"refund", int64(2), "and the second order?"}},
		qCaseTools:        {{"refund", "orders.get", `{"id":1}`, false}, {"refund", "refund.issue", nil, true}},
		qCaseBumpRevision: {{int64(5)}},
		qCaseNextVersion:  {{int64(3)}},
	})
	store, objects := caseStore(query)
	version, _, err := store.Freeze(ctx, 7, "support", 4)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := normalizeScript(refundScript(), scriptBounds{})
	content, _ := canonicalScript(want)
	digest := sha256Hex(content)
	if string(objects.Objects["evaluation/golden/7/support/"+digest]) != string(content) {
		t.Fatalf("script not uploaded under its digest: %v", objects.Objects)
	}
	inserts := query.history[qCaseExampleInsert]
	if len(inserts) != 2 || inserts[0][16] != nil || inserts[0][17] != nil ||
		inserts[1][16] != "object://cases/evaluation/golden/7/support/"+digest || inserts[1][17] != digest {
		t.Fatalf("example inserts = %v", inserts)
	}
	examples := []domain.GoldenExample{
		{ExampleID: "plain", Payload: domain.ObjectRef{Digest: sha256Hex([]byte("yo"))}, ExpectedBehavior: []byte("polite refusal")},
		{ExampleID: "refund", Payload: domain.ObjectRef{Digest: sha256Hex([]byte("I want a refund"))}, ExpectedBehavior: []byte("polite refusal"),
			Script: domain.ObjectRef{Digest: digest}},
	}
	if version.DatasetRevision != DatasetRevision(examples) {
		t.Fatal("the dataset revision must digest the frozen script")
	}
	examples[1].Script = domain.ObjectRef{}
	if version.DatasetRevision == DatasetRevision(examples) {
		t.Fatal("a script must change the dataset revision")
	}
}
