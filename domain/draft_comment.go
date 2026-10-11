package domain

import "time"

// DraftCommentAnchor is what a draft comment is about.
type DraftCommentAnchor string

const (
	CommentOnDraft         DraftCommentAnchor = "D"
	CommentOnPromptSection DraftCommentAnchor = "P"
	CommentOnSetting       DraftCommentAnchor = "S"
)

// DraftCommentStatus is open until someone resolves the comment.
type DraftCommentStatus string

const (
	DraftCommentOpen     DraftCommentStatus = "O"
	DraftCommentResolved DraftCommentStatus = "R"
)

// DraftComment is one remark on a draft revision, anchored to the draft as a whole, a prompt
// section (optionally one language of it), or a setting by resource kind and id.
type DraftComment struct {
	CommentID       int64
	AgentID         string
	DraftRevision   int64
	Anchor          DraftCommentAnchor
	PromptSectionID int64
	LanguageCode    string
	ResourceKind    ResourceKind
	ResourceID      string
	Body            string
	Status          DraftCommentStatus
	Author          PrincipalRef
	CreatedAt       time.Time
	ResolvedBy      *PrincipalRef
	ResolvedAt      *time.Time
}
