package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const (
	qReviewLockDraft    = "scout_review_lock_draft"
	qReviewSupersede    = "scout_review_supersede"
	qReviewInsert       = "scout_review_insert"
	qReviewLatest       = "scout_review_latest"
	qReviewGet          = "scout_review_get"
	qReviewDecide       = "scout_review_decide"
	qReviewClaim        = "scout_review_claim"
	qReviewConsume      = "scout_review_consume"
	qReviewOpenComments = "scout_review_open_comments"
	qReviewActive       = "scout_review_active_definition"
	qReviewActiveLocked = "scout_review_active_definition_locked"
	qReviewAudit        = "scout_review_audit"
	qReviewDraftEditors = "scout_review_draft_editors"

	qReviewSupersedeRestore = "scout_review_supersede_restore"
	qReviewLatestRestore    = "scout_review_latest_restore"
	qReviewClaimRestore     = "scout_review_claim_restore"
	qReviewVersion          = "scout_review_version"

	reviewCatalogID     = "scout.controlplane.publish_review"
	maxReviewNote       = 400
	maxReviewException  = 200
	maxReviewEvaluation = 50
)

const reviewColumns = `id, definition_digest, draft_revision, prompt_profile_revision, base_version, diff, evaluations,
       status_code, requested_by, request_note, requested_at, decided_by, decision_note, decided_at,
       sod_exception_id, published_version, restore_version`

// reviewLatest is the agent's current publish review: a newer request withdraws any earlier approval.
const reviewLatest = `
SELECT ` + reviewColumns + `
  FROM agent_publish_review
 WHERE partner_id = ? AND agent_id = ? AND status_code <> 'superseded' AND restore_version IS NULL
 ORDER BY id DESC
 LIMIT 1`

// reviewLatestRestore is the agent's current restore review, whichever version it names.
const reviewLatestRestore = `
SELECT ` + reviewColumns + `
  FROM agent_publish_review
 WHERE partner_id = ? AND agent_id = ? AND status_code <> 'superseded' AND restore_version IS NOT NULL
 ORDER BY id DESC
 LIMIT 1`

var reviewQueries = map[string]string{
	qReviewLockDraft: `
SELECT draft_revision FROM agent_draft WHERE partner_id = ? AND agent_id = ? FOR UPDATE`,
	qReviewSupersede: `
UPDATE agent_publish_review SET status_code = 'superseded'
 WHERE partner_id = ? AND agent_id = ? AND status_code = 'requested' AND restore_version IS NULL`,
	qReviewSupersedeRestore: `
UPDATE agent_publish_review SET status_code = 'superseded'
 WHERE partner_id = ? AND agent_id = ? AND status_code = 'requested' AND restore_version IS NOT NULL`,
	qReviewInsert: `
INSERT INTO agent_publish_review (id, partner_id, agent_id, definition_digest, draft_revision, prompt_profile_revision,
       base_version, restore_version, diff, evaluations, status_code, requested_by, request_note, sod_exception_id)
VALUES (nextval('agent_publish_review_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, 'requested', ?, ?, ?)
RETURNING id, requested_at`,
	qReviewLatest:        reviewLatest,
	qReviewLatestRestore: reviewLatestRestore,
	qReviewClaimRestore: reviewLatestRestore + `
   FOR UPDATE`,
	qReviewVersion: `
SELECT definition FROM agent_version WHERE partner_id = ? AND agent_id = ? AND agent_version = ?`,
	qReviewGet: `
SELECT ` + reviewColumns + `
  FROM agent_publish_review
 WHERE partner_id = ? AND agent_id = ? AND id = ?`,
	qReviewDecide: `
UPDATE agent_publish_review
   SET status_code = ?, decided_by = ?, decision_note = ?, decided_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND id = ? AND status_code = 'requested' AND definition_digest = ?
RETURNING decided_at`,
	qReviewClaim: reviewLatest + `
   FOR UPDATE`,
	qReviewConsume: `
UPDATE agent_publish_review SET published_version = ?
 WHERE partner_id = ? AND agent_id = ? AND id = ? AND status_code = 'approved' AND published_version IS NULL
RETURNING id`,
	qReviewOpenComments: `
SELECT COUNT(*) FROM agent_draft_comment WHERE partner_id = ? AND agent_id = ? AND status_code = 'open'`,
	qReviewActive: `
SELECT v.agent_version, v.definition
  FROM agent_deployment dep
  JOIN agent_version v ON v.partner_id = dep.partner_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE dep.partner_id = ? AND dep.agent_id = ?`,
	// In the restore transaction the deployment stays locked until the restore deploys over it.
	qReviewActiveLocked: `
SELECT v.agent_version, v.definition
  FROM agent_deployment dep
  JOIN agent_version v ON v.partner_id = dep.partner_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE dep.partner_id = ? AND dep.agent_id = ?
   FOR UPDATE OF dep`,
	// The people who changed the agent's draft since its last publish or restore.
	qReviewDraftEditors: `
SELECT DISTINCT e.principal_id
  FROM agent_studio_event e
 WHERE e.partner_id = ? AND e.agent_id = ? AND e.principal_kind = 'human'
   AND e.event IN ('DRAFT_SAVE', 'ENABLE', 'DISABLE', 'SETTING', 'GUARDRAILS', 'GOVERNANCE', 'RESET', 'SET_DEFAULT')
   AND NOT EXISTS (SELECT 1 FROM agent_studio_event p
                    WHERE p.partner_id = e.partner_id AND p.agent_id = e.agent_id
                      AND p.event IN ('PUBLISH', 'RESTORE') AND p.id > e.id)`,
	qReviewAudit: `
INSERT INTO agent_studio_event (id, partner_id, agent_id, event, detail, principal_kind, principal_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?, ?)`,
	qCommentList:       commentListAll,
	qCommentListStatus: commentListStatus,
}

// StudioPublishReviews is two-person publish. The author requests a review of the exact
// release the draft would freeze; a person holding APPROVE_PUBLISH who authored none of it
// approves or rejects it. Its authors are the requester and the last editors of every resource
// it adds or changes. Composed as a publish guard and a release writer, it refuses a publish
// the tenant's Separation requires reviewing unless the agent's latest review is an approved,
// unconsumed review of the candidate digest, its publisher is none of its authors (save the
// requester under the exception they named, while Separation still admits it), and no draft
// comment is open. The release writer re-checks under a row lock and consumes the review.
// Restores where Separation requires it take a restore review of the exact version over the
// release active when it was requested; its only author is its requester.
type StudioPublishReviews struct {
	Studio     *StudioService
	Separation contract.PublishSeparation
	// Evaluator is optional; it supplies the evaluation results a review records.
	Evaluator contract.PublishReviewEvaluator
	// Releases reads the active release's settings for the diff; nil uses scope.TableEffectiveReleaseStore.
	Releases contract.EffectiveReleaseRepository
	Now      func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

var (
	_ contract.StudioPublishReviewBackend = (*StudioPublishReviews)(nil)
	_ contract.PublishGuard               = (*StudioPublishReviews)(nil)
	_ ReleaseWriter                       = (*StudioPublishReviews)(nil)
)

func (r *StudioPublishReviews) init(ctx context.Context) error {
	if r.Studio == nil || r.Separation == nil {
		return fmt.Errorf("%w: publish reviews need the studio service and a publish separation", domain.ErrNotReady)
	}
	if err := r.Studio.init(ctx); err != nil {
		return err
	}
	r.once.Do(func() { r.qs = r.Studio.DB.GetQueryService(ctx, reviewQueries) })
	return nil
}

func (r *StudioPublishReviews) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return r.Studio.now()
}

// Request records a review of the agent's current draft, or of restoring RestoreVersion,
// superseding any review of the same kind still open.
func (r *StudioPublishReviews) Request(ctx context.Context, actor domain.StudioActor, request domain.PublishReviewRequest) (domain.PublishReview, error) {
	if err := r.init(ctx); err != nil {
		return domain.PublishReview{}, err
	}
	kind, action := domain.PublishReviewOfPublish, domain.StudioEdit
	if request.RestoreVersion != "" {
		kind, action = domain.PublishReviewOfRestore, domain.StudioRestore
	}
	if _, err := r.Studio.editable(ctx, actor, action, request.AgentID); err != nil {
		return domain.PublishReview{}, err
	}
	requester := actorUserID(actor)
	if requester == nil {
		return domain.PublishReview{}, fmt.Errorf("%w: a person requests a publish review", domain.ErrForbidden)
	}
	note, exception := strings.TrimSpace(request.Note), strings.TrimSpace(request.SodExceptionID)
	if len(note) > maxReviewNote {
		return domain.PublishReview{}, validationError("note", "must not exceed 400 characters")
	}
	if len(exception) > maxReviewException {
		return domain.PublishReview{}, validationError("sod_exception_id", "must not exceed 200 characters")
	}
	if kind == domain.PublishReviewOfRestore {
		if err := validateRestoreVersion(request.RestoreVersion); err != nil {
			return domain.PublishReview{}, err
		}
	}
	if exception != "" {
		if err := r.Separation.Excuse(ctx, actor.TenantID, request.AgentID, kind, exception, actor.Principal, r.now()); err != nil {
			return domain.PublishReview{}, err
		}
	}
	var review domain.PublishReview
	var candidate domain.PublishCandidate
	if kind == domain.PublishReviewOfRestore {
		var err error
		if review, candidate, err = r.restoreCandidate(ctx, actor, request.AgentID, request.RestoreVersion); err != nil {
			return domain.PublishReview{}, err
		}
	} else {
		draft, draftCandidate, err := r.Studio.publishCandidate(ctx, actor, request.AgentID)
		if err != nil {
			return domain.PublishReview{}, err
		}
		diff, err := r.diff(ctx, actor.TenantID, draft, draftCandidate)
		if err != nil {
			return domain.PublishReview{}, err
		}
		candidate = draftCandidate
		review = domain.PublishReview{
			AgentID: request.AgentID, DefinitionDigest: candidate.Definition.DefinitionDigest,
			DraftRevision: draft.ExpectedDraftRevision, PromptProfileRevision: draft.ExpectedPromptProfileRevision,
			BaseVersion: diff.ActiveVersion, Diff: diff,
		}
	}
	evaluations, err := r.evaluations(ctx, candidate)
	if err != nil {
		return domain.PublishReview{}, err
	}
	encodedDiff, err := json.Marshal(review.Diff)
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("encode review diff: %w", err)
	}
	var encodedEvaluations any
	if len(evaluations) > 0 {
		raw, marshalErr := json.Marshal(evaluations)
		if marshalErr != nil {
			return domain.PublishReview{}, fmt.Errorf("encode review evaluations: %w", marshalErr)
		}
		encodedEvaluations = string(raw)
	}
	review.Evaluations, review.Status = evaluations, domain.PublishReviewRequested
	review.RequestedBy, review.RequestNote, review.SodExceptionID = *requester, note, exception

	tx, err := r.Studio.DB.BeginTx(ctx, reviewQueries)
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("begin review transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	lock, err := tx.Query(ctx, qReviewLockDraft, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("lock reviewed draft: %w", err)
	}
	if len(lock.Rows) == 0 {
		return domain.PublishReview{}, domain.ErrNotFound
	}
	// A restore reinstates a published version, so only a publish review is bound to the draft revision.
	supersede, event, detail := qReviewSupersede, "PUBLISH_REVIEW_REQUEST", "of definition "+review.DefinitionDigest
	if kind == domain.PublishReviewOfRestore {
		supersede, event, detail = qReviewSupersedeRestore, "RESTORE_REVIEW_REQUEST", "restoring version "+review.RestoreVersion
		if review.BaseVersion != "" {
			detail += " over " + review.BaseVersion
		}
	} else if common.AsInt64(lock.Rows[0][0]) != review.DraftRevision {
		return domain.PublishReview{}, domain.ErrRevisionConflict
	}
	if _, err = tx.Query(ctx, supersede, actor.TenantID, request.AgentID); err != nil {
		return domain.PublishReview{}, fmt.Errorf("supersede open reviews: %w", err)
	}
	inserted, err := tx.Query(ctx, qReviewInsert, actor.TenantID, request.AgentID, review.DefinitionDigest, review.DraftRevision,
		review.PromptProfileRevision, nullableString(review.BaseVersion), nullableString(review.RestoreVersion), string(encodedDiff),
		encodedEvaluations, *requester, nullableString(note), nullableString(exception))
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("insert publish review: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return domain.PublishReview{}, fmt.Errorf("insert publish review: no row returned")
	}
	review.ReviewID = common.AsInt64(inserted.Rows[0][0])
	review.RequestedAt = common.AsTime(inserted.Rows[0][1])
	detail = fmt.Sprintf("review %d %s", review.ReviewID, detail)
	if exception != "" {
		detail += " naming separation exception " + exception
	}
	if _, err = tx.Query(ctx, qReviewAudit, actor.TenantID, request.AgentID, event, detail,
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return domain.PublishReview{}, fmt.Errorf("record studio audit: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PublishReview{}, fmt.Errorf("commit publish review: %w", err)
	}
	committed = true
	return review, nil
}

// Review returns the agent's latest review of kind (publish when empty): a publish review
// against its current draft with the open comments, or a restore review against the active release.
func (r *StudioPublishReviews) Review(ctx context.Context, actor domain.StudioActor, agentID string, kind domain.PublishReviewKind) (domain.PublishReviewState, error) {
	if kind == "" {
		kind = domain.PublishReviewOfPublish
	}
	if kind != domain.PublishReviewOfPublish && kind != domain.PublishReviewOfRestore {
		return domain.PublishReviewState{}, validationError("kind", "must be publish or restore")
	}
	if err := r.init(ctx); err != nil {
		return domain.PublishReviewState{}, err
	}
	if err := r.Studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.PublishReviewState{}, err
	}
	required, err := r.Separation.Required(ctx, actor.TenantID, agentID, kind, r.now())
	if err != nil {
		return domain.PublishReviewState{}, fmt.Errorf("resolve publish separation: %w", err)
	}
	state := domain.PublishReviewState{AgentID: agentID, Kind: kind, Required: required}
	if kind == domain.PublishReviewOfRestore {
		return r.restoreState(ctx, actor.TenantID, state)
	}
	_, candidate, err := r.Studio.publishCandidate(ctx, actor, agentID)
	switch {
	case err == nil:
		state.CurrentDigest = candidate.Definition.DefinitionDigest
	case !candidateProblem(err):
		return domain.PublishReviewState{}, err
	}
	latest, err := r.qs.Query(ctx, qReviewLatest, actor.TenantID, agentID)
	if err != nil {
		return domain.PublishReviewState{}, fmt.Errorf("load publish review: %w", err)
	}
	if len(latest.Rows) > 0 {
		review, scanErr := scanReview(agentID, latest.Rows[0])
		if scanErr != nil {
			return domain.PublishReviewState{}, scanErr
		}
		state.Review = &review
		state.Stale = review.DefinitionDigest != state.CurrentDigest
	}
	state.OpenComments, err = listComments(ctx, r.qs, actor.TenantID, agentID, domain.DraftCommentOpen, r.Studio.commentLimit())
	if err != nil {
		return domain.PublishReviewState{}, err
	}
	return state, nil
}

// Approve approves a requested review while the draft, or for a restore the version and the
// active release, still match it. None of its authors may.
func (r *StudioPublishReviews) Approve(ctx context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error) {
	review, err := r.decidable(ctx, actor, decision)
	if err != nil {
		return domain.PublishReview{}, err
	}
	if review.RestoreVersion != "" {
		if err = r.restoreUnchanged(ctx, r.qs, actor.TenantID, review); err != nil {
			return domain.PublishReview{}, err
		}
	} else if err = r.draftUnchanged(ctx, actor, review); err != nil {
		return domain.PublishReview{}, err
	}
	if approver := *actorUserID(actor); reviewAuthor(review, approver) {
		return domain.PublishReview{}, fmt.Errorf("%w: user %d authored review %d and cannot approve it", domain.ErrForbidden, approver, review.ReviewID)
	}
	return r.decide(ctx, actor, review, domain.PublishReviewApproved, strings.TrimSpace(decision.Note))
}

func (r *StudioPublishReviews) draftUnchanged(ctx context.Context, actor domain.StudioActor, review domain.PublishReview) error {
	_, candidate, err := r.Studio.publishCandidate(ctx, actor, review.AgentID)
	if candidateProblem(err) {
		return fmt.Errorf("%w: the draft of review %d no longer compiles: %v", domain.ErrConflict, review.ReviewID, err)
	}
	if err != nil {
		return err
	}
	if candidate.Definition.DefinitionDigest != review.DefinitionDigest {
		return fmt.Errorf("%w: the draft changed after review %d was requested", domain.ErrConflict, review.ReviewID)
	}
	return nil
}

// Reject closes a requested review without approving it.
func (r *StudioPublishReviews) Reject(ctx context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error) {
	review, err := r.decidable(ctx, actor, decision)
	if err != nil {
		return domain.PublishReview{}, err
	}
	return r.decide(ctx, actor, review, domain.PublishReviewRejected, strings.TrimSpace(decision.Note))
}

func (r *StudioPublishReviews) decidable(ctx context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error) {
	if err := r.init(ctx); err != nil {
		return domain.PublishReview{}, err
	}
	if _, err := r.Studio.editable(ctx, actor, domain.StudioApprovePublish, decision.AgentID); err != nil {
		return domain.PublishReview{}, err
	}
	if actorUserID(actor) == nil {
		return domain.PublishReview{}, fmt.Errorf("%w: a person decides a publish review", domain.ErrForbidden)
	}
	var fields []domain.AgentFieldError
	if decision.ReviewID <= 0 {
		fields = append(fields, domain.AgentFieldError{Field: "review_id", Message: "required"})
	}
	if len(strings.TrimSpace(decision.Note)) > maxReviewNote {
		fields = append(fields, domain.AgentFieldError{Field: "note", Message: "must not exceed 400 characters"})
	}
	if len(fields) > 0 {
		return domain.PublishReview{}, &StudioValidationError{Fields: fields}
	}
	res, err := r.qs.Query(ctx, qReviewGet, actor.TenantID, decision.AgentID, decision.ReviewID)
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("load publish review: %w", err)
	}
	if len(res.Rows) == 0 {
		return domain.PublishReview{}, fmt.Errorf("%w: publish review %d", domain.ErrNotFound, decision.ReviewID)
	}
	review, err := scanReview(decision.AgentID, res.Rows[0])
	if err != nil {
		return domain.PublishReview{}, err
	}
	if review.Status != domain.PublishReviewRequested {
		return domain.PublishReview{}, fmt.Errorf("%w: publish review %d is %s", domain.ErrConflict, review.ReviewID, review.Status)
	}
	return review, nil
}

func (r *StudioPublishReviews) decide(ctx context.Context, actor domain.StudioActor, review domain.PublishReview, status domain.PublishReviewStatus, note string) (domain.PublishReview, error) {
	decider := *actorUserID(actor)
	tx, err := r.Studio.DB.BeginTx(ctx, reviewQueries)
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("begin review decision: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	decided, err := tx.Query(ctx, qReviewDecide, string(status), decider, nullableString(note), actor.TenantID, review.AgentID, review.ReviewID, review.DefinitionDigest)
	if err != nil {
		return domain.PublishReview{}, fmt.Errorf("decide publish review: %w", err)
	}
	if len(decided.Rows) == 0 {
		return domain.PublishReview{}, fmt.Errorf("%w: publish review %d was decided or superseded concurrently", domain.ErrConflict, review.ReviewID)
	}
	prefix, detail := "PUBLISH_REVIEW_", fmt.Sprintf("review %d", review.ReviewID)
	if review.RestoreVersion != "" {
		prefix = "RESTORE_REVIEW_"
	}
	event := prefix + "REJECT"
	if status == domain.PublishReviewApproved {
		event = prefix + "APPROVE"
	}
	if _, err = tx.Query(ctx, qReviewAudit, actor.TenantID, review.AgentID, event, detail, string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return domain.PublishReview{}, fmt.Errorf("record studio audit: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PublishReview{}, fmt.Errorf("commit review decision: %w", err)
	}
	committed = true
	decidedAt := common.AsTime(decided.Rows[0][0])
	review.Status, review.DecidedBy, review.DecidedAt, review.DecisionNote = status, &decider, &decidedAt, note
	return review, nil
}

// CheckPublish refuses a publish or a restore the tenant's separation requires reviewing without
// an admissible review. A scheduled activation deploys a release already admitted and passes.
func (r *StudioPublishReviews) CheckPublish(ctx context.Context, candidate domain.PublishCandidate) error {
	if candidate.Activation {
		return nil
	}
	if err := r.init(ctx); err != nil {
		return err
	}
	target, lookup := reviewTarget{kind: domain.PublishReviewOfPublish, digest: candidate.Definition.DefinitionDigest}, qReviewLatest
	if candidate.RestoredFrom != "" {
		target.kind, target.restoreVersion, lookup = domain.PublishReviewOfRestore, candidate.RestoredFrom, qReviewLatestRestore
	}
	_, err := r.admit(ctx, r.qs, lookup, candidate.Actor.TenantID, candidate.Definition.AgentID, target, candidate.Actor.Principal)
	return err
}

// WriteRelease checks the review again under a row lock in the publish or restore transaction
// and consumes it with the new version, recording an event that names any exception relied on.
func (r *StudioPublishReviews) WriteRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if r.Studio == nil || r.Separation == nil {
		return fmt.Errorf("%w: publish reviews need the studio service and a publish separation", domain.ErrNotReady)
	}
	catalog, ok := tx.(keelport.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the publish transaction cannot bind another query catalog", domain.ErrNotReady)
	}
	qs := catalog.QueryService(reviewCatalogID, reviewQueries)
	publisher := domain.PrincipalRef{Kind: domain.PrincipalService}
	if definition.PublishedBy != nil {
		publisher = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: strconv.FormatInt(*definition.PublishedBy, 10)}
	}
	target, claim, event := reviewTarget{kind: domain.PublishReviewOfPublish, digest: definition.DefinitionDigest}, qReviewClaim, "PUBLISH_REVIEWED"
	if definition.RestoredFromVersion != "" {
		target.kind, target.restoreVersion = domain.PublishReviewOfRestore, definition.RestoredFromVersion
		claim, event = qReviewClaimRestore, "RESTORE_REVIEWED"
	}
	admitted, err := r.admit(ctx, qs, claim, tenantID, definition.AgentID, target, publisher)
	if err != nil || admitted == nil {
		return err
	}
	consumed, err := qs.Query(ctx, qReviewConsume, definition.Version, tenantID, definition.AgentID, admitted.ReviewID)
	if err != nil {
		return fmt.Errorf("consume publish review: %w", err)
	}
	if len(consumed.Rows) == 0 {
		return fmt.Errorf("%w: publish review %d was consumed concurrently", domain.ErrConflict, admitted.ReviewID)
	}
	detail := "version " + definition.Version
	if target.restoreVersion != "" {
		detail += " restoring " + target.restoreVersion
	}
	detail += fmt.Sprintf(" approved in review %d", admitted.ReviewID)
	if admitted.exception != "" {
		detail += " under separation exception " + admitted.exception
	}
	var kind, id any
	if publisher.ID != "" {
		kind, id = string(publisher.Kind), publisher.ID
	}
	if _, err = qs.Query(ctx, qReviewAudit, tenantID, definition.AgentID, event, detail, kind, id); err != nil {
		return fmt.Errorf("record studio audit: %w", err)
	}
	return nil
}

// reviewTarget is what a publish or a restore asks a review to admit.
type reviewTarget struct {
	kind           domain.PublishReviewKind
	digest         string
	restoreVersion string
}

// admission is the review a publish relies on and the separation exception it relied on, if any.
type admission struct {
	domain.PublishReview
	exception string
}

// admit returns the agent's latest review of the target's kind when it is an approved,
// unconsumed review of the target, or nil when it is not and the tenant does not require one.
func (r *StudioPublishReviews) admit(ctx context.Context, qs keelport.QueryService, lookup string, tenantID int64, agentID string, target reviewTarget, publisher domain.PrincipalRef) (*admission, error) {
	now := r.now()
	required, err := r.Separation.Required(ctx, tenantID, agentID, target.kind, now)
	if err != nil {
		return nil, fmt.Errorf("resolve publish separation: %w", err)
	}
	res, err := qs.Query(ctx, lookup, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("load latest publish review: %w", err)
	}
	var problem error
	var review domain.PublishReview
	switch {
	case len(res.Rows) == 0:
		problem = fmt.Errorf("%w: agent %q needs an approved %s review of definition %s", domain.ErrConflict, agentID, target.kind, target.digest)
	default:
		if review, err = scanReview(agentID, res.Rows[0]); err != nil {
			return nil, err
		}
		problem = admissible(review, target)
	}
	if problem == nil && target.kind == domain.PublishReviewOfRestore {
		active := qReviewActive
		if lookup == qReviewClaimRestore {
			active = qReviewActiveLocked
		}
		problem = r.activeUnchanged(ctx, qs, active, tenantID, review)
	}
	if problem != nil {
		if required {
			return nil, problem
		}
		return nil, nil
	}
	admitted := &admission{PublishReview: review}
	if !required {
		return admitted, nil
	}
	if review.DecidedBy == nil || reviewAuthor(review, *review.DecidedBy) {
		return nil, fmt.Errorf("%w: review %d was approved by one of its authors", domain.ErrForbidden, review.ReviewID)
	}
	if publisherID, isPerson := personID(publisher); isPerson && reviewAuthor(review, publisherID) {
		if publisherID != review.RequestedBy || review.SodExceptionID == "" {
			return nil, fmt.Errorf("%w: user %d authored review %d and cannot carry it out", domain.ErrForbidden, publisherID, review.ReviewID)
		}
		if err = r.Separation.Excuse(ctx, tenantID, agentID, target.kind, review.SodExceptionID, publisher, now); err != nil {
			return nil, err
		}
		admitted.exception = review.SodExceptionID
	}
	// Draft comments concern the draft; a restore reinstates a published version.
	if target.kind == domain.PublishReviewOfRestore {
		return admitted, nil
	}
	open, err := qs.Query(ctx, qReviewOpenComments, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("count open draft comments: %w", err)
	}
	if len(open.Rows) > 0 && common.AsInt64(open.Rows[0][0]) > 0 {
		return nil, fmt.Errorf("%w: agent %q has %d open draft comments", domain.ErrConflict, agentID, common.AsInt64(open.Rows[0][0]))
	}
	return admitted, nil
}

// admissible explains why the agent's latest review does not admit the target, or returns nil.
func admissible(review domain.PublishReview, target reviewTarget) error {
	switch {
	case review.Status != domain.PublishReviewApproved:
		return fmt.Errorf("%w: the latest %s review %d is %s", domain.ErrConflict, target.kind, review.ReviewID, review.Status)
	case review.PublishedVersion != "":
		return fmt.Errorf("%w: review %d was consumed by version %s", domain.ErrConflict, review.ReviewID, review.PublishedVersion)
	case review.RestoreVersion != target.restoreVersion:
		return fmt.Errorf("%w: review %d approved restoring version %s, not %s", domain.ErrConflict, review.ReviewID, review.RestoreVersion, target.restoreVersion)
	case review.DefinitionDigest != target.digest:
		return fmt.Errorf("%w: review %d approved definition %s, not %s", domain.ErrConflict, review.ReviewID, review.DefinitionDigest, target.digest)
	}
	return nil
}

// reviewAuthor reports whether the user requested the review or last edited a resource it adds or changes.
func reviewAuthor(review domain.PublishReview, user int64) bool {
	return user == review.RequestedBy || slices.Contains(review.Diff.Editors, user)
}

func personID(principal domain.PrincipalRef) (int64, bool) {
	if principal.Kind != domain.PrincipalHuman {
		return 0, false
	}
	id, err := strconv.ParseInt(principal.ID, 10, 64)
	return id, err == nil && id > 0
}

func (r *StudioPublishReviews) evaluations(ctx context.Context, candidate domain.PublishCandidate) ([]domain.PublishReviewEvaluation, error) {
	if r.Evaluator == nil {
		return nil, nil
	}
	evaluations, err := r.Evaluator.Evaluations(ctx, candidate)
	if err != nil {
		return nil, fmt.Errorf("evaluate reviewed draft: %w", err)
	}
	if len(evaluations) > maxReviewEvaluation {
		return nil, fmt.Errorf("%w: at most %d evaluations accompany a review", domain.ErrValidation, maxReviewEvaluation)
	}
	for _, evaluation := range evaluations {
		if strings.TrimSpace(evaluation.Name) == "" || len(evaluation.Name) > 200 || len(evaluation.Summary) > 2000 || len(evaluation.Reference) > 400 {
			return nil, fmt.Errorf("%w: an evaluation has a name of at most 200 characters, a summary of at most 2000, and a reference of at most 400", domain.ErrValidation)
		}
	}
	return evaluations, nil
}

// diff compares the candidate with the release active now: drift causes, the compiled prompt
// sections, and the effective settings other than prompt sections. Its editors are the last
// editors of changed settings and everyone who changed the draft since the last publish.
func (r *StudioPublishReviews) diff(ctx context.Context, tenantID int64, draft domain.AgentDraft, candidate domain.PublishCandidate) (domain.PublishReviewDiff, error) {
	var diff domain.PublishReviewDiff
	if draft.Drift != nil {
		diff.Causes, diff.ChangedLanguages = draft.Drift.Causes, draft.Drift.ChangedLanguages
	}
	var active domain.AgentDefinition
	var activeSettings []domain.EffectiveResource
	var err error
	if diff.ActiveVersion, active, activeSettings, err = r.activeRelease(ctx, tenantID, draft.AgentID); err != nil {
		return diff, err
	}
	diff.PromptSections = promptSectionChanges(active.Languages, candidate.Definition.Languages)
	diff.Settings = settingChanges(activeSettings, candidate.Settings.Resources)
	diff.Editors = changedEditors(activeSettings, candidate.Settings.Resources)
	drafted, err := r.qs.Query(ctx, qReviewDraftEditors, tenantID, draft.AgentID)
	if err != nil {
		return diff, fmt.Errorf("load draft editors: %w", err)
	}
	for _, row := range drafted.Rows {
		editor, err := strconv.ParseInt(common.AsString(row[0]), 10, 64)
		if err != nil {
			return diff, fmt.Errorf("draft editor %q is not a user id: %w", common.AsString(row[0]), err)
		}
		if !slices.Contains(diff.Editors, editor) {
			diff.Editors = append(diff.Editors, editor)
		}
	}
	slices.Sort(diff.Editors)
	return diff, nil
}

// activeRelease is the agent's stable release with its frozen settings; the version is empty
// before the first publish, and a release frozen before settings were stored has none.
func (r *StudioPublishReviews) activeRelease(ctx context.Context, tenantID int64, agentID string) (string, domain.AgentDefinition, []domain.EffectiveResource, error) {
	res, err := r.qs.Query(ctx, qReviewActive, tenantID, agentID)
	if err != nil {
		return "", domain.AgentDefinition{}, nil, fmt.Errorf("load active definition: %w", err)
	}
	if len(res.Rows) == 0 {
		return "", domain.AgentDefinition{}, nil, nil
	}
	version := common.AsString(res.Rows[0][0])
	active, err := decodeDefinition(res.Rows[0][1])
	if err != nil {
		return "", domain.AgentDefinition{}, nil, err
	}
	release, err := r.releases().Get(ctx, tenantID, agentID, version)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return "", domain.AgentDefinition{}, nil, fmt.Errorf("load active settings: %w", err)
	}
	return version, active, release.Resources, nil
}

func (r *StudioPublishReviews) releases() contract.EffectiveReleaseRepository {
	if r.Releases != nil {
		return r.Releases
	}
	return &scope.TableEffectiveReleaseStore{DB: r.Studio.DB}
}

func promptSectionChanges(before, after []domain.CompiledPrompt) []domain.PromptSectionChange {
	type key struct {
		language string
		section  int64
	}
	index := func(languages []domain.CompiledPrompt) map[key]domain.CompiledPromptSection {
		sections := map[key]domain.CompiledPromptSection{}
		for _, language := range languages {
			for _, section := range language.Sections {
				sections[key{language.LanguageCode, section.PromptSectionID}] = section
			}
		}
		return sections
	}
	old, current := index(before), index(after)
	var changes []domain.PromptSectionChange
	for k, section := range current {
		previous, existed := old[k]
		switch {
		case !existed:
			changes = append(changes, domain.PromptSectionChange{LanguageCode: k.language, PromptSectionID: k.section, Caption: section.Caption, Change: domain.DiffAdded})
		case previous.Sequence != section.Sequence || previous.Caption != section.Caption || previous.Description != section.Description ||
			previous.Instruction != section.Instruction || previous.Output != section.Output:
			changes = append(changes, domain.PromptSectionChange{LanguageCode: k.language, PromptSectionID: k.section, Caption: section.Caption, Change: domain.DiffChanged})
		}
	}
	for k, section := range old {
		if _, kept := current[k]; !kept {
			changes = append(changes, domain.PromptSectionChange{LanguageCode: k.language, PromptSectionID: k.section, Caption: section.Caption, Change: domain.DiffRemoved})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].LanguageCode != changes[j].LanguageCode {
			return changes[i].LanguageCode < changes[j].LanguageCode
		}
		return changes[i].PromptSectionID < changes[j].PromptSectionID
	})
	return changes
}

// changedEditors is the last editor of every resource, prompt sections included, whose value
// after differs from before or that before lacks.
func changedEditors(before, after []domain.EffectiveResource) []int64 {
	old := map[string][]byte{}
	for _, resource := range before {
		old[string(resource.ResourceKind)+"\x00"+resource.ResourceID] = resource.Value
	}
	var editors []int64
	for _, resource := range after {
		previous, existed := old[string(resource.ResourceKind)+"\x00"+resource.ResourceID]
		if editor := resource.Source.Approver; editor != nil && (!existed || !bytes.Equal(previous, resource.Value)) && !slices.Contains(editors, *editor) {
			editors = append(editors, *editor)
		}
	}
	slices.Sort(editors)
	return editors
}

func settingChanges(before, after []domain.EffectiveResource) []domain.SettingChange {
	key := func(resource domain.EffectiveResource) string {
		return string(resource.ResourceKind) + "\x00" + resource.ResourceID
	}
	old := map[string]domain.EffectiveResource{}
	for _, resource := range before {
		if resource.ResourceKind != domain.ResourcePromptSection {
			old[key(resource)] = resource
		}
	}
	var changes []domain.SettingChange
	for _, resource := range after {
		if resource.ResourceKind == domain.ResourcePromptSection {
			continue
		}
		previous, existed := old[key(resource)]
		delete(old, key(resource))
		switch {
		case !existed:
			changes = append(changes, domain.SettingChange{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
				Change: domain.DiffAdded, After: json.RawMessage(resource.Value)})
		case !bytes.Equal(previous.Value, resource.Value):
			changes = append(changes, domain.SettingChange{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
				Change: domain.DiffChanged, Before: json.RawMessage(previous.Value), After: json.RawMessage(resource.Value)})
		}
	}
	for _, resource := range old {
		changes = append(changes, domain.SettingChange{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
			Change: domain.DiffRemoved, Before: json.RawMessage(resource.Value)})
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].ResourceKind != changes[j].ResourceKind {
			return changes[i].ResourceKind < changes[j].ResourceKind
		}
		return changes[i].ResourceID < changes[j].ResourceID
	})
	return changes
}

// publishCandidate is the release Publish would freeze from the agent's current draft, written nowhere.
func (s *StudioService) publishCandidate(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentDraft, domain.PublishCandidate, error) {
	draft, err := s.draft(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentDraft{}, domain.PublishCandidate{}, err
	}
	full := draft
	full.Enabled = true
	if err = s.validateDraft(ctx, actor.TenantID, full, true); err != nil {
		return domain.AgentDraft{}, domain.PublishCandidate{}, err
	}
	definition, err := s.definition(ctx, actor.TenantID, draft)
	if err != nil {
		return domain.AgentDraft{}, domain.PublishCandidate{}, err
	}
	settings, err := s.freeze(ctx, actor.TenantID, draft, &definition, actorUserID(actor))
	if err != nil {
		return domain.AgentDraft{}, domain.PublishCandidate{}, err
	}
	return draft, domain.PublishCandidate{Actor: actor, Definition: definition, Settings: settings}, nil
}

// candidateProblem reports a draft that does not compile into a release, as opposed to a failure to read it.
func candidateProblem(err error) bool {
	return err != nil && (settingsProblem(err) || errors.Is(err, domain.ErrNoPrompts))
}

func scanReview(agentID string, row []any) (domain.PublishReview, error) {
	review := domain.PublishReview{
		ReviewID: common.AsInt64(row[0]), AgentID: agentID, DefinitionDigest: common.AsString(row[1]),
		DraftRevision: common.AsInt64(row[2]), PromptProfileRevision: common.AsInt64(row[3]), BaseVersion: common.AsString(row[4]),
		Status: domain.PublishReviewStatus(common.AsString(row[7])), RequestedBy: common.AsInt64(row[8]),
		RequestNote: common.AsString(row[9]), RequestedAt: common.AsTime(row[10]), DecisionNote: common.AsString(row[12]),
		SodExceptionID: common.AsString(row[14]), PublishedVersion: common.AsString(row[15]), RestoreVersion: common.AsString(row[16]),
	}
	if err := json.Unmarshal([]byte(common.AsString(row[5])), &review.Diff); err != nil {
		return domain.PublishReview{}, fmt.Errorf("decode publish review %d diff: %w", review.ReviewID, err)
	}
	if raw := common.AsString(row[6]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &review.Evaluations); err != nil {
			return domain.PublishReview{}, fmt.Errorf("decode publish review %d evaluations: %w", review.ReviewID, err)
		}
	}
	if decidedBy := common.AsInt64(row[11]); decidedBy > 0 {
		review.DecidedBy = &decidedBy
	}
	if decidedAt, ok := common.AsTimeOK(row[13]); ok {
		review.DecidedAt = &decidedAt
	}
	return review, nil
}
