package evaluation

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var reviewActor = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}}

func reviewTurnItem(agentID string, status domain.ReviewStatus) domain.HumanReviewItem {
	return domain.HumanReviewItem{ItemID: 12, TenantID: 7, AgentID: agentID, AgentVersion: "3", Source: domain.ReviewSourceFeedback,
		Reason: "rated 1 of 5", Status: status, Turn: domain.ReviewTurnRef{RequestID: "req-1", ConversationID: "c1", TurnNo: 2,
			Input:    domain.ObjectRef{URI: "scout://state/in", Digest: sha256Hex([]byte("hello"))},
			Response: domain.ObjectRef{URI: "scout://state/out", Digest: sha256Hex([]byte("hi there"))}},
		Grade: domain.ReviewGrade{Score: 1, Verdict: domain.ReviewFail, ExpectedBehavior: []byte("greets back")}}
}

func studioReview(allowed grants, queue *fake.HumanReviewQueue, cases *caseStoreFake, query *queryFake) *StudioReview {
	return &StudioReview{DB: dbFake{query: query}, Authorizer: allowed, Queue: queue, GoldenCases: cases,
		Objects: objectReaderFunc(func(_ context.Context, ref domain.ObjectRef) ([]byte, error) {
			return map[string][]byte{"scout://state/in": []byte("hello"), "scout://state/out": []byte("hi there")}[ref.URI], nil
		})}
}

func TestStudioReviewListsForReviewersOnly(t *testing.T) {
	ctx := context.Background()
	var filter domain.ReviewFilter
	queue := &fake.HumanReviewQueue{ListFunc: func(_ context.Context, tenantID int64, got domain.ReviewFilter) ([]domain.HumanReviewItem, error) {
		filter = got
		return []domain.HumanReviewItem{{ItemID: 30}, {ItemID: 20}, {ItemID: 10}}, nil
	}}
	review := studioReview(grants{domain.StudioReview: true}, queue, &caseStoreFake{}, newQueryFake(nil))
	page, err := review.Reviews(ctx, reviewActor, domain.ReviewFilter{AgentID: "writer-a", Limit: 2, Sources: []domain.ReviewSource{domain.ReviewSourceFeedback}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextBefore != 20 || filter.Limit != 3 || len(filter.Sources) != 1 {
		t.Fatalf("page = %+v, filter = %+v", page, filter)
	}
	if _, err := review.Reviews(ctx, reviewActor, domain.ReviewFilter{AgentID: "writer-a"}); err != nil || filter.Limit != DefaultReviewPageSize+1 {
		t.Fatalf("default page = %d, %v", filter.Limit, err)
	}
	if _, err := review.Reviews(ctx, reviewActor, domain.ReviewFilter{AgentID: "writer-a", Limit: DefaultReviewMaxPageSize + 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized page = %v", err)
	}
	viewer := studioReview(grants{domain.StudioView: true}, queue, &caseStoreFake{}, newQueryFake(nil))
	if _, err := viewer.Reviews(ctx, reviewActor, domain.ReviewFilter{AgentID: "writer-a"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer without REVIEW listed reviews: %v", err)
	}
}

func TestStudioReviewReadsAndGradesItemsOfVisibleAgentsOnly(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(nil)
	var graded domain.PrincipalRef
	status := domain.ReviewPending
	queue := &fake.HumanReviewQueue{
		GetFunc: func(_ context.Context, _ int64, itemID int64) (domain.HumanReviewItem, error) {
			switch itemID {
			case 12:
				return reviewTurnItem("writer-a", status), nil
			case 13:
				return reviewTurnItem("other-agent", domain.ReviewPending), nil
			}
			return domain.HumanReviewItem{}, domain.ErrNotFound
		},
		GradeFunc: func(_ context.Context, _ int64, itemID int64, grade domain.ReviewGrade, by domain.PrincipalRef) (domain.HumanReviewItem, error) {
			graded = by
			item := reviewTurnItem("writer-a", domain.ReviewGraded)
			item.Grade = grade
			return item, nil
		},
	}
	review := studioReview(grants{domain.StudioView: true, domain.StudioReview: true}, queue, &caseStoreFake{}, query)

	content, err := review.ReviewTurn(ctx, reviewActor, 12)
	if err != nil || string(content.Input) != "hello" || string(content.Response) != "hi there" {
		t.Fatalf("turn = %+v, %v", content, err)
	}
	if _, err := review.ReviewTurn(ctx, reviewActor, 13); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("item of an invisible agent = %v, want not found", err)
	}
	tampered := studioReview(grants{domain.StudioView: true, domain.StudioReview: true}, queue, &caseStoreFake{}, query)
	tampered.Objects = objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return []byte("changed"), nil })
	if _, err := tampered.ReviewTurn(ctx, reviewActor, 12); err == nil {
		t.Fatal("content that does not match its digest was served")
	}
	tampered.MaxTurnBytes = 3
	if _, err := tampered.ReviewTurn(ctx, reviewActor, 12); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized turn = %v", err)
	}

	item, err := review.Grade(ctx, reviewActor, 12, domain.ReviewGrade{Score: 2, Verdict: domain.ReviewFail})
	if err != nil || item.Status != domain.ReviewGraded || graded != reviewActor.Principal {
		t.Fatalf("grade = %+v by %+v, %v", item, graded, err)
	}
	if args := query.args[qStudioReviewEvent]; args[1] != "writer-a" || args[2] != StudioEventReviewGrade || args[5] != "9" {
		t.Fatalf("audit args = %v", args)
	}
	status, query.args = domain.ReviewGraded, map[string][]any{}
	if _, err := review.Grade(ctx, reviewActor, 12, domain.ReviewGrade{Score: 2, Verdict: domain.ReviewFail}); err != nil {
		t.Fatalf("grade replay = %v", err)
	}
	if _, audited := query.args[qStudioReviewEvent]; audited {
		t.Fatal("an idempotent grade replay was audited again")
	}
	viewer := studioReview(grants{domain.StudioView: true}, queue, &caseStoreFake{}, newQueryFake(nil))
	if _, err := viewer.Grade(ctx, reviewActor, 12, domain.ReviewGrade{Score: 2, Verdict: domain.ReviewFail}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer graded: %v", err)
	}
}

func TestStudioReviewPromotesAGradedTurnIntoTheDevGoldenSet(t *testing.T) {
	ctx := context.Background()
	status := domain.ReviewGraded
	var promotedCase string
	promotions := 0
	queue := &fake.HumanReviewQueue{
		GetFunc: func(context.Context, int64, int64) (domain.HumanReviewItem, error) {
			item := reviewTurnItem("writer-a", status)
			item.PromotedCaseID = promotedCase
			return item, nil
		},
		PromoteFunc: func(_ context.Context, _ int64, itemID int64, goldenSetID, caseID string) (domain.HumanReviewItem, error) {
			promotions++
			item := reviewTurnItem("writer-a", domain.ReviewPromoted)
			item.PromotedSetID, item.PromotedCaseID = goldenSetID, caseID
			return item, nil
		},
	}
	cases := &caseStoreFake{}
	query := newQueryFake(nil)
	editor := grants{domain.StudioView: true, domain.StudioReview: true, domain.StudioEdit: true}
	review := studioReview(editor, queue, cases, query)

	item, set, err := review.Promote(ctx, reviewActor, domain.ReviewPromotion{ItemID: 12, ExpectedRevision: 4})
	if err != nil {
		t.Fatal(err)
	}
	put := cases.put
	if item.PromotedCaseID != "review-12" || set.GoldenSetID != "support" || put.CaseID != "review-12" || put.GoldenSetID != "support" ||
		string(put.Input) != "hello" || string(put.ExpectedBehavior) != "greets back" || put.Hidden || put.Name != "Review 12" ||
		put.ReviewItemID != 12 || put.ModifiedBy != reviewActor.Principal || cases.scopes[0] != domain.GoldenScopeDev {
		t.Fatalf("promoted %+v into %+v", item, put)
	}
	if args := query.args[qStudioReviewEvent]; args[2] != StudioEventReviewPromote {
		t.Fatalf("audit args = %v", args)
	}

	// A retry after the case was written but before the item was marked converges without rewriting it.
	cases.put, cases.cases = domain.GoldenCase{}, []domain.GoldenCase{{CaseID: "review-12", Input: []byte("hello"), ReviewItemID: 12, ReviewAgentID: "writer-a"}}
	if _, set, err = review.Promote(ctx, reviewActor, domain.ReviewPromotion{ItemID: 12, ExpectedRevision: 4}); err != nil || cases.put.CaseID != "" || promotions != 2 {
		t.Fatalf("converging retry wrote %+v, marks %d, %v", cases.put, promotions, err)
	}
	if string(set.Cases[0].Input) != "hello" {
		t.Fatalf("a reviewer's set withheld the promoted input: %+v", set.Cases)
	}
	cases.cases = []domain.GoldenCase{{CaseID: "review-12", Input: []byte("hand written")}}
	if _, _, err := review.Promote(ctx, reviewActor, domain.ReviewPromotion{ItemID: 12, ExpectedRevision: 4}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a foreign case of the same id = %v", err)
	}
	cases.cases = nil

	status, promotedCase, cases.put, query.args = domain.ReviewPromoted, "review-12", domain.GoldenCase{}, map[string][]any{}
	if _, _, err := review.Promote(ctx, reviewActor, domain.ReviewPromotion{ItemID: 12, ExpectedRevision: 5}); err != nil || cases.put.CaseID != "" {
		t.Fatalf("replayed promotion wrote %+v, %v", cases.put, err)
	}
	if _, audited := query.args[qStudioReviewEvent]; audited {
		t.Fatal("a replayed promotion was audited again")
	}
	status, promotedCase = domain.ReviewPending, ""
	if _, _, err := review.Promote(ctx, reviewActor, domain.ReviewPromotion{ItemID: 12}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("promotion of an ungraded item = %v", err)
	}
	status = domain.ReviewGraded
	reviewer := studioReview(grants{domain.StudioView: true, domain.StudioReview: true}, queue, &caseStoreFake{}, newQueryFake(nil))
	if _, _, err := reviewer.Promote(ctx, reviewActor, domain.ReviewPromotion{ItemID: 12}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("promotion without EDIT = %v", err)
	}
}
