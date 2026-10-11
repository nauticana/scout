package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

var reviewer = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}

func reviewQueue(query *queryFake) *ReviewQueueStore {
	return &ReviewQueueStore{keelStore: keelStore{DB: dbFake{query: query}}, Now: fixedClock(testClock)}
}

// reviewRow is a human_review_item row joined to its turn, in reviewColumns order.
func reviewRow(id int64, status string, score any, verdict, notes, expected, reviewerID any) []any {
	var kind any
	if reviewerID != nil {
		kind = "human"
	}
	return []any{id, "writer-a", "3", "feedback", "rated 1 of 5", nil, "c1", int64(2), "req-1",
		"scout://state/in", sha256Hex([]byte("hello")), "scout://state/out", sha256Hex([]byte("hi there")),
		status, testClock, score, verdict, notes, expected, kind, reviewerID, nil, nil, nil, nil}
}

func TestReviewQueueStoreDerivesProductionItemsFromTheCompletedTurn(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qReviewTurn:    {{"c1", int64(2), "completed", "writer-a", "3"}},
		qReviewPending: {{int64(0)}},
	})
	store := reviewQueue(query)
	err := store.Enqueue(ctx, domain.HumanReviewItem{TenantID: 7, Source: domain.ReviewSourceFeedback, Reason: "rated 1 of 5",
		AgentID: "spoofed", Turn: domain.ReviewTurnRef{RequestID: "req-1"}})
	if err != nil {
		t.Fatal(err)
	}
	args := query.args[qReviewTurnInsert]
	if len(args) != 9 || args[0] != int64(7) || args[1] != "writer-a" || args[2] != "3" || args[3] != "feedback" || args[6] != "c1" || args[7] != int64(2) {
		t.Fatalf("insert args = %v", args)
	}
	if turn := query.args[qReviewTurn]; turn[0] != int64(7) || turn[1] != "req-1" {
		t.Fatalf("turn lookup args = %v", turn)
	}

	running := reviewQueue(newQueryFake(map[string][][]any{qReviewTurn: {{"c1", int64(2), "running", "writer-a", "3"}}}))
	if err := running.Enqueue(ctx, domain.HumanReviewItem{TenantID: 7, Source: domain.ReviewSourceSampled, Reason: "sampled: base",
		Turn: domain.ReviewTurnRef{RequestID: "req-1"}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("running turn = %v", err)
	}
	missing := reviewQueue(newQueryFake(nil))
	if err := missing.Enqueue(ctx, domain.HumanReviewItem{TenantID: 7, Source: domain.ReviewSourceSampled, Reason: "sampled: base",
		Turn: domain.ReviewTurnRef{RequestID: "other-tenant"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing turn = %v", err)
	}

	full := newQueryFake(map[string][][]any{qReviewTurn: {{"c1", int64(2), "completed", "writer-a", "3"}}, qReviewPending: {{int64(3)}}})
	fullStore := reviewQueue(full)
	fullStore.MaxPending = 3
	if err := fullStore.Enqueue(ctx, domain.HumanReviewItem{TenantID: 7, Source: domain.ReviewSourceSampled, Reason: "sampled: base",
		Turn: domain.ReviewTurnRef{RequestID: "req-1"}}); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("over the pending cap = %v", err)
	}
	if _, inserted := full.args[qReviewTurnInsert]; inserted {
		t.Fatal("an item over the pending cap was inserted")
	}
	replay := newQueryFake(map[string][][]any{
		qReviewTurn: {{"c1", int64(2), "completed", "writer-a", "3"}}, qReviewExisting: {{int64(11)}},
	})
	replayStore := reviewQueue(replay)
	replayStore.MaxPending = 1
	if err := replayStore.Enqueue(ctx, domain.HumanReviewItem{TenantID: 7, Source: domain.ReviewSourceSampled, Reason: "sampled: base",
		Turn: domain.ReviewTurnRef{RequestID: "req-1"}}); err != nil {
		t.Fatalf("replay at capacity = %v", err)
	}
	if _, counted := replay.args[qReviewPending]; counted {
		t.Fatal("a replay consumed pending capacity")
	}

	for name, item := range map[string]domain.HumanReviewItem{
		"no request":     {TenantID: 7, Source: domain.ReviewSourceFeedback, Reason: "x"},
		"unknown source": {TenantID: 7, Source: "evaluation", Reason: "x", Turn: domain.ReviewTurnRef{RequestID: "r"}},
		"no tenant":      {Source: domain.ReviewSourceSampled, Reason: "x", Turn: domain.ReviewTurnRef{RequestID: "r"}},
		"blank reason":   {TenantID: 7, Source: domain.ReviewSourceSampled, Reason: " ", Turn: domain.ReviewTurnRef{RequestID: "r"}},
	} {
		if err := store.Enqueue(ctx, item); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}

// The pending cap counts only items Studio can resolve, so nothing else can fill it for good.
func TestReviewQueuePendingCapCountsOnlyResolvableItems(t *testing.T) {
	sql := reviewQueries[qReviewPending]
	if !strings.Contains(sql, "status_code = 'pending'") || !strings.Contains(sql, "source_code IN ('sampled', 'feedback')") {
		t.Fatalf("pending count = %s", sql)
	}
}

func TestReviewQueueStoreListsOneAgentPerTenantWithFilters(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{qReviewList: {reviewRow(12, "pending", nil, nil, nil, nil, nil)}})
	store := reviewQueue(query)
	items, err := store.List(ctx, 7, domain.ReviewFilter{AgentID: "writer-a", Status: domain.ReviewPending, Release: "3",
		Sources: []domain.ReviewSource{domain.ReviewSourceSampled, domain.ReviewSourceFeedback}, Before: 40, Limit: 11})
	if err != nil {
		t.Fatal(err)
	}
	args := query.args[qReviewList]
	if args[0] != int64(7) || args[1] != "writer-a" || args[2] != "pending" || args[4] != "3" || args[6] != ",sampled,feedback," ||
		args[8] != int64(40) || args[10] != 11 {
		t.Fatalf("list args = %v", args)
	}
	item := items[0]
	if len(items) != 1 || item.ItemID != 12 || item.TenantID != 7 || item.Source != domain.ReviewSourceFeedback || item.Turn.RequestID != "req-1" ||
		item.Turn.Input.URI != "scout://state/in" || item.Turn.TurnNo != 2 || item.Status != domain.ReviewPending || item.Reviewer != (domain.PrincipalRef{}) {
		t.Fatalf("item = %+v", item)
	}

	for name, filter := range map[string]domain.ReviewFilter{
		"no agent":       {Limit: 1},
		"no limit":       {AgentID: "writer-a"},
		"bad status":     {AgentID: "writer-a", Limit: 1, Status: "open"},
		"bad source":     {AgentID: "writer-a", Limit: 1, Sources: []domain.ReviewSource{"chat"}},
		"negative after": {AgentID: "writer-a", Limit: 1, Before: -1},
	} {
		if _, err := store.List(ctx, 7, filter); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}

func TestReviewQueueStoreGradesOnceAndReplaysTheSameGrade(t *testing.T) {
	ctx := context.Background()
	grade := domain.ReviewGrade{Score: 2, Verdict: domain.ReviewFail, Notes: "invented a refund rule", ExpectedBehavior: []byte("cites the policy")}
	graded := reviewRow(12, "graded", int64(2), "fail", "invented a refund rule", "cites the policy", "9")
	query := newQueryFake(map[string][][]any{qReviewGrade: {{int64(12)}}, qReviewGet: {graded}})
	store := reviewQueue(query)
	item, err := store.Grade(ctx, 7, 12, grade, reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != domain.ReviewGraded || item.Grade.Score != 2 || string(item.Grade.ExpectedBehavior) != "cites the policy" || item.Reviewer != reviewer {
		t.Fatalf("graded = %+v", item)
	}
	if args := query.args[qReviewGrade]; args[0] != 2 || args[1] != "fail" || args[4] != "human" || args[5] != "9" || args[7] != int64(7) || args[8] != int64(12) {
		t.Fatalf("grade args = %v", args)
	}

	replay := reviewQueue(newQueryFake(map[string][][]any{qReviewGet: {graded}}))
	if _, err := replay.Grade(ctx, 7, 12, grade, reviewer); err != nil {
		t.Fatalf("same grade replay = %v", err)
	}
	other := grade
	other.Score = 4
	if _, err := replay.Grade(ctx, 7, 12, other, reviewer); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second grade = %v", err)
	}
	if _, err := replay.Grade(ctx, 7, 12, grade, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "10"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another grader = %v", err)
	}
	dismissed := reviewQueue(newQueryFake(map[string][][]any{qReviewGet: {reviewRow(12, "dismissed", nil, nil, "noise", nil, "9")}}))
	if _, err := dismissed.Grade(ctx, 7, 12, grade, reviewer); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("grade of a dismissed item = %v", err)
	}
	if _, err := reviewQueue(newQueryFake(nil)).Grade(ctx, 7, 99, grade, reviewer); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing item = %v", err)
	}

	for name, bad := range map[string]domain.ReviewGrade{
		"score zero":     {Score: 0, Verdict: domain.ReviewPass},
		"score six":      {Score: 6, Verdict: domain.ReviewPass},
		"unknown":        {Score: 3, Verdict: "maybe"},
		"long notes":     {Score: 3, Verdict: domain.ReviewPass, Notes: strings.Repeat("n", reviewNotesMax+1)},
		"large expected": {Score: 3, Verdict: domain.ReviewPass, ExpectedBehavior: []byte(strings.Repeat("e", DefaultReviewMaxExpectedBytes+1))},
		"invalid utf-8":  {Score: 3, Verdict: domain.ReviewPass, ExpectedBehavior: []byte{0xff}},
		"nul expected":   {Score: 3, Verdict: domain.ReviewPass, ExpectedBehavior: []byte("a\x00b")},
		"nul notes":      {Score: 3, Verdict: domain.ReviewPass, Notes: "a\x00b"},
	} {
		if _, err := store.Grade(ctx, 7, 12, bad, reviewer); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
	if _, err := store.Grade(ctx, 7, 12, grade, domain.PrincipalRef{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no grader = %v", err)
	}
}

func TestReviewQueueStoreDismissesAndMarksOnlyGradedTurnsPromoted(t *testing.T) {
	ctx := context.Background()
	dismissed := reviewQueue(newQueryFake(map[string][][]any{qReviewGet: {reviewRow(12, "dismissed", nil, nil, "noise", nil, "9")}}))
	if item, err := dismissed.Dismiss(ctx, 7, 12, " noise ", reviewer); err != nil || item.Status != domain.ReviewDismissed || item.Grade.Notes != "noise" {
		t.Fatalf("dismiss = %+v, %v", item, err)
	}
	if _, err := dismissed.Dismiss(ctx, 7, 12, "other", reviewer); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second dismissal = %v", err)
	}
	if _, err := dismissed.Dismiss(ctx, 7, 12, "a\x00b", reviewer); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("NUL in notes = %v", err)
	}

	promotedRow := reviewRow(12, "promoted", int64(2), "fail", nil, nil, "9")
	promotedRow[22], promotedRow[23], promotedRow[24] = "support", "review-12", testClock
	query := newQueryFake(map[string][][]any{qReviewGet: {promotedRow}})
	promoted := reviewQueue(query)
	if item, err := promoted.Promoted(ctx, 7, 12, "support", "review-12"); err != nil || item.PromotedCaseID != "review-12" {
		t.Fatalf("promoted = %+v, %v", item, err)
	}
	if args := query.args[qReviewPromoted]; args[0] != "support" || args[1] != "review-12" || args[3] != int64(7) || args[4] != int64(12) {
		t.Fatalf("promote args = %v", args)
	}
	pending := reviewQueue(newQueryFake(map[string][][]any{qReviewGet: {reviewRow(12, "pending", nil, nil, nil, nil, nil)}}))
	if _, err := pending.Promoted(ctx, 7, 12, "support", "review-12"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("promotion of a pending item = %v", err)
	}
	if _, err := pending.Promoted(ctx, 7, 12, "support", "../x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("path case id = %v", err)
	}
}
