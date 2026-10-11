package domain

import "time"

// StudioReview lets a person grade, dismiss, and read production turns queued for human review.
const StudioReview StudioAction = "REVIEW"

// ReviewSource says how a production turn entered the human review queue: the sampler
// selected it, or a person rated it at or below the threshold.
type ReviewSource string

const (
	ReviewSourceSampled  ReviewSource = "S"
	ReviewSourceFeedback ReviewSource = "F"
)

// Valid reports whether the source is a known one.
func (source ReviewSource) Valid() bool {
	return source == ReviewSourceSampled || source == ReviewSourceFeedback
}

// ReviewStatus is a review item's state: pending until graded or dismissed, and a
// graded turn may be promoted into its agent's golden set.
type ReviewStatus string

const (
	ReviewPending   ReviewStatus = "P"
	ReviewGraded    ReviewStatus = "G"
	ReviewDismissed ReviewStatus = "D"
	ReviewPromoted  ReviewStatus = "M"
)

// ReviewVerdict is a grader's pass or fail.
type ReviewVerdict string

const (
	ReviewPass ReviewVerdict = "P"
	ReviewFail ReviewVerdict = "F"
)

// Review grade scale.
const (
	ReviewMinScore = 1
	ReviewMaxScore = 5
)

// ReviewGrade is one person's grade of a review item; ExpectedBehavior becomes a
// promoted golden case's expected behavior.
type ReviewGrade struct {
	Score            int
	Verdict          ReviewVerdict
	Notes            string
	ExpectedBehavior []byte
}

// ReviewTurnRef names a production turn and its stored input and response objects.
type ReviewTurnRef struct {
	RequestID      string
	ConversationID string
	TurnNo         int64
	Input          ObjectRef
	Response       ObjectRef
}

// HumanReviewItem is one production turn awaiting, or given, a human grade. It holds
// references only; content stays in object storage. Who rated a turn is never part of an item.
type HumanReviewItem struct {
	ItemID       int64
	TenantID     int64
	AgentID      string
	AgentVersion string
	Source       ReviewSource
	Reason       string
	RiskTier     string
	Turn         ReviewTurnRef
	Status       ReviewStatus
	EnqueuedAt   time.Time
	// Grade, Reviewer, and ReviewedAt are set once the item is graded or dismissed; a
	// dismissal carries only Notes.
	Grade      ReviewGrade
	Reviewer   PrincipalRef
	ReviewedAt time.Time
	// PromotedSetID and PromotedCaseID name the golden case a promotion wrote.
	PromotedSetID  string
	PromotedCaseID string
	PromotedAt     time.Time
}

// ReviewFilter selects one agent's review items, newest first; Before continues a page.
type ReviewFilter struct {
	AgentID string
	Status  ReviewStatus
	Release string
	// Sources restricts the listing to these sources; empty lists every source.
	Sources []ReviewSource
	Before  int64
	Limit   int
}

// ReviewPage is one page of review items; NextBefore continues it and is zero on the last page.
type ReviewPage struct {
	Items      []HumanReviewItem
	NextBefore int64
}

// ReviewTurnContent is a production turn's input and response as a reviewer reads them.
type ReviewTurnContent struct {
	Item     HumanReviewItem
	Input    []byte
	Response []byte
}

// ReviewPromotion asks to copy a graded production turn into its agent's golden set as
// a dev case behind the set's working revision.
type ReviewPromotion struct {
	ItemID           int64
	CaseName         string
	RiskTier         string
	ExpectedRevision int64
}

// ReviewTurn identifies a production turn offered to human review.
type ReviewTurn struct {
	TenantContext TenantContext
	RequestID     string
	AgentID       string
	AgentVersion  string
}
