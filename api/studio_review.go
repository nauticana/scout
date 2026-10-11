package api

import "time"

const (
	StudioReviewsPath       = StudioBasePath + "reviews"
	StudioReviewTurnPath    = StudioBasePath + "review/turn"
	StudioReviewGradePath   = StudioBasePath + "review/grade"
	StudioReviewDismissPath = StudioBasePath + "review/dismiss"
	StudioReviewPromotePath = StudioBasePath + "review/promote"
)

// ReviewGrade is a grader's score (1 to 5), pass or fail verdict, notes, and the
// behavior a promoted golden case expects.
type ReviewGrade struct {
	Score            int    `json:"score"`
	Verdict          string `json:"verdict"`
	Notes            string `json:"notes,omitempty"`
	ExpectedBehavior string `json:"expected_behavior,omitempty"`
}

// ReviewItem is one sampled or low-rated turn of a release under human review. It never
// names who rated the turn; reviewer is who graded or dismissed it.
type ReviewItem struct {
	ItemID       int64        `json:"item_id"`
	AgentName    string       `json:"agent_name"`
	Version      string       `json:"version"`
	Source       string       `json:"source"`
	Reason       string       `json:"reason"`
	RiskTier     string       `json:"risk_tier,omitempty"`
	Status       string       `json:"status"`
	EnqueuedAt   time.Time    `json:"enqueued_at"`
	Grade        *ReviewGrade `json:"grade,omitempty"`
	Notes        string       `json:"notes,omitempty"`
	ReviewerKind string       `json:"reviewer_kind,omitempty"`
	ReviewerID   string       `json:"reviewer_id,omitempty"`
	ReviewedAt   *time.Time   `json:"reviewed_at,omitempty"`
	GoldenSetID  string       `json:"golden_set_id,omitempty"`
	CaseID       string       `json:"case_id,omitempty"`
	PromotedAt   *time.Time   `json:"promoted_at,omitempty"`
}

// ReviewPage is GET reviews?agent_name=&status=&source=&release=&before=&limit=: items
// newest first; next_before continues the page and is absent on the last one.
type ReviewPage struct {
	Items      []ReviewItem `json:"items"`
	NextBefore int64        `json:"next_before,omitempty"`
}

// ReviewTurn is GET review/turn?item_id=: the item and its turn's input and response.
type ReviewTurn struct {
	Item     ReviewItem `json:"item"`
	Input    string     `json:"input"`
	Response string     `json:"response,omitempty"`
}

// ReviewGradeRequest is POST review/grade; an item takes one grade.
type ReviewGradeRequest struct {
	ItemID int64 `json:"item_id"`
	ReviewGrade
}

// ReviewDismissRequest is POST review/dismiss.
type ReviewDismissRequest struct {
	ItemID int64  `json:"item_id"`
	Notes  string `json:"notes,omitempty"`
}

// ReviewPromoteRequest is POST review/promote: the graded item becomes case review-<item_id>
// of the agent's golden set, whose current revision expected_revision names.
type ReviewPromoteRequest struct {
	ItemID           int64  `json:"item_id"`
	CaseName         string `json:"case_name,omitempty"`
	RiskTier         string `json:"risk_tier,omitempty"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// ReviewPromoted is the promoted item and the golden set after the promotion.
type ReviewPromoted struct {
	Item ReviewItem    `json:"item"`
	Set  GoldenCaseSet `json:"set"`
}
