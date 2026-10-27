package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const (
	qCommentInsert     = "scout_comment_insert"
	qCommentList       = "scout_comment_list"
	qCommentListStatus = "scout_comment_list_status"
	qCommentResolve    = "scout_comment_resolve"
	qCommentGet        = "scout_comment_get"

	maxCommentBody = 2000
)

// DefaultMaxCommentsShown bounds one list of draft comments when StudioService.MaxCommentsShown is zero.
const DefaultMaxCommentsShown = 500

// Status codes quoted for the named SQL below.
const (
	sqlCommentOpen     = "'" + string(domain.DraftCommentOpen) + "'"
	sqlCommentResolved = "'" + string(domain.DraftCommentResolved) + "'"
)

const commentColumns = `id, draft_revision, anchor_code, prompt_section_id, language_code, resource_kind_code, resource_id,
       body, status_code, author_kind, author_id, created_at, resolved_by_kind, resolved_by_id, resolved_at`

const (
	commentListAll = `
SELECT ` + commentColumns + `
  FROM agent_draft_comment
 WHERE partner_id = ? AND agent_id = ?
 ORDER BY id DESC
 LIMIT ?`
	commentListStatus = `
SELECT ` + commentColumns + `
  FROM agent_draft_comment
 WHERE partner_id = ? AND agent_id = ? AND status_code = ?
 ORDER BY id DESC
 LIMIT ?`
)

var commentQueries = map[string]string{
	qCommentInsert: `
INSERT INTO agent_draft_comment (id, partner_id, agent_id, draft_revision, anchor_code, prompt_section_id, language_code,
       resource_kind_code, resource_id, body, status_code, author_kind, author_id)
SELECT nextval('agent_draft_comment_seq'), d.partner_id, d.agent_id, d.draft_revision, ?, ?, ?, ?, ?, ?, ` + sqlCommentOpen + `, ?, ?
  FROM agent_draft d
 WHERE d.partner_id = ? AND d.agent_id = ? AND d.draft_revision = ?
RETURNING id, created_at`,
	qCommentList:       commentListAll,
	qCommentListStatus: commentListStatus,
	qCommentResolve: `
UPDATE agent_draft_comment
   SET status_code = ` + sqlCommentResolved + `, resolved_by_kind = ?, resolved_by_id = ?, resolved_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND id = ? AND status_code = ` + sqlCommentOpen + `
RETURNING ` + commentColumns,
	qCommentGet: `
SELECT status_code FROM agent_draft_comment WHERE partner_id = ? AND agent_id = ? AND id = ?`,
}

// StudioDraftComments lets the people working on a draft discuss it: a comment is made on the
// current draft revision and anchored to the draft, a prompt section, or a setting. Commenting
// and resolving need EDIT or APPROVE_PUBLISH on the agent; listing needs VIEW. Open comments
// block a publish that two-person publish governs.
type StudioDraftComments struct {
	Studio *StudioService

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioDraftCommentBackend = (*StudioDraftComments)(nil)

func (c *StudioDraftComments) init(ctx context.Context) error {
	if c.Studio == nil {
		return fmt.Errorf("%w: draft comments need the studio service", domain.ErrNotReady)
	}
	if err := c.Studio.init(ctx); err != nil {
		return err
	}
	c.once.Do(func() { c.qs = c.Studio.DB.GetQueryService(ctx, commentQueries) })
	return nil
}

// Comments lists the agent's newest comments, all or of one status.
func (c *StudioDraftComments) Comments(ctx context.Context, actor domain.StudioActor, agentID string, status domain.DraftCommentStatus) ([]domain.DraftComment, error) {
	if err := c.init(ctx); err != nil {
		return nil, err
	}
	if status != "" && status != domain.DraftCommentOpen && status != domain.DraftCommentResolved {
		return nil, validationError("status", "must be "+string(domain.DraftCommentOpen)+" or "+string(domain.DraftCommentResolved))
	}
	if err := c.Studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	return listComments(ctx, c.qs, actor.TenantID, agentID, status, c.Studio.commentLimit())
}

// Comment records an open comment on the draft revision the author saw.
func (c *StudioDraftComments) Comment(ctx context.Context, actor domain.StudioActor, comment domain.DraftComment) (domain.DraftComment, error) {
	if err := c.init(ctx); err != nil {
		return domain.DraftComment{}, err
	}
	if err := c.collaborator(ctx, actor, comment.AgentID); err != nil {
		return domain.DraftComment{}, err
	}
	comment.Body = strings.TrimSpace(comment.Body)
	comment.LanguageCode = strings.TrimSpace(comment.LanguageCode)
	comment.ResourceID = strings.TrimSpace(comment.ResourceID)
	if err := c.validate(ctx, actor.TenantID, comment); err != nil {
		return domain.DraftComment{}, err
	}
	var section, language, kind, resource any
	switch comment.Anchor {
	case domain.CommentOnPromptSection:
		section, language = comment.PromptSectionID, common.NullIfEmpty(comment.LanguageCode)
	case domain.CommentOnSetting:
		kind, resource = string(comment.ResourceKind), comment.ResourceID
	}
	inserted, err := c.qs.Query(ctx, qCommentInsert, string(comment.Anchor), section, language, kind, resource, comment.Body,
		string(actor.Principal.Kind), actor.Principal.ID, actor.TenantID, comment.AgentID, comment.DraftRevision)
	if err != nil {
		return domain.DraftComment{}, fmt.Errorf("insert draft comment: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return domain.DraftComment{}, domain.ErrRevisionConflict
	}
	comment.CommentID = common.AsInt64(inserted.Rows[0][0])
	comment.CreatedAt = common.AsTime(inserted.Rows[0][1])
	comment.Status, comment.Author, comment.ResolvedBy, comment.ResolvedAt = domain.DraftCommentOpen, actor.Principal, nil, nil
	if comment.Anchor != domain.CommentOnPromptSection {
		comment.PromptSectionID, comment.LanguageCode = 0, ""
	}
	if comment.Anchor != domain.CommentOnSetting {
		comment.ResourceKind, comment.ResourceID = "", ""
	}
	return comment, nil
}

// Resolve closes one open comment.
func (c *StudioDraftComments) Resolve(ctx context.Context, actor domain.StudioActor, agentID string, commentID int64) (domain.DraftComment, error) {
	if err := c.init(ctx); err != nil {
		return domain.DraftComment{}, err
	}
	if commentID <= 0 {
		return domain.DraftComment{}, validationError("comment_id", "required")
	}
	if err := c.collaborator(ctx, actor, agentID); err != nil {
		return domain.DraftComment{}, err
	}
	resolved, err := c.qs.Query(ctx, qCommentResolve, string(actor.Principal.Kind), actor.Principal.ID, actor.TenantID, agentID, commentID)
	if err != nil {
		return domain.DraftComment{}, fmt.Errorf("resolve draft comment: %w", err)
	}
	if len(resolved.Rows) > 0 {
		return scanComment(agentID, resolved.Rows[0]), nil
	}
	existing, err := c.qs.Query(ctx, qCommentGet, actor.TenantID, agentID, commentID)
	if err != nil {
		return domain.DraftComment{}, fmt.Errorf("load draft comment: %w", err)
	}
	if len(existing.Rows) == 0 {
		return domain.DraftComment{}, fmt.Errorf("%w: draft comment %d", domain.ErrNotFound, commentID)
	}
	return domain.DraftComment{}, fmt.Errorf("%w: draft comment %d is already resolved", domain.ErrConflict, commentID)
}

// collaborator admits an editor or a publish reviewer of a draft that is not archived.
func (c *StudioDraftComments) collaborator(ctx context.Context, actor domain.StudioActor, agentID string) error {
	_, err := c.Studio.editable(ctx, actor, domain.StudioEdit, agentID)
	if errors.Is(err, domain.ErrForbidden) {
		_, err = c.Studio.editable(ctx, actor, domain.StudioApprovePublish, agentID)
	}
	return err
}

func (c *StudioDraftComments) validate(ctx context.Context, tenantID int64, comment domain.DraftComment) error {
	var fields []domain.AgentFieldError
	if comment.DraftRevision <= 0 {
		fields = append(fields, domain.AgentFieldError{Field: "draft_revision", Message: "required"})
	}
	if length := utf8.RuneCountInString(comment.Body); length == 0 || length > maxCommentBody {
		fields = append(fields, domain.AgentFieldError{Field: "body", Message: "must contain 1 to 2000 characters"})
	}
	switch comment.Anchor {
	case domain.CommentOnDraft:
		if comment.PromptSectionID != 0 || comment.LanguageCode != "" || comment.ResourceKind != "" || comment.ResourceID != "" {
			fields = append(fields, domain.AgentFieldError{Field: "anchor", Message: "a comment on the whole draft names no section or setting"})
		}
	case domain.CommentOnPromptSection:
		if comment.ResourceKind != "" || comment.ResourceID != "" {
			fields = append(fields, domain.AgentFieldError{Field: "anchor", Message: "a prompt section comment names no setting"})
		}
		if field := c.promptSectionProblem(ctx, tenantID, comment); field != nil {
			fields = append(fields, *field)
		}
	case domain.CommentOnSetting:
		if comment.PromptSectionID != 0 || comment.LanguageCode != "" {
			fields = append(fields, domain.AgentFieldError{Field: "anchor", Message: "a setting comment names no prompt section"})
		}
		if comment.ResourceID == "" || len(comment.ResourceID) > 80 {
			fields = append(fields, domain.AgentFieldError{Field: "resource_id", Message: "must contain 1 to 80 characters"})
		}
		if !c.settingKind(ctx, comment.ResourceKind) {
			fields = append(fields, domain.AgentFieldError{Field: "resource_kind", Message: "must be a setting kind other than prompt_section"})
		}
	default:
		fields = append(fields, domain.AgentFieldError{Field: "anchor", Message: "must be " + string(domain.CommentOnDraft) + ", " + string(domain.CommentOnPromptSection) + ", or " + string(domain.CommentOnSetting)})
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

// promptSectionProblem checks that the anchored section, and language when named, are in the draft.
func (c *StudioDraftComments) promptSectionProblem(ctx context.Context, tenantID int64, comment domain.DraftComment) *domain.AgentFieldError {
	if comment.PromptSectionID <= 0 {
		return &domain.AgentFieldError{Field: "prompt_section_id", Message: "required"}
	}
	draft, err := c.Studio.draft(ctx, tenantID, comment.AgentID)
	if err != nil {
		return &domain.AgentFieldError{Field: "prompt_section_id", Message: "the draft's prompt sections cannot be read"}
	}
	for _, language := range draft.Languages {
		if comment.LanguageCode != "" && language.LanguageCode != comment.LanguageCode {
			continue
		}
		for _, section := range language.Sections {
			if section.PromptSectionID == comment.PromptSectionID {
				return nil
			}
		}
	}
	return &domain.AgentFieldError{Field: "prompt_section_id", Message: "is not a section of the draft in that language"}
}

func (c *StudioDraftComments) settingKind(ctx context.Context, kind domain.ResourceKind) bool {
	if kind == "" || kind == domain.ResourcePromptSection {
		return false
	}
	mergers := c.Studio.Mergers
	if mergers == nil {
		registry, err := scope.NewMergerRegistry()
		if err != nil {
			return false
		}
		mergers = registry
	}
	merger, err := mergers.MergerFor(ctx, kind)
	return err == nil && merger != nil
}

// listComments returns the agent's newest limit comments, all or of one status.
func listComments(ctx context.Context, qs keelport.QueryService, tenantID int64, agentID string, status domain.DraftCommentStatus, limit int) ([]domain.DraftComment, error) {
	name, args := qCommentList, []any{tenantID, agentID}
	if status != "" {
		name, args = qCommentListStatus, append(args, string(status))
	}
	args = append(args, limit)
	res, err := qs.Query(ctx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("list draft comments: %w", err)
	}
	comments := make([]domain.DraftComment, 0, len(res.Rows))
	for _, row := range res.Rows {
		comments = append(comments, scanComment(agentID, row))
	}
	return comments, nil
}

func scanComment(agentID string, row []any) domain.DraftComment {
	comment := domain.DraftComment{
		CommentID: common.AsInt64(row[0]), AgentID: agentID, DraftRevision: common.AsInt64(row[1]),
		Anchor: domain.DraftCommentAnchor(common.AsString(row[2])), PromptSectionID: common.AsInt64(row[3]),
		LanguageCode: common.AsString(row[4]), ResourceKind: domain.ResourceKind(common.AsString(row[5])),
		ResourceID: common.AsString(row[6]), Body: common.AsString(row[7]), Status: domain.DraftCommentStatus(common.AsString(row[8])),
		Author:    domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[9])), ID: common.AsString(row[10])},
		CreatedAt: common.AsTime(row[11]),
	}
	if row[12] != nil {
		comment.ResolvedBy = &domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[12])), ID: common.AsString(row[13])}
	}
	if resolvedAt, ok := common.AsTimeOK(row[14]); ok {
		comment.ResolvedAt = &resolvedAt
	}
	return comment
}
