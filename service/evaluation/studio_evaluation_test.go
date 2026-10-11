package evaluation

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type grants map[domain.StudioAction]bool

func (allowed grants) Authorize(_ context.Context, _ domain.StudioActor, action domain.StudioAction, agentID string) error {
	if agentID != "writer-a" || !allowed[action] {
		return domain.ErrForbidden
	}
	return nil
}

type caseStoreFake struct {
	contract.GoldenCaseStore
	scopes     []domain.GoldenScope
	put        domain.GoldenCase
	frozen     string
	owners     map[string]string
	associated []domain.GoldenSetAssociation
}

func (store *caseStoreFake) SetOwner(_ context.Context, _ int64, goldenSetID string) (string, error) {
	owner, exists := store.owners[goldenSetID]
	if !exists {
		return "", domain.ErrNotFound
	}
	return owner, nil
}

func (store *caseStoreFake) Associate(_ context.Context, association domain.GoldenSetAssociation) error {
	store.associated = append(store.associated, association)
	return nil
}

func (store *caseStoreFake) AgentSet(context.Context, int64, string) (string, int64, error) {
	return "support", 4, nil
}

func (store *caseStoreFake) Cases(_ context.Context, scope domain.GoldenScope, _ int64, agentID string) (domain.GoldenCaseSet, error) {
	store.scopes = append(store.scopes, scope)
	return domain.GoldenCaseSet{AgentID: agentID, GoldenSetID: "support", Revision: 5}, nil
}

func (store *caseStoreFake) PutCase(_ context.Context, scope domain.GoldenScope, goldenCase domain.GoldenCase, _ int64) (int64, error) {
	store.scopes = append(store.scopes, scope)
	store.put = goldenCase
	return 5, nil
}

func (store *caseStoreFake) Freeze(_ context.Context, _ int64, goldenSetID string, _ int64) (domain.GoldenSetVersion, int64, error) {
	store.frozen = goldenSetID
	return domain.GoldenSetVersion{GoldenSetID: goldenSetID, SetVersion: 3}, 6, nil
}

type runStoreFake struct {
	enqueued domain.AgentEvaluationEnqueue
	results  []domain.AgentCaseResult
	limit    int
	scopes   []domain.GoldenScope
}

func (store *runStoreFake) Enqueue(_ context.Context, request domain.AgentEvaluationEnqueue) (domain.AgentEvaluationRun, error) {
	store.enqueued = request
	return domain.AgentEvaluationRun{RunID: 11, AgentID: request.AgentID, GoldenSetID: "support", SetVersion: request.SetVersion, Subject: request.Subject}, nil
}

func (store *runStoreFake) Run(_ context.Context, scope domain.GoldenScope, _ int64, runID int64) (domain.AgentEvaluationRun, error) {
	store.scopes = append(store.scopes, scope)
	return domain.AgentEvaluationRun{RunID: runID, AgentID: "writer-a"}, nil
}

func (store *runStoreFake) Results(_ context.Context, scope domain.GoldenScope, _ int64, _ int64, _ string, limit int) ([]domain.AgentCaseResult, error) {
	store.scopes, store.limit = append(store.scopes, scope), limit
	return store.results[:min(limit, len(store.results))], nil
}

type freezerFunc func(context.Context, domain.StudioActor, string) (domain.FrozenDraft, error)

func (function freezerFunc) FreezeDraft(ctx context.Context, actor domain.StudioActor, agentID string) (domain.FrozenDraft, error) {
	return function(ctx, actor, agentID)
}

var studioActor = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}}

func studioEvaluation(allowed grants) (*StudioEvaluation, *caseStoreFake, *runStoreFake, *queryFake) {
	query, cases, runs := newQueryFake(nil), &caseStoreFake{}, &runStoreFake{}
	return &StudioEvaluation{
		DB: dbFake{query: query}, Authorizer: allowed, GoldenCases: cases, Runs: runs, MaxOpenRuns: 2,
		Drafts: freezerFunc(func(_ context.Context, _ domain.StudioActor, agentID string) (domain.FrozenDraft, error) {
			return domain.FrozenDraft{Definition: domain.AgentDefinition{AgentID: agentID, DefinitionDigest: draftDigest}}, nil
		}),
	}, cases, runs, query
}

func TestStudioEvaluationWorksInTheDevScopeOnly(t *testing.T) {
	ctx := context.Background()
	service, cases, _, query := studioEvaluation(grants{domain.StudioView: true, domain.StudioEdit: true})
	if _, err := service.Cases(ctx, studioActor, "writer-a"); err != nil {
		t.Fatal(err)
	}
	change := domain.GoldenCaseChange{AgentID: "writer-a", Op: domain.GoldenCasePut, ExpectedRevision: 4,
		Case: domain.GoldenCase{CaseID: "refund", Name: "Refund", Input: []byte("hi"), TenantID: 99, GoldenSetID: "elsewhere"}}
	if _, err := service.ChangeCases(ctx, studioActor, change); err != nil {
		t.Fatal(err)
	}
	if cases.put.TenantID != 7 || cases.put.GoldenSetID != "support" || cases.put.ModifiedBy.ID != "9" {
		t.Fatalf("the client chose the case's tenant or set: %+v", cases.put)
	}
	for _, scope := range cases.scopes {
		if scope != domain.GoldenScopeDev {
			t.Fatalf("a Studio actor reached the %s scope", scope)
		}
	}
	if args := query.args[qStudioEvaluationEvent]; args[2] != StudioEventCases || args[3] != "put support/refund" || args[4] != "human" {
		t.Fatalf("audit args = %v", args)
	}
	set, version, err := service.FreezeCases(ctx, studioActor, "writer-a", 5)
	if err != nil || version.SetVersion != 3 || cases.frozen != "support" || set.Revision != 5 {
		t.Fatalf("freeze = %+v, %+v, %v", set, version, err)
	}
	if _, err := service.ChangeCases(ctx, studioActor, domain.GoldenCaseChange{AgentID: "writer-a", Op: "rename"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown op = %v", err)
	}
}

func TestStudioEvaluationAuthorizesEveryCall(t *testing.T) {
	ctx := context.Background()
	service, cases, runs, _ := studioEvaluation(grants{domain.StudioView: true})
	if _, err := service.ChangeCases(ctx, studioActor, domain.GoldenCaseChange{AgentID: "writer-a", Op: domain.GoldenCasePut}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("put without EDIT = %v", err)
	}
	if _, _, err := service.FreezeCases(ctx, studioActor, "writer-a", 4); !errors.Is(err, domain.ErrForbidden) || cases.frozen != "" {
		t.Fatalf("freeze without EDIT = %v", err)
	}
	if _, err := service.Evaluate(ctx, studioActor, domain.AgentEvaluationRequest{AgentID: "writer-a", SetVersion: 3, Draft: true}); !errors.Is(err, domain.ErrForbidden) || runs.enqueued.AgentID != "" {
		t.Fatalf("evaluate without TEST = %v", err)
	}
	if _, err := service.Cases(ctx, studioActor, "writer-b"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another agent's cases = %v", err)
	}
	blind, _, _, _ := studioEvaluation(grants{})
	if _, err := blind.Evaluation(ctx, studioActor, 11, "", 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a run of an agent the actor cannot view = %v, want not found", err)
	}
}

type agentGrants map[string]bool

func (allowed agentGrants) Authorize(_ context.Context, _ domain.StudioActor, action domain.StudioAction, agentID string) error {
	if action != domain.StudioEdit || !allowed[agentID] {
		return domain.ErrForbidden
	}
	return nil
}

// EDIT on one agent must not borrow another agent's or a product's golden set.
func TestStudioEvaluationSharesAGoldenSetOnlyWithEditOnItsOwner(t *testing.T) {
	ctx := context.Background()
	associate := func(allowed agentGrants, goldenSetID string) (*caseStoreFake, error) {
		service, cases, _, _ := studioEvaluation(nil)
		service.Authorizer = allowed
		cases.owners = map[string]string{"payments-golden": "payments", "product-gate": ""}
		_, err := service.ChangeCases(ctx, studioActor, domain.GoldenCaseChange{AgentID: "faq", Op: domain.GoldenCaseAssociate, GoldenSetID: goldenSetID})
		return cases, err
	}
	if cases, err := associate(agentGrants{"faq": true}, "payments-golden"); !errors.Is(err, domain.ErrForbidden) || len(cases.associated) != 0 {
		t.Fatalf("borrowing another agent's set = %v, %+v", err, cases.associated)
	}
	if cases, err := associate(agentGrants{"faq": true, "payments": true}, "product-gate"); !errors.Is(err, domain.ErrForbidden) || len(cases.associated) != 0 {
		t.Fatalf("borrowing a product's set = %v, %+v", err, cases.associated)
	}
	cases, err := associate(agentGrants{"faq": true, "payments": true}, "payments-golden")
	if err != nil || len(cases.associated) != 1 || cases.associated[0].Owner != "payments" || cases.associated[0].TenantID != 7 {
		t.Fatalf("sharing with EDIT on the owner = %v, %+v", err, cases.associated)
	}
	cases, err = associate(agentGrants{"faq": true}, "faq-cases")
	if err != nil || len(cases.associated) != 1 || cases.associated[0].Owner != "faq" || cases.associated[0].By.ID != "9" {
		t.Fatalf("a new set is the agent's own: %v, %+v", err, cases.associated)
	}
}

func TestStudioEvaluationPinsTheFrozenDraft(t *testing.T) {
	ctx := context.Background()
	service, _, runs, query := studioEvaluation(grants{domain.StudioTest: true})
	run, err := service.Evaluate(ctx, studioActor, domain.AgentEvaluationRequest{AgentID: "writer-a", SetVersion: 3, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	enqueued := runs.enqueued
	if enqueued.Scope != domain.GoldenScopeDev || enqueued.Subject.Kind != domain.SubjectDraft || enqueued.Subject.DefinitionDigest != draftDigest ||
		enqueued.Draft == nil || enqueued.MaxOpenRuns != 2 || enqueued.TenantID != 7 || enqueued.RequestedBy.ID != "9" || run.RunID != 11 {
		t.Fatalf("enqueued = %+v", enqueued)
	}
	if args := query.args[qStudioEvaluationEvent]; args[2] != StudioEventEvaluate || args[3] != "run 11 of support version 3 on draft "+draftDigest {
		t.Fatalf("audit args = %v", args)
	}
	if _, err := service.Evaluate(ctx, studioActor, domain.AgentEvaluationRequest{AgentID: "writer-a", SetVersion: 3, Release: "v2"}); err != nil {
		t.Fatal(err)
	}
	if subject := runs.enqueued.Subject; subject.Kind != domain.SubjectRelease || subject.Version != "v2" || runs.enqueued.Draft != nil {
		t.Fatalf("release subject = %+v", subject)
	}
	for name, request := range map[string]domain.AgentEvaluationRequest{
		"both subjects":     {AgentID: "writer-a", SetVersion: 3, Draft: true, Release: "v2"},
		"no subject":        {AgentID: "writer-a", SetVersion: 3},
		"no set version":    {AgentID: "writer-a", Draft: true},
		"blank release":     {AgentID: "writer-a", SetVersion: 3, Release: "  "},
		"negative version":  {AgentID: "writer-a", SetVersion: -1, Release: "v2"},
		"draft and release": {AgentID: "writer-a", SetVersion: 3, Draft: true, Release: " v2 "},
	} {
		if _, err := service.Evaluate(ctx, studioActor, request); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v", name, err)
		}
	}
}

func TestStudioEvaluationPagesResults(t *testing.T) {
	ctx := context.Background()
	service, _, runs, _ := studioEvaluation(grants{domain.StudioView: true})
	runs.results = []domain.AgentCaseResult{{ExampleID: "c"}, {ExampleID: "b"}, {ExampleID: "a"}}
	page, err := service.Evaluation(ctx, studioActor, 11, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Results) != 2 || page.NextBefore != "b" || runs.limit != 3 || page.Run.RunID != 11 {
		t.Fatalf("page = %+v (asked for %d)", page, runs.limit)
	}
	page, err = service.Evaluation(ctx, studioActor, 11, "", 0)
	if err != nil || len(page.Results) != 3 || page.NextBefore != "" || runs.limit != studioPageSize+1 {
		t.Fatalf("last page = %+v, %v", page, err)
	}
	for _, scope := range runs.scopes {
		if scope != domain.GoldenScopeDev {
			t.Fatalf("a Studio reader read the %s scope", scope)
		}
	}
	for _, limit := range []int{-1, studioMaxPageSize + 1} {
		if _, err := service.Evaluation(ctx, studioActor, 11, "", limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("limit %d = %v", limit, err)
		}
	}
	if _, err := service.Evaluation(ctx, studioActor, 0, "", 10); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing run id = %v", err)
	}
}
