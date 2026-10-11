package domain

import (
	"encoding/json"
	"time"
)

// StudioApprovePublish lets a person approve or reject a publish review another person requested.
const StudioApprovePublish StudioAction = "APPROVE_PUBLISH"

// PublishReviewStatus is a publish review's state; a newer request supersedes an open one.
type PublishReviewStatus string

const (
	PublishReviewRequested  PublishReviewStatus = "requested"
	PublishReviewApproved   PublishReviewStatus = "approved"
	PublishReviewRejected   PublishReviewStatus = "rejected"
	PublishReviewSuperseded PublishReviewStatus = "superseded"
)

// DiffChange says how one part of a draft differs from the active release.
type DiffChange string

const (
	DiffAdded   DiffChange = "added"
	DiffRemoved DiffChange = "removed"
	DiffChanged DiffChange = "changed"
)

// PublishReview asks a second person to approve publishing one exact draft, identified by the
// definition digest publication would freeze: a draft that changes afterwards no longer matches
// it. Publishing consumes an approved review, recording the version in PublishedVersion.
// SodExceptionID names the separation-of-duties exception under which the requester may publish
// the approved review themselves.
type PublishReview struct {
	ReviewID              int64
	AgentID               string
	DefinitionDigest      string
	DraftRevision         int64
	PromptProfileRevision int64
	BaseVersion           string
	Diff                  PublishReviewDiff
	Evaluations           []PublishReviewEvaluation
	Status                PublishReviewStatus
	RequestedBy           int64
	RequestNote           string
	RequestedAt           time.Time
	DecidedBy             *int64
	DecisionNote          string
	DecidedAt             *time.Time
	SodExceptionID        string
	PublishedVersion      string
}

// PublishReviewDiff is what the reviewed draft changes against the release active when the
// review was requested; ActiveVersion is empty for an agent's first release. Editors are the
// users who last edited a resource it adds or changes; with the requester they are its authors.
type PublishReviewDiff struct {
	ActiveVersion    string                `json:"active_version,omitempty"`
	Editors          []int64               `json:"editors,omitempty"`
	Causes           []string              `json:"causes,omitempty"`
	ChangedLanguages []string              `json:"changed_languages,omitempty"`
	PromptSections   []PromptSectionChange `json:"prompt_sections,omitempty"`
	Settings         []SettingChange       `json:"settings,omitempty"`
}

// PromptSectionChange is one compiled prompt section that differs from the active release.
type PromptSectionChange struct {
	LanguageCode    string     `json:"language_code"`
	PromptSectionID int64      `json:"prompt_section_id"`
	Caption         string     `json:"caption"`
	Change          DiffChange `json:"change"`
}

// SettingChange is one effective setting that differs from the active release; Before and
// After are canonical JSON values.
type SettingChange struct {
	ResourceKind ResourceKind    `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Change       DiffChange      `json:"change"`
	Before       json.RawMessage `json:"before,omitempty"`
	After        json.RawMessage `json:"after,omitempty"`
}

// PublishReviewEvaluation is one evaluation result a product supplies for the reviewed draft.
type PublishReviewEvaluation struct {
	Name      string `json:"name"`
	Passed    bool   `json:"passed"`
	Summary   string `json:"summary,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// PublishReviewRequest asks for a review of the agent's current draft. SodExceptionID optionally
// names the separation-of-duties exception under which the requester will publish it.
type PublishReviewRequest struct {
	AgentID        string
	Note           string
	SodExceptionID string
}

// PublishReviewDecision approves or rejects one requested review.
type PublishReviewDecision struct {
	AgentID  string
	ReviewID int64
	Note     string
}

// PublishReviewState is an agent's latest review against its current draft. Required reports
// whether two-person publish applies now; Stale that the draft no longer matches the review;
// CurrentDigest is empty when the draft does not compile.
type PublishReviewState struct {
	AgentID       string
	Required      bool
	CurrentDigest string
	Review        *PublishReview
	Stale         bool
	OpenComments  []DraftComment
}
