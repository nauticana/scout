package evaluation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qReviewTurn       = "scout_review_turn"
	qReviewExisting   = "scout_review_existing"
	qReviewPending    = "scout_review_pending"
	qReviewTurnInsert = "scout_review_turn_insert"
	qReviewGet        = "scout_review_get"
	qReviewList       = "scout_review_list"
	qReviewGrade      = "scout_review_grade"
	qReviewDismiss    = "scout_review_dismiss"
	qReviewPromoted   = "scout_review_promoted"
)

const reviewColumns = `h.id, h.agent_id, h.agent_version, h.source_code, h.reason, h.risk_tier,
       h.conversation_id, h.turn_no, t.request_id, t.input_uri, t.input_digest, t.response_uri,
       t.response_digest, h.status_code, h.enqueued_at, h.score, h.verdict_code, h.notes, h.expected_behavior,
       h.reviewer_kind, h.reviewer_id, h.reviewed_at, h.promoted_set_id, h.promoted_case_id, h.promoted_at
  FROM human_review_item h
  JOIN conversation_turn t ON t.partner_id = h.partner_id AND t.conversation_id = h.conversation_id AND t.turn_no = h.turn_no`

const reviewColumnCount = 25

var reviewQueries = map[string]string{
	qReviewTurn: `
SELECT t.conversation_id, t.turn_no, t.status_code, c.agent_id, c.agent_version
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND t.request_id = ?`,
	qReviewExisting: `
SELECT id FROM human_review_item
 WHERE partner_id = ? AND conversation_id = ? AND turn_no = ?`,
	qReviewPending: `
SELECT COUNT(*) FROM human_review_item
 WHERE partner_id = ? AND agent_id = ? AND status_code = ? AND source_code IN (?, ?)`,
	qReviewTurnInsert: `
INSERT INTO human_review_item (id, partner_id, agent_id, agent_version, source_code, reason, risk_tier, conversation_id, turn_no, enqueued_at)
VALUES (NEXTVAL('human_review_item_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING`,
	qReviewGet: `
SELECT ` + reviewColumns + `
 WHERE h.partner_id = ? AND h.id = ?`,
	// Sources arrive as ",a,b," so one statement filters any subset.
	qReviewList: `
SELECT ` + reviewColumns + `
 WHERE h.partner_id = ? AND h.agent_id = ?
   AND (? = '' OR h.status_code = ?)
   AND (? = '' OR h.agent_version = ?)
   AND (? = '' OR STRPOS(?, ',' || h.source_code || ',') > 0)
   AND (? = 0 OR h.id < ?)
 ORDER BY h.id DESC
 LIMIT ?`,
	qReviewGrade: `
UPDATE human_review_item
   SET status_code = ?, score = ?, verdict_code = ?, notes = ?, expected_behavior = ?,
       reviewer_kind = ?, reviewer_id = ?, reviewed_at = ?
 WHERE partner_id = ? AND id = ? AND status_code = ?
RETURNING id`,
	qReviewDismiss: `
UPDATE human_review_item
   SET status_code = ?, notes = ?, reviewer_kind = ?, reviewer_id = ?, reviewed_at = ?
 WHERE partner_id = ? AND id = ? AND status_code = ?
RETURNING id`,
	qReviewPromoted: `
UPDATE human_review_item
   SET status_code = ?, promoted_set_id = ?, promoted_case_id = ?, promoted_at = ?
 WHERE partner_id = ? AND id = ? AND status_code = ?
RETURNING id`,
}

// Defaults of ReviewQueueStore's limits.
const (
	DefaultReviewMaxPending       = 1000
	DefaultReviewMaxExpectedBytes = 32 << 10
	reviewNotesMax                = 2000
	reviewReasonMax               = 400
)

// ReviewQueueStore is the durable HumanReviewQueue over human_review_item. An item is
// derived from its completed turn, so a caller cannot attach a turn to another agent or
// release, and it keeps only references to the turn's stored objects.
type ReviewQueueStore struct {
	keelStore
	// MaxPending bounds one agent's pending items, approximately: concurrent enqueues may overshoot
	// it. Zero means DefaultReviewMaxPending.
	MaxPending int
	// MaxExpectedBytes bounds a grade's expected behavior; zero means DefaultReviewMaxExpectedBytes.
	MaxExpectedBytes int
	Now              func() time.Time
}

var _ contract.HumanReviewQueue = (*ReviewQueueStore)(nil)

func (store *ReviewQueueStore) now() time.Time {
	if store.Now != nil {
		return store.Now().UTC()
	}
	return time.Now().UTC()
}

func (store *ReviewQueueStore) validate() error {
	if store.MaxPending < 0 || store.MaxExpectedBytes < 0 {
		return fmt.Errorf("%w: review queue limits cannot be negative", domain.ErrValidation)
	}
	return nil
}

// Enqueue adds a completed turn once.
func (store *ReviewQueueStore) Enqueue(ctx context.Context, item domain.HumanReviewItem) error {
	if err := store.validate(); err != nil {
		return err
	}
	reason := common.TruncateRunes(strings.TrimSpace(item.Reason), reviewReasonMax)
	if item.TenantID <= 0 || !item.Source.Valid() || reason == "" || strings.TrimSpace(item.Turn.RequestID) == "" || utf8.RuneCountInString(item.RiskTier) > 20 {
		return fmt.Errorf("%w: a review item needs a tenant, a known source, a reason, its turn's request id, and a risk tier of at most 20 characters", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "review queue store", reviewQueries)
	if err != nil {
		return err
	}
	turn, err := qs.Query(ctx, qReviewTurn, item.TenantID, item.Turn.RequestID)
	if err != nil {
		return fmt.Errorf("load reviewed turn: %w", err)
	}
	if len(turn.Rows) == 0 {
		return fmt.Errorf("%w: turn %q", domain.ErrNotFound, item.Turn.RequestID)
	}
	row := turn.Rows[0]
	if common.AsString(row[2]) != "completed" {
		return fmt.Errorf("%w: only a completed turn is reviewed", domain.ErrConflict)
	}
	agentID, agentVersion := common.AsString(row[3]), common.AsString(row[4])
	conversationID, turnNo := common.AsString(row[0]), common.AsInt64(row[1])
	existing, err := qs.Query(ctx, qReviewExisting, item.TenantID, conversationID, turnNo)
	if err != nil {
		return fmt.Errorf("find existing review: %w", err)
	}
	if len(existing.Rows) > 0 {
		return nil
	}
	pending, err := qs.Query(ctx, qReviewPending, item.TenantID, agentID, string(domain.ReviewPending),
		string(domain.ReviewSourceSampled), string(domain.ReviewSourceFeedback))
	if err != nil {
		return fmt.Errorf("count pending reviews: %w", err)
	}
	if maxPending := orDefault(store.MaxPending, DefaultReviewMaxPending); len(pending.Rows) > 0 && common.AsInt64(pending.Rows[0][0]) >= int64(maxPending) {
		return fmt.Errorf("%w: agent %q already holds %d pending reviews", domain.ErrRateLimited, agentID, maxPending)
	}
	if _, err = qs.Query(ctx, qReviewTurnInsert, item.TenantID, agentID, agentVersion, string(item.Source), reason, nullable(item.RiskTier),
		conversationID, turnNo, store.now()); err != nil {
		return fmt.Errorf("enqueue turn review: %w", err)
	}
	return nil
}

// Get returns one item of the tenant.
func (store *ReviewQueueStore) Get(ctx context.Context, tenantID, itemID int64) (domain.HumanReviewItem, error) {
	if tenantID <= 0 || itemID <= 0 {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: tenant and item id are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "review queue store", reviewQueries)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	result, err := qs.Query(ctx, qReviewGet, tenantID, itemID)
	if err != nil {
		return domain.HumanReviewItem{}, fmt.Errorf("load review item: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: review item %d", domain.ErrNotFound, itemID)
	}
	return reviewItem(tenantID, result.Rows[0])
}

// List returns one page of an agent's items, newest first.
func (store *ReviewQueueStore) List(ctx context.Context, tenantID int64, filter domain.ReviewFilter) ([]domain.HumanReviewItem, error) {
	if tenantID <= 0 || strings.TrimSpace(filter.AgentID) == "" || filter.Before < 0 || filter.Limit <= 0 {
		return nil, fmt.Errorf("%w: tenant, agent, a non-negative cursor, and a positive limit are required", domain.ErrValidation)
	}
	if err := validReviewStatus(filter.Status, true); err != nil {
		return nil, err
	}
	sources := ""
	for _, source := range filter.Sources {
		if !source.Valid() {
			return nil, fmt.Errorf("%w: unknown review source %q", domain.ErrValidation, source)
		}
		sources += "," + string(source)
	}
	if sources != "" {
		sources += ","
	}
	qs, err := store.queries(ctx, "review queue store", reviewQueries)
	if err != nil {
		return nil, err
	}
	status := string(filter.Status)
	result, err := qs.Query(ctx, qReviewList, tenantID, filter.AgentID, status, status, filter.Release, filter.Release,
		sources, sources, filter.Before, filter.Before, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list review items: %w", err)
	}
	items := make([]domain.HumanReviewItem, 0, len(result.Rows))
	for _, row := range result.Rows {
		item, err := reviewItem(tenantID, row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (store *ReviewQueueStore) validGrade(grade domain.ReviewGrade) (domain.ReviewGrade, error) {
	maxBytes := orDefault(store.MaxExpectedBytes, DefaultReviewMaxExpectedBytes)
	grade.Notes = strings.TrimSpace(grade.Notes)
	if len(bytes.TrimSpace(grade.ExpectedBehavior)) == 0 {
		grade.ExpectedBehavior = nil
	}
	switch {
	case grade.Score < domain.ReviewMinScore || grade.Score > domain.ReviewMaxScore:
		return grade, fmt.Errorf("%w: score must be %d to %d", domain.ErrValidation, domain.ReviewMinScore, domain.ReviewMaxScore)
	case grade.Verdict != domain.ReviewPass && grade.Verdict != domain.ReviewFail:
		return grade, fmt.Errorf("%w: verdict must be %s or %s", domain.ErrValidation, domain.ReviewPass, domain.ReviewFail)
	case utf8.RuneCountInString(grade.Notes) > reviewNotesMax || !validText([]byte(grade.Notes)):
		return grade, fmt.Errorf("%w: notes must be at most %d characters of UTF-8 without NUL", domain.ErrValidation, reviewNotesMax)
	case len(grade.ExpectedBehavior) > maxBytes || !validText(grade.ExpectedBehavior):
		return grade, fmt.Errorf("%w: expected behavior must be at most %d bytes of UTF-8 without NUL", domain.ErrValidation, maxBytes)
	}
	return grade, nil
}

// Grade records the reviewer's grade of a pending item once.
func (store *ReviewQueueStore) Grade(ctx context.Context, tenantID, itemID int64, grade domain.ReviewGrade, reviewer domain.PrincipalRef) (domain.HumanReviewItem, error) {
	if err := store.validate(); err != nil {
		return domain.HumanReviewItem{}, err
	}
	grade, err := store.validGrade(grade)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if tenantID <= 0 || itemID <= 0 || reviewer.Kind == "" || reviewer.ID == "" {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: tenant, item id, and reviewer are required", domain.ErrValidation)
	}
	qs, err := store.queries(ctx, "review queue store", reviewQueries)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if _, err = qs.Query(ctx, qReviewGrade, string(domain.ReviewGraded), grade.Score, string(grade.Verdict), nullable(grade.Notes), nullable(string(grade.ExpectedBehavior)),
		string(reviewer.Kind), reviewer.ID, store.now(), tenantID, itemID, string(domain.ReviewPending)); err != nil {
		return domain.HumanReviewItem{}, fmt.Errorf("grade review item: %w", err)
	}
	item, err := store.Get(ctx, tenantID, itemID)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	graded := item.Status == domain.ReviewGraded || item.Status == domain.ReviewPromoted
	if !graded || item.Reviewer != reviewer || item.Grade.Score != grade.Score || item.Grade.Verdict != grade.Verdict ||
		item.Grade.Notes != grade.Notes || !bytes.Equal(item.Grade.ExpectedBehavior, grade.ExpectedBehavior) {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: review item %d is already %s", domain.ErrConflict, itemID, item.Status)
	}
	return item, nil
}

// Dismiss resolves a pending item without a grade once.
func (store *ReviewQueueStore) Dismiss(ctx context.Context, tenantID, itemID int64, notes string, reviewer domain.PrincipalRef) (domain.HumanReviewItem, error) {
	notes = strings.TrimSpace(notes)
	if tenantID <= 0 || itemID <= 0 || reviewer.Kind == "" || reviewer.ID == "" || utf8.RuneCountInString(notes) > reviewNotesMax || !validText([]byte(notes)) {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: tenant, item id, reviewer, and notes of at most %d characters without NUL are required", domain.ErrValidation, reviewNotesMax)
	}
	qs, err := store.queries(ctx, "review queue store", reviewQueries)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if _, err = qs.Query(ctx, qReviewDismiss, string(domain.ReviewDismissed), nullable(notes), string(reviewer.Kind), reviewer.ID, store.now(),
		tenantID, itemID, string(domain.ReviewPending)); err != nil {
		return domain.HumanReviewItem{}, fmt.Errorf("dismiss review item: %w", err)
	}
	item, err := store.Get(ctx, tenantID, itemID)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if item.Status != domain.ReviewDismissed || item.Reviewer != reviewer || item.Grade.Notes != notes {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: review item %d is already %s", domain.ErrConflict, itemID, item.Status)
	}
	return item, nil
}

// Promoted marks a graded item promoted into the golden case.
func (store *ReviewQueueStore) Promoted(ctx context.Context, tenantID, itemID int64, goldenSetID, caseID string) (domain.HumanReviewItem, error) {
	if tenantID <= 0 || itemID <= 0 {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: tenant and item id are required", domain.ErrValidation)
	}
	if err := errors.Join(validIdentifier("golden_set_id", goldenSetID, 80), validIdentifier("case_id", caseID, 120)); err != nil {
		return domain.HumanReviewItem{}, err
	}
	qs, err := store.queries(ctx, "review queue store", reviewQueries)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if _, err = qs.Query(ctx, qReviewPromoted, string(domain.ReviewPromoted), goldenSetID, caseID, store.now(), tenantID, itemID,
		string(domain.ReviewGraded)); err != nil {
		return domain.HumanReviewItem{}, fmt.Errorf("mark review item promoted: %w", err)
	}
	item, err := store.Get(ctx, tenantID, itemID)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if item.Status != domain.ReviewPromoted || item.PromotedSetID != goldenSetID || item.PromotedCaseID != caseID {
		return domain.HumanReviewItem{}, fmt.Errorf("%w: review item %d is %s, not graded", domain.ErrConflict, itemID, item.Status)
	}
	return item, nil
}

func validReviewStatus(status domain.ReviewStatus, allowEmpty bool) error {
	switch status {
	case domain.ReviewPending, domain.ReviewGraded, domain.ReviewDismissed, domain.ReviewPromoted:
		return nil
	case "":
		if allowEmpty {
			return nil
		}
	}
	return fmt.Errorf("%w: unknown review status %q", domain.ErrValidation, status)
}

func reviewItem(tenantID int64, row []any) (domain.HumanReviewItem, error) {
	if len(row) < reviewColumnCount {
		return domain.HumanReviewItem{}, fmt.Errorf("review item row: expected %d columns, got %d", reviewColumnCount, len(row))
	}
	item := domain.HumanReviewItem{
		ItemID: common.AsInt64(row[0]), TenantID: tenantID, AgentID: common.AsString(row[1]), AgentVersion: common.AsString(row[2]),
		Source: domain.ReviewSource(common.AsString(row[3])), Reason: common.AsString(row[4]), RiskTier: common.AsString(row[5]),
		Turn: domain.ReviewTurnRef{
			ConversationID: common.AsString(row[6]), TurnNo: common.AsInt64(row[7]), RequestID: common.AsString(row[8]),
			Input:    domain.ObjectRef{URI: common.AsString(row[9]), Digest: common.AsString(row[10])},
			Response: domain.ObjectRef{URI: common.AsString(row[11]), Digest: common.AsString(row[12])},
		},
		Status: domain.ReviewStatus(common.AsString(row[13])), EnqueuedAt: common.AsTime(row[14]),
		Grade: domain.ReviewGrade{
			Score: int(common.AsInt64(row[15])), Verdict: domain.ReviewVerdict(common.AsString(row[16])), Notes: common.AsString(row[17]),
		},
		Reviewer:      domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[19])), ID: common.AsString(row[20])},
		ReviewedAt:    common.AsTime(row[21]),
		PromotedSetID: common.AsString(row[22]), PromotedCaseID: common.AsString(row[23]), PromotedAt: common.AsTime(row[24]),
	}
	if expected := common.AsString(row[18]); expected != "" {
		item.Grade.ExpectedBehavior = []byte(expected)
	}
	if err := validReviewStatus(item.Status, false); err != nil {
		return domain.HumanReviewItem{}, fmt.Errorf("review item %d: %w", item.ItemID, err)
	}
	if !item.Source.Valid() {
		return domain.HumanReviewItem{}, fmt.Errorf("review item %d: unknown source %q", item.ItemID, item.Source)
	}
	return item, nil
}
