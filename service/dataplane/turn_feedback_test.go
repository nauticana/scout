package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var feedbackPerson = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}

func feedbackService(query *queueQueryFake, allowed bool) *TurnFeedback {
	return &TurnFeedback{
		DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView},
		Roles: fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
			return domain.AuthorizationGrant{Allowed: allowed}, nil
		}),
	}
}

func TestFeedbackRatesACompletedTurnOnce(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qFeedbackTurn: {{"conversation-1", int64(3), "completed", "writer-a", "42"}}}}
	if err := feedbackService(query, true).RecordFeedback(context.Background(), domain.TenantContext{TenantID: 7}, feedbackPerson, "r-1", 4, "helpful"); err != nil {
		t.Fatalf("RecordFeedback: %v", err)
	}
	if args := query.firstArgs(qFeedbackPut); args[1] != "conversation-1" || args[2] != int64(3) || args[4] != "42" || args[5] != 4 {
		t.Fatalf("args = %v", args)
	}
}

func TestFeedbackRefusesBadRatingsRunningTurnsAndStrangers(t *testing.T) {
	tenant := domain.TenantContext{TenantID: 7}
	query := &queueQueryFake{rows: map[string][][]any{qFeedbackTurn: {{"conversation-1", int64(3), "running", "writer-a", "42"}}}}
	if err := feedbackService(query, true).RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 6, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("rating 6 = %v", err)
	}
	if err := feedbackService(query, true).RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 3, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("running turn = %v", err)
	}
	query = &queueQueryFake{rows: map[string][][]any{qFeedbackTurn: {{"conversation-1", int64(3), "completed", "writer-a", "42"}}}}
	if err := feedbackService(query, false).RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 3, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("stranger = %v", err)
	}
	if query.firstArgs(qFeedbackPut) != nil {
		t.Fatal("refused feedback must not be stored")
	}
	query = &queueQueryFake{rows: map[string][][]any{qFeedbackTurn: {{"conversation-1", int64(3), "completed", "writer-a", "7"}}}}
	if err := feedbackService(query, true).RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 3, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another person's turn = %v", err)
	}
}

func TestFeedbackSummaryCountsPerReleaseWithoutPeople(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qFeedbackSummary: {
		{"3", int64(1), int64(2)}, {"3", int64(5), int64(7)}, {"4", int64(4), int64(1)},
	}}}
	summaries, err := feedbackService(query, true).Feedback(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(time.Hour))
	if err != nil || len(summaries) != 2 {
		t.Fatalf("summaries = %+v, error = %v", summaries, err)
	}
	if summaries[0].Counts != [5]int64{2, 0, 0, 0, 7} || summaries[1].Release != "4" || summaries[1].Counts[3] != 1 {
		t.Fatalf("summaries = %+v", summaries)
	}
}

type reviewIntakeFake struct {
	completed []domain.ReviewTurn
	rated     []domain.ReviewTurn
	ratings   []int
	err       error
}

func (f *reviewIntakeFake) TurnCompleted(_ context.Context, turn domain.ReviewTurn) error {
	f.completed = append(f.completed, turn)
	return f.err
}

func (f *reviewIntakeFake) TurnRated(_ context.Context, turn domain.ReviewTurn, rating int) error {
	f.rated, f.ratings = append(f.rated, turn), append(f.ratings, rating)
	return f.err
}

func TestFeedbackOffersTheRatedTurnToReviewWithoutThePerson(t *testing.T) {
	tenant := domain.TenantContext{TenantID: 7, Region: "eu"}
	turnRow := [][]any{{"conversation-1", int64(3), "completed", "writer-a", "42", "3"}}
	query := &queueQueryFake{rows: map[string][][]any{qFeedbackTurn: turnRow}}
	review := &reviewIntakeFake{}
	feedback := feedbackService(query, true)
	feedback.Review = review
	if err := feedback.RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 1, "wrong"); err != nil {
		t.Fatal(err)
	}
	want := domain.ReviewTurn{TenantContext: tenant, RequestID: "r-1", AgentID: "writer-a", AgentVersion: "3"}
	if len(review.rated) != 1 || review.rated[0] != want || review.ratings[0] != 1 {
		t.Fatalf("offered %+v with %v", review.rated, review.ratings)
	}

	review.err = domain.ErrRateLimited
	if err := feedback.RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 1, ""); err != nil {
		t.Fatalf("a full review queue failed the feedback: %v", err)
	}
	review.err = errors.New("review store down")
	if err := feedback.RecordFeedback(context.Background(), tenant, feedbackPerson, "r-1", 1, ""); err != nil {
		t.Fatalf("a review intake failure failed the stored rating: %v", err)
	}
	if args := query.firstArgs(qFeedbackPut); args == nil || len(review.rated) != 3 {
		t.Fatalf("rating stored = %v, offers = %d", args, len(review.rated))
	}
}
