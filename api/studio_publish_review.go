package api

import (
	"encoding/json"
	"time"
)

const (
	// StudioPublishReviewPath requests a review (POST) or shows the latest one (GET
	// ?agent_name=&kind=publish|restore, publish when kind is absent).
	StudioPublishReviewPath        = StudioBasePath + "publish-review"
	StudioPublishReviewApprovePath = StudioBasePath + "publish-review/approve"
	StudioPublishReviewRejectPath  = StudioBasePath + "publish-review/reject"
	// StudioCommentsPath lists draft comments: GET ?agent_name=&status=O|R (open, resolved).
	StudioCommentsPath       = StudioBasePath + "comments"
	StudioCommentPath        = StudioBasePath + "comment"
	StudioCommentResolvePath = StudioBasePath + "comment/resolve"
)

// PublishReviewRequest asks a second person to review agent_name's current draft, or restoring
// its published restore_version when set. sod_exception_id optionally names the
// separation-of-duties exception under which the requester will carry out the approved review themselves.
type PublishReviewRequest struct {
	AgentName      string `json:"agent_name"`
	Note           string `json:"note,omitempty"`
	SodExceptionID string `json:"sod_exception_id,omitempty"`
	RestoreVersion string `json:"restore_version,omitempty"`
}

// PublishReviewDecision approves or rejects review_id.
type PublishReviewDecision struct {
	AgentName string `json:"agent_name"`
	ReviewID  int64  `json:"review_id"`
	Note      string `json:"note,omitempty"`
}

// PublishReview is one review of an exact draft, identified by definition_digest, or of
// restoring restore_version over base_version. requested_by and decided_by are user ids;
// published_version is the release that consumed the approval.
type PublishReview struct {
	ReviewID              int64                     `json:"review_id"`
	AgentName             string                    `json:"agent_name"`
	DefinitionDigest      string                    `json:"definition_digest"`
	DraftRevision         int64                     `json:"draft_revision"`
	PromptProfileRevision int64                     `json:"prompt_profile_revision"`
	BaseVersion           string                    `json:"base_version,omitempty"`
	Status                string                    `json:"status"`
	RequestedBy           int64                     `json:"requested_by"`
	RequestNote           string                    `json:"request_note,omitempty"`
	RequestedAt           time.Time                 `json:"requested_at"`
	DecidedBy             *int64                    `json:"decided_by,omitempty"`
	DecisionNote          string                    `json:"decision_note,omitempty"`
	DecidedAt             *time.Time                `json:"decided_at,omitempty"`
	SodExceptionID        string                    `json:"sod_exception_id,omitempty"`
	PublishedVersion      string                    `json:"published_version,omitempty"`
	RestoreVersion        string                    `json:"restore_version,omitempty"`
	Diff                  PublishReviewDiff         `json:"diff"`
	Evaluations           []PublishReviewEvaluation `json:"evaluations"`
}

// PublishReviewDiff is what the reviewed draft changes against base_version. editors are the
// user ids that last edited an added or changed resource; none of them may approve the review.
type PublishReviewDiff struct {
	Editors          []int64               `json:"editors"`
	Causes           []string              `json:"causes"`
	ChangedLanguages []string              `json:"changed_languages"`
	PromptSections   []PromptSectionChange `json:"prompt_sections"`
	Settings         []SettingChange       `json:"settings"`
}

// PromptSectionChange is one compiled prompt section added, removed, or changed.
type PromptSectionChange struct {
	LanguageCode    string `json:"language_code"`
	PromptSectionID int64  `json:"prompt_section_id"`
	Caption         string `json:"caption"`
	Change          string `json:"change"`
}

// SettingChange is one effective setting added, removed, or changed, with canonical JSON values.
type SettingChange struct {
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Change       string          `json:"change"`
	Before       json.RawMessage `json:"before,omitempty"`
	After        json.RawMessage `json:"after,omitempty"`
}

// PublishReviewEvaluation is one evaluation result recorded with the review.
type PublishReviewEvaluation struct {
	Name      string `json:"name"`
	Passed    bool   `json:"passed"`
	Summary   string `json:"summary,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// PublishReviewState is the latest review of kind. required reports whether two-person review
// applies. For a publish, stale says the draft changed since the review was requested; for a
// restore, active_version is the release active now and stale says it is not the review's base_version.
type PublishReviewState struct {
	AgentName     string         `json:"agent_name"`
	Kind          string         `json:"kind"`
	Required      bool           `json:"required"`
	ActiveVersion string         `json:"active_version,omitempty"`
	CurrentDigest string         `json:"current_digest,omitempty"`
	Stale         bool           `json:"stale"`
	Review        *PublishReview `json:"review,omitempty"`
	OpenComments  []DraftComment `json:"open_comments"`
}

// DraftComment is a remark on draft_revision anchored to the draft ("D"), a prompt section
// ("P": prompt_section_id, optional language_code), or a setting ("S":
// resource_kind and resource_id). On the way in only agent_name, draft_revision, the anchor
// fields, and body are read.
type DraftComment struct {
	CommentID       int64      `json:"comment_id,omitempty"`
	AgentName       string     `json:"agent_name"`
	DraftRevision   int64      `json:"draft_revision"`
	Anchor          string     `json:"anchor"`
	PromptSectionID int64      `json:"prompt_section_id,omitempty"`
	LanguageCode    string     `json:"language_code,omitempty"`
	ResourceKind    string     `json:"resource_kind,omitempty"`
	ResourceID      string     `json:"resource_id,omitempty"`
	Body            string     `json:"body"`
	Status          string     `json:"status,omitempty"`
	AuthorKind      string     `json:"author_kind,omitempty"`
	AuthorID        string     `json:"author_id,omitempty"`
	CreatedAt       *time.Time `json:"created_at,omitempty"`
	ResolvedByKind  string     `json:"resolved_by_kind,omitempty"`
	ResolvedByID    string     `json:"resolved_by_id,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
}

// DraftCommentResolveRequest resolves comment_id of agent_name.
type DraftCommentResolveRequest struct {
	AgentName string `json:"agent_name"`
	CommentID int64  `json:"comment_id"`
}
