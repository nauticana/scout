package handler

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type turnReviewBackendFake struct {
	contract.StudioReviewBackend
	filter    domain.ReviewFilter
	grade     domain.ReviewGrade
	promotion domain.ReviewPromotion
}

func gradedReviewItem() domain.HumanReviewItem {
	return domain.HumanReviewItem{ItemID: 12, AgentID: "writer-a", AgentVersion: "3", Source: domain.ReviewSourceFeedback, Reason: "rated 1 of 5",
		Status: domain.ReviewGraded, EnqueuedAt: time.Unix(100, 0).UTC(),
		Turn:     domain.ReviewTurnRef{RequestID: "req-1", ConversationID: "c1", TurnNo: 2, Input: domain.ObjectRef{URI: "scout://in", Digest: "d"}},
		Grade:    domain.ReviewGrade{Score: 2, Verdict: domain.ReviewFail, Notes: "wrong", ExpectedBehavior: []byte("greets")},
		Reviewer: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, ReviewedAt: time.Unix(200, 0).UTC()}
}

func (f *turnReviewBackendFake) Reviews(_ context.Context, _ domain.StudioActor, filter domain.ReviewFilter) (domain.ReviewPage, error) {
	f.filter = filter
	return domain.ReviewPage{Items: []domain.HumanReviewItem{gradedReviewItem()}, NextBefore: 12}, nil
}

func (f *turnReviewBackendFake) ReviewTurn(context.Context, domain.StudioActor, int64) (domain.ReviewTurnContent, error) {
	return domain.ReviewTurnContent{Item: gradedReviewItem(), Input: []byte("hello"), Response: []byte("hi")}, nil
}

func (f *turnReviewBackendFake) Grade(_ context.Context, _ domain.StudioActor, _ int64, grade domain.ReviewGrade) (domain.HumanReviewItem, error) {
	f.grade = grade
	return gradedReviewItem(), nil
}

func (f *turnReviewBackendFake) Promote(_ context.Context, _ domain.StudioActor, promotion domain.ReviewPromotion) (domain.HumanReviewItem, domain.GoldenCaseSet, error) {
	f.promotion = promotion
	item := gradedReviewItem()
	item.Status, item.PromotedSetID, item.PromotedCaseID = domain.ReviewPromoted, "support", "review-12"
	return item, domain.GoldenCaseSet{AgentID: "writer-a", GoldenSetID: "support", Revision: 5}, nil
}

func TestStudioReviewRoutes(t *testing.T) {
	h := &StudioReviewHandler{}
	routes := h.Routes()
	for _, path := range []string{api.StudioReviewsPath, api.StudioReviewTurnPath, api.StudioReviewGradePath, api.StudioReviewDismissPath, api.StudioReviewPromotePath} {
		if routes[path] == nil {
			t.Fatalf("missing route %s", path)
		}
	}
	if feature := h.Feature(); feature.ID != StudioFeatureReview || len(routes) != 5 || feature.Actions[1] != string(domain.StudioReview) {
		t.Fatalf("feature = %+v, routes = %d", feature, len(routes))
	}
}

func TestStudioReviewHandlerMapsItemsWithoutTurnIdentifiers(t *testing.T) {
	backend := &turnReviewBackendFake{}
	h := &StudioReviewHandler{Backend: backend}
	view, err := h.reviews(context.Background(), domain.StudioActor{}, url.Values{
		"agent_name": {"writer-a"}, "status": {"graded"}, "source": {"feedback"}, "release": {"3"}, "before": {"40"}, "limit": {"10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f := backend.filter; f.AgentID != "writer-a" || f.Status != domain.ReviewGraded || f.Release != "3" || f.Before != 40 || f.Limit != 10 ||
		len(f.Sources) != 1 || f.Sources[0] != domain.ReviewSourceFeedback {
		t.Fatalf("filter = %+v", f)
	}
	page := view.(api.ReviewPage)
	item := page.Items[0]
	if page.NextBefore != 12 || item.ItemID != 12 || item.Version != "3" || item.Grade == nil || item.Grade.ExpectedBehavior != "greets" ||
		item.ReviewerID != "9" || item.ReviewedAt == nil || item.PromotedAt != nil {
		t.Fatalf("page = %+v", page)
	}
	encoded, _ := json.Marshal(page)
	for _, leak := range []string{"req-1", "c1", "scout://in", "turn_no"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("review page exposes %q: %s", leak, encoded)
		}
	}
	if _, err := h.reviews(context.Background(), domain.StudioActor{}, url.Values{"limit": {"x"}}); err == nil {
		t.Fatal("a non-numeric limit was accepted")
	}

	turn, err := h.turn(context.Background(), domain.StudioActor{}, url.Values{"item_id": {"12"}})
	if err != nil || turn.(api.ReviewTurn).Input != "hello" || turn.(api.ReviewTurn).Response != "hi" {
		t.Fatalf("turn = %+v, %v", turn, err)
	}
	if _, err = h.grade(context.Background(), domain.StudioActor{}, api.ReviewGradeRequest{ItemID: 12,
		ReviewGrade: api.ReviewGrade{Score: 2, Verdict: "fail", ExpectedBehavior: "greets"}}); err != nil {
		t.Fatal(err)
	}
	if backend.grade.Score != 2 || backend.grade.Verdict != domain.ReviewFail || string(backend.grade.ExpectedBehavior) != "greets" {
		t.Fatalf("grade = %+v", backend.grade)
	}
	promoted, err := h.promote(context.Background(), domain.StudioActor{}, api.ReviewPromoteRequest{ItemID: 12, ExpectedRevision: 4, RiskTier: "high"})
	if err != nil {
		t.Fatal(err)
	}
	result := promoted.(api.ReviewPromoted)
	if backend.promotion.ExpectedRevision != 4 || backend.promotion.RiskTier != "high" || result.Item.CaseID != "review-12" || result.Set.Revision != 5 {
		t.Fatalf("promoted = %+v from %+v", result, backend.promotion)
	}
}
