package evaluation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qStudioReviewEvent = "scout_studio_review_event"

var studioReviewQueries = map[string]string{qStudioReviewEvent: studioEvaluationQueries[qStudioEvaluationEvent]}

// Studio lifecycle events StudioReview records.
const (
	StudioEventReviewGrade   = "REVIEW_GRADE"
	StudioEventReviewDismiss = "REVIEW_DISMISS"
	StudioEventReviewPromote = "REVIEW_PROMOTE"
)

// Defaults of StudioReview's limits.
const (
	DefaultReviewPageSize     = 50
	DefaultReviewMaxPageSize  = 200
	DefaultReviewMaxTurnBytes = 1 << 20
)

// ReviewCasePrefix prefixes the golden case id a promoted review item becomes.
const ReviewCasePrefix = "review-"

// StudioReview serves Studio's Review tab over the human review queue. An item of an agent
// the actor cannot VIEW reads as not found. Listing, reading, grading, and dismissing need
// REVIEW on the item's agent, and promotion also EDIT. Replays are not audited again, and
// items and pages never name who rated a turn.
type StudioReview struct {
	DB          keelport.DatabaseRepository
	Authorizer  contract.StudioAuthorizer
	Queue       contract.HumanReviewQueue
	GoldenCases contract.GoldenCaseStore
	// Objects reads a turn's stored input and response; the service verifies their digests.
	Objects contract.ObjectReader
	// PageSize answers a page that names no limit and MaxPageSize caps one that does; zero
	// takes DefaultReviewPageSize and DefaultReviewMaxPageSize.
	PageSize    int
	MaxPageSize int
	// MaxTurnBytes bounds the turn input or response a reviewer reads or promotes; zero takes DefaultReviewMaxTurnBytes.
	MaxTurnBytes int

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioReviewBackend = (*StudioReview)(nil)

func (s *StudioReview) init(ctx context.Context) error {
	if s.DB == nil || s.Authorizer == nil || s.Queue == nil || s.GoldenCases == nil || s.Objects == nil {
		return fmt.Errorf("studio review: database, authorizer, review queue, case store, and object reader are required")
	}
	pageSize, maxPageSize := orDefault(s.PageSize, DefaultReviewPageSize), orDefault(s.MaxPageSize, DefaultReviewMaxPageSize)
	if s.PageSize < 0 || s.MaxPageSize < 0 || s.MaxTurnBytes < 0 || pageSize > maxPageSize {
		return fmt.Errorf("%w: review page size %d must be non-negative and within the largest page %d, and the turn bound non-negative",
			domain.ErrValidation, pageSize, maxPageSize)
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, studioReviewQueries) })
	if s.qs == nil {
		return fmt.Errorf("studio review: query service is required")
	}
	return nil
}

func (s *StudioReview) audit(ctx context.Context, actor domain.StudioActor, agentID, event, detail string) error {
	if _, err := s.qs.Query(ctx, qStudioReviewEvent, actor.TenantID, agentID, event, nullable(truncate(detail, studioEventDetailMax)),
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return fmt.Errorf("record studio %s event: %w", strings.ToLower(event), err)
	}
	return nil
}

// Reviews returns one page of an agent's review items, newest first.
func (s *StudioReview) Reviews(ctx context.Context, actor domain.StudioActor, filter domain.ReviewFilter) (domain.ReviewPage, error) {
	if err := s.init(ctx); err != nil {
		return domain.ReviewPage{}, err
	}
	maxPageSize := orDefault(s.MaxPageSize, DefaultReviewMaxPageSize)
	if filter.Limit < 0 || filter.Limit > maxPageSize || filter.Before < 0 {
		return domain.ReviewPage{}, fmt.Errorf("%w: limit must be within 0..%d and before non-negative", domain.ErrValidation, maxPageSize)
	}
	if filter.Limit == 0 {
		filter.Limit = orDefault(s.PageSize, DefaultReviewPageSize)
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioReview, filter.AgentID); err != nil {
		return domain.ReviewPage{}, err
	}
	limit := filter.Limit
	filter.Limit++
	items, err := s.Queue.List(ctx, actor.TenantID, filter)
	if err != nil {
		return domain.ReviewPage{}, err
	}
	page := domain.ReviewPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextBefore = items[limit-1].ItemID
	}
	return page, nil
}

// item loads an item the actor may VIEW and authorizes action on its agent.
func (s *StudioReview) item(ctx context.Context, actor domain.StudioActor, itemID int64, actions ...domain.StudioAction) (domain.HumanReviewItem, error) {
	if err := s.init(ctx); err != nil {
		return domain.HumanReviewItem{}, err
	}
	item, err := s.Queue.Get(ctx, actor.TenantID, itemID)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	if err = s.Authorizer.Authorize(ctx, actor, domain.StudioView, item.AgentID); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return domain.HumanReviewItem{}, fmt.Errorf("%w: review item %d", domain.ErrNotFound, itemID)
		}
		return domain.HumanReviewItem{}, err
	}
	for _, action := range actions {
		if err = s.Authorizer.Authorize(ctx, actor, action, item.AgentID); err != nil {
			return domain.HumanReviewItem{}, err
		}
	}
	return item, nil
}

func (s *StudioReview) read(ctx context.Context, ref domain.ObjectRef, what string) ([]byte, error) {
	if ref.URI == "" {
		return nil, nil
	}
	content, err := s.Objects.Read(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("read turn %s: %w", what, err)
	}
	if maxBytes := orDefault(s.MaxTurnBytes, DefaultReviewMaxTurnBytes); len(content) > maxBytes {
		return nil, fmt.Errorf("%w: turn %s exceeds %d bytes", domain.ErrValidation, what, maxBytes)
	}
	if sha256Hex(content) != ref.Digest {
		return nil, fmt.Errorf("turn %s does not match its digest", what)
	}
	return content, nil
}

// ReviewTurn returns the item's turn input and response, each checked against its digest.
func (s *StudioReview) ReviewTurn(ctx context.Context, actor domain.StudioActor, itemID int64) (domain.ReviewTurnContent, error) {
	item, err := s.item(ctx, actor, itemID, domain.StudioReview)
	if err != nil {
		return domain.ReviewTurnContent{}, err
	}
	content := domain.ReviewTurnContent{Item: item}
	if content.Input, err = s.read(ctx, item.Turn.Input, "input"); err != nil {
		return domain.ReviewTurnContent{}, err
	}
	if content.Response, err = s.read(ctx, item.Turn.Response, "response"); err != nil {
		return domain.ReviewTurnContent{}, err
	}
	return content, nil
}

// Grade records the actor's grade of a pending item.
func (s *StudioReview) Grade(ctx context.Context, actor domain.StudioActor, itemID int64, grade domain.ReviewGrade) (domain.HumanReviewItem, error) {
	item, err := s.item(ctx, actor, itemID, domain.StudioReview)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	replay := item.Status != domain.ReviewPending
	if item, err = s.Queue.Grade(ctx, actor.TenantID, itemID, grade, actor.Principal); err != nil || replay {
		return item, err
	}
	detail := fmt.Sprintf("item %d of %s %s %d", itemID, item.AgentVersion, item.Grade.Verdict, item.Grade.Score)
	if err = s.audit(ctx, actor, item.AgentID, StudioEventReviewGrade, detail); err != nil {
		return domain.HumanReviewItem{}, err
	}
	return item, nil
}

// Dismiss resolves a pending item without a grade.
func (s *StudioReview) Dismiss(ctx context.Context, actor domain.StudioActor, itemID int64, notes string) (domain.HumanReviewItem, error) {
	item, err := s.item(ctx, actor, itemID, domain.StudioReview)
	if err != nil {
		return domain.HumanReviewItem{}, err
	}
	replay := item.Status != domain.ReviewPending
	if item, err = s.Queue.Dismiss(ctx, actor.TenantID, itemID, notes, actor.Principal); err != nil || replay {
		return item, err
	}
	if err = s.audit(ctx, actor, item.AgentID, StudioEventReviewDismiss, fmt.Sprintf("item %d of %s", itemID, item.AgentVersion)); err != nil {
		return domain.HumanReviewItem{}, err
	}
	return item, nil
}

// Promote copies a graded turn into the agent's golden set as dev case review-<item>: the
// turn's stored input, and the grade's expected behavior. The set's case limits and gate
// rules apply. Replaying a promoted item writes nothing, and a retry after the case was
// written only marks the item promoted.
func (s *StudioReview) Promote(ctx context.Context, actor domain.StudioActor, promotion domain.ReviewPromotion) (domain.HumanReviewItem, domain.GoldenCaseSet, error) {
	item, err := s.item(ctx, actor, promotion.ItemID, domain.StudioReview, domain.StudioEdit)
	if err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	caseID := fmt.Sprintf("%s%d", ReviewCasePrefix, item.ItemID)
	if item.Status == domain.ReviewPromoted && item.PromotedCaseID == caseID {
		return s.promoted(ctx, actor, item)
	}
	if item.Status != domain.ReviewGraded {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, fmt.Errorf("%w: only a graded review item is promoted; item %d is %s", domain.ErrConflict, item.ItemID, item.Status)
	}
	name := strings.TrimSpace(promotion.CaseName)
	if name == "" {
		name = fmt.Sprintf("Review %d", item.ItemID)
	}
	if utf8.RuneCountInString(name) > 200 {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, fmt.Errorf("%w: case name exceeds 200 characters", domain.ErrValidation)
	}
	goldenSetID, _, err := s.GoldenCases.AgentSet(ctx, actor.TenantID, item.AgentID)
	if err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	written, err := s.caseWritten(ctx, actor, item, caseID)
	if err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	if !written {
		input, err := s.read(ctx, item.Turn.Input, "input")
		if err != nil {
			return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
		}
		if _, err = s.GoldenCases.PutCase(ctx, domain.GoldenScopeDev, domain.GoldenCase{
			TenantID: actor.TenantID, GoldenSetID: goldenSetID, CaseID: caseID, Name: name, Input: input,
			ExpectedBehavior: item.Grade.ExpectedBehavior, RiskTier: promotion.RiskTier, ModifiedBy: actor.Principal, ReviewItemID: item.ItemID,
		}, promotion.ExpectedRevision); err != nil {
			return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
		}
	}
	if item, err = s.Queue.Promoted(ctx, actor.TenantID, item.ItemID, goldenSetID, caseID); err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	if err = s.audit(ctx, actor, item.AgentID, StudioEventReviewPromote, fmt.Sprintf("item %d to %s/%s", item.ItemID, goldenSetID, caseID)); err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	return s.promoted(ctx, actor, item)
}

// caseWritten reports whether an earlier attempt already wrote the item's case; a case of
// that id from anywhere else is a conflict rather than something to overwrite.
func (s *StudioReview) caseWritten(ctx context.Context, actor domain.StudioActor, item domain.HumanReviewItem, caseID string) (bool, error) {
	set, err := s.GoldenCases.Cases(ctx, domain.GoldenScopeDev, actor.TenantID, item.AgentID)
	if err != nil {
		return false, err
	}
	for _, goldenCase := range set.Cases {
		if goldenCase.CaseID != caseID {
			continue
		}
		if goldenCase.ReviewItemID != item.ItemID {
			return false, fmt.Errorf("%w: golden case %q exists and was not promoted from item %d", domain.ErrConflict, caseID, item.ItemID)
		}
		return true, nil
	}
	return false, nil
}

func (s *StudioReview) promoted(ctx context.Context, actor domain.StudioActor, item domain.HumanReviewItem) (domain.HumanReviewItem, domain.GoldenCaseSet, error) {
	set, err := s.GoldenCases.Cases(ctx, domain.GoldenScopeDev, actor.TenantID, item.AgentID)
	if err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	if set, err = withholdReviewInputs(ctx, s.Authorizer, actor, set); err != nil {
		return domain.HumanReviewItem{}, domain.GoldenCaseSet{}, err
	}
	return item, set, nil
}
