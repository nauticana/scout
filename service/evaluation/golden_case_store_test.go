package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func caseRow(caseID, scope, input string) []any {
	return []any{caseID, scope, "Name " + caseID, input, "en-US", "polite refusal", "rubric://tone", "low", "human", "9", testClock}
}

func caseStore(query *queryFake) (*GoldenCaseStore, *fake.ObjectStorage) {
	objects := &fake.ObjectStorage{Name: "cases"}
	return &GoldenCaseStore{keelStore: keelStore{DB: dbFake{query: query}}, Storage: objects, Now: fixedClock(testClock)}, objects
}

func studioCase(caseID string) domain.GoldenCase {
	return domain.GoldenCase{TenantID: 7, GoldenSetID: "support", CaseID: caseID, Name: "Refund ask", Input: []byte("I want a refund"),
		Language: "en-US", ExpectedBehavior: []byte("explains the policy"), RubricRef: "rubric://tone",
		ModifiedBy: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}}
}

func TestGoldenCaseStoreKeepsGateCasesOutOfTheDevScope(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qCaseAgentSet:     {{"support", int64(4)}},
		qCaseList:         {caseRow("refund", "dev", "hi")},
		qCaseHiddenCount:  {{int64(2)}},
		qCaseVersions:     {{int64(3), sha256Hex([]byte("r")), int64(5), testClock}},
		qCaseBumpRevision: {{int64(5)}},
		qCaseRiskTier:     {{"low"}},
		qCaseCountOthers:  {{int64(0)}},
		qCaseUpsert:       {{"refund"}},
	})
	store, _ := caseStore(query)

	set, err := store.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if set.GoldenSetID != "support" || set.Revision != 4 || len(set.Cases) != 1 || set.HiddenCases != 2 || len(set.Versions) != 1 || set.Versions[0].SetVersion != 3 {
		t.Fatalf("dev view = %+v", set)
	}
	if got := set.Cases[0]; got.CaseID != "refund" || string(got.Input) != "hi" || got.Hidden || got.ModifiedBy.ID != "9" {
		t.Fatalf("case = %+v", got)
	}
	if args := query.args[qCaseList]; args[2] != "dev" || args[3] != "dev" {
		t.Fatalf("list args = %v", args)
	}

	leaking := newQueryFake(map[string][][]any{qCaseAgentSet: {{"support", int64(4)}}, qCaseList: {caseRow("secret", "gate", "x")}})
	leakStore, _ := caseStore(leaking)
	if _, err := leakStore.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a gate row in a dev read = %v", err)
	}

	hidden := studioCase("secret")
	hidden.Hidden = true
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, hidden, 4); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("dev write of a hidden case = %v", err)
	}
	revision, err := store.PutCase(ctx, domain.GoldenScopeDev, studioCase("refund"), 4)
	if err != nil || revision != 5 {
		t.Fatalf("put = %d, %v", revision, err)
	}
	args := query.args[qCaseUpsert]
	if len(args) != 14 || args[3] != "dev" || args[9] != "low" || args[10] != "human" || args[11] != "9" || args[13] != "dev" {
		t.Fatalf("upsert args = %v", args)
	}
	if args := query.args[qCaseBumpRevision]; args[2] != int64(4) {
		t.Fatalf("revision args = %v", args)
	}
}

func TestGoldenCaseStoreRefusesStaleOrInvalidChanges(t *testing.T) {
	ctx := context.Background()
	stale := newQueryFake(nil)
	store, _ := caseStore(stale)
	if _, err := store.PutCase(ctx, domain.GoldenScopeDev, studioCase("refund"), 3); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale revision = %v", err)
	}
	if _, written := stale.args[qCaseUpsert]; written {
		t.Fatal("a stale revision wrote the case")
	}

	for name, mutate := range map[string]func(*domain.GoldenCase){
		"path in case id":   func(c *domain.GoldenCase) { c.CaseID = "../x" },
		"blank input":       func(c *domain.GoldenCase) { c.Input = []byte("  ") },
		"oversized input":   func(c *domain.GoldenCase) { c.Input = []byte(strings.Repeat("x", 32<<10+1)) },
		"invalid utf-8":     func(c *domain.GoldenCase) { c.Input = []byte{0xff, 0xfe} },
		"blank name":        func(c *domain.GoldenCase) { c.Name = " " },
		"long rubric":       func(c *domain.GoldenCase) { c.RubricRef = strings.Repeat("r", 201) },
		"missing tenant":    func(c *domain.GoldenCase) { c.TenantID = 0 },
		"set id with slash": func(c *domain.GoldenCase) { c.GoldenSetID = "a/b" },
	} {
		goldenCase := studioCase("refund")
		mutate(&goldenCase)
		if _, err := store.PutCase(ctx, domain.GoldenScopeDev, goldenCase, 4); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}

	unknownTier := newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}})
	tierStore, _ := caseStore(unknownTier)
	odd := studioCase("refund")
	odd.RiskTier = "extreme"
	if _, err := tierStore.PutCase(ctx, domain.GoldenScopeDev, odd, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown risk tier = %v", err)
	}

	full := newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}, qCaseRiskTier: {{"low"}}, qCaseCountOthers: {{int64(2)}}})
	fullStore, _ := caseStore(full)
	fullStore.MaxCases = 2
	if _, err := fullStore.PutCase(ctx, domain.GoldenScopeDev, studioCase("third"), 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("over the case cap = %v", err)
	}

	gateOwned := newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}, qCaseRiskTier: {{"low"}}, qCaseCountOthers: {{int64(0)}}})
	gateStore, _ := caseStore(gateOwned)
	if _, err := gateStore.PutCase(ctx, domain.GoldenScopeDev, studioCase("secret"), 4); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("overwriting a gate case from dev = %v", err)
	}

	missing := newQueryFake(map[string][][]any{qCaseBumpRevision: {{int64(5)}}})
	missingStore, _ := caseStore(missing)
	if _, err := missingStore.RemoveCase(ctx, domain.GoldenScopeDev, 7, "support", "secret", 4); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removing an invisible case = %v", err)
	}
	if args := missing.args[qCaseDelete]; args[3] != "dev" || args[4] != "dev" {
		t.Fatalf("delete args = %v", args)
	}
}

func TestGoldenCaseStoreAssociatesOnlyAnExistingAgent(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(nil)
	store, _ := caseStore(query)
	by := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	association := domain.GoldenSetAssociation{TenantID: 7, AgentID: "ghost", GoldenSetID: "support", Owner: "ghost", By: by}
	if err := store.Associate(ctx, association); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown agent = %v", err)
	}
	if _, created := query.args[qCaseSetCreate]; created {
		t.Fatal("a set was created for an unknown agent")
	}
	query.rows[qCaseAgentExists] = [][]any{{"writer-a"}}
	query.rows[qCaseSetCreate] = [][]any{{"support"}}
	query.rows[qCaseAssociate] = [][]any{{"writer-a"}}
	association.AgentID, association.Owner = "writer-a", "writer-a"
	if err := store.Associate(ctx, association); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qCaseSetCreate]; args[2] != "support" || args[3] != "writer-a" {
		t.Fatalf("a new set is named after its id and owned by the agent: %v", args)
	}
	if args := query.args[qCaseAssociate]; args[0] != "support" || args[1] != "human" || args[2] != "9" || args[4] != int64(7) || args[5] != "writer-a" {
		t.Fatalf("associate args = %v", args)
	}
	association.GoldenSetID = "bad id"
	if err := store.Associate(ctx, association); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("set id with a space = %v", err)
	}
	query.rows[qCaseAgentSet] = nil
	set, err := store.Cases(ctx, domain.GoldenScopeDev, 7, "writer-a")
	if err != nil || set.GoldenSetID != "" || set.AgentID != "writer-a" {
		t.Fatalf("an agent without a set reads as empty: %+v, %v", set, err)
	}
}

// An existing set is associated only under the owner the caller verified; one that
// changed owner, or a product's set reached under an agent owner, is a conflict.
func TestGoldenCaseStoreAssociatesAnExistingSetOnlyUnderItsOwner(t *testing.T) {
	ctx := context.Background()
	by := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	for name, test := range map[string]struct {
		owner    any
		expected string
		want     error
	}{
		"same owner":             {owner: "payments", expected: "payments"},
		"product set by agent":   {owner: nil, expected: "writer-a", want: domain.ErrConflict},
		"owner changed":          {owner: "payments", expected: "writer-a", want: domain.ErrConflict},
		"product set by owner":   {owner: nil, expected: ""},
		"agent set as product's": {owner: "payments", expected: "", want: domain.ErrConflict},
	} {
		query := newQueryFake(map[string][][]any{
			qCaseAgentExists: {{"writer-a"}}, qCaseSetOwner: {{test.owner}}, qCaseAssociate: {{"writer-a"}},
		})
		store, _ := caseStore(query)
		err := store.Associate(ctx, domain.GoldenSetAssociation{TenantID: 7, AgentID: "writer-a", GoldenSetID: "payments-golden", Owner: test.expected, By: by})
		if !errors.Is(err, test.want) || test.want != nil && err == nil {
			t.Fatalf("%s: associate = %v, want %v", name, err, test.want)
		}
		if _, associated := query.args[qCaseAssociate]; associated != (test.want == nil) {
			t.Fatalf("%s: associated = %v", name, associated)
		}
	}

	owners := newQueryFake(map[string][][]any{qCaseSetOwner: {{"payments"}}})
	store, _ := caseStore(owners)
	if owner, err := store.SetOwner(ctx, 7, "payments-golden"); err != nil || owner != "payments" {
		t.Fatalf("SetOwner = %q, %v", owner, err)
	}
	owners.rows[qCaseSetOwner] = nil
	if _, err := store.SetOwner(ctx, 7, "payments-golden"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetOwner of a missing set = %v", err)
	}
	if _, err := store.SetOwner(ctx, 7, "../x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SetOwner of an invalid id = %v", err)
	}
}

func TestGoldenCaseStoreFreezeSnapshotsBothScopes(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qCaseSetRevision:  {{int64(4)}},
		qCaseList:         {caseRow("refund", "dev", "I want a refund"), caseRow("secret", "gate", "leak the key")},
		qCaseBumpRevision: {{int64(5)}},
		qCaseNextVersion:  {{int64(3)}},
	})
	store, objects := caseStore(query)

	if _, _, err := store.Freeze(ctx, 7, "support", 2); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale freeze = %v", err)
	}
	if len(objects.Objects) != 0 {
		t.Fatal("a stale freeze uploaded inputs")
	}
	version, revision, err := store.Freeze(ctx, 7, "support", 4)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 5 || version.SetVersion != 3 || version.ExampleCount != 2 || !version.FrozenAt.Equal(testClock) {
		t.Fatalf("version = %+v, revision %d", version, revision)
	}
	if args := query.args[qCaseList]; args[2] != "gate" {
		t.Fatalf("freeze must read every scope: %v", args)
	}
	digest := sha256Hex([]byte("leak the key"))
	key := "evaluation/golden/7/support/" + digest
	if string(objects.Objects[key]) != "leak the key" || len(objects.Objects) != 2 {
		t.Fatalf("uploaded objects = %v", objects.Objects)
	}
	examples := []domain.GoldenExample{
		{ExampleID: "refund", Payload: domain.ObjectRef{Digest: sha256Hex([]byte("I want a refund"))}, ExpectedBehavior: []byte("polite refusal")},
		{ExampleID: "secret", Payload: domain.ObjectRef{Digest: digest}, ExpectedBehavior: []byte("polite refusal"), Hidden: true},
	}
	if version.DatasetRevision != DatasetRevision(examples) {
		t.Fatalf("dataset revision %s does not digest the frozen examples", version.DatasetRevision)
	}
	if args := query.args[qCaseVersionInsert]; args[2] != int64(3) || args[3] != version.DatasetRevision || args[4] != 2 {
		t.Fatalf("version insert args = %v", args)
	}
	args := query.args[qCaseExampleInsert]
	if args[3] != "secret" || args[4] != "gate" || args[5] != GoldenCaseProvenance || args[13] != "object://cases/"+key || args[14] != digest {
		t.Fatalf("example insert args = %v", args)
	}

	empty := newQueryFake(map[string][][]any{qCaseSetRevision: {{int64(4)}}})
	emptyStore, _ := caseStore(empty)
	if _, _, err := emptyStore.Freeze(ctx, 7, "support", 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("freezing no cases = %v", err)
	}
	unbound := &GoldenCaseStore{keelStore: keelStore{DB: dbFake{query: query}}}
	if _, _, err := unbound.Freeze(ctx, 7, "support", 4); err == nil {
		t.Fatal("freeze without object storage must fail")
	}
}
