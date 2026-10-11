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
	PublishReviewRequested  PublishReviewStatus = "R"
	PublishReviewApproved   PublishReviewStatus = "A"
	PublishReviewRejected   PublishReviewStatus = "X"
	PublishReviewSuperseded PublishReviewStatus = "S"
)

// PublishReviewKind says what a review admits: publishing the draft, or restoring a published version.
type PublishReviewKind string

const (
	PublishReviewOfPublish PublishReviewKind = "publish"
	PublishReviewOfRestore PublishReviewKind = "restore"
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
// the approved review themselves. A review of a restore names RestoreVersion: its digest is that
// version's, its diff is against BaseVersion, and only restoring that version over that base consumes it.
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
	RestoreVersion        string
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

// PublishReviewRequest asks for a review of the agent's current draft, or of restoring
// RestoreVersion when set. SodExceptionID optionally names the separation-of-duties exception
// under which the requester will publish or restore it.
type PublishReviewRequest struct {
	AgentID        string
	Note           string
	SodExceptionID string
	RestoreVersion string
}

// PublishReviewDecision approves or rejects one requested review.
type PublishReviewDecision struct {
	AgentID  string
	ReviewID int64
	Note     string
}

// PublishReviewState is an agent's latest review of Kind. Required reports whether two-person
// review applies now. For a publish, Stale says the draft no longer matches the review and
// CurrentDigest is empty when the draft does not compile; for a restore, ActiveVersion is the
// release active now and Stale says it is not the one the review was requested over.
type PublishReviewState struct {
	AgentID       string
	Kind          PublishReviewKind
	Required      bool
	ActiveVersion string
	CurrentDigest string
	Review        *PublishReview
	Stale         bool
	OpenComments  []DraftComment
}
