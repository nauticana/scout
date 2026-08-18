package domain

import (
	"encoding/json"
	"time"
)

// AgentReadiness is the operator-facing state of an agent configuration.
type AgentReadiness string

const (
	AgentReady        AgentReadiness = "ready"
	AgentDisabled     AgentReadiness = "disabled"
	AgentMissingModel AgentReadiness = "missing_model"
	AgentUnpublished  AgentReadiness = "unpublished"
	AgentError        AgentReadiness = "error"
	// Product-gate states a consumer may layer over a Ready agent: the plan
	// does not include the capability, a required integration is not
	// connected, or the calling user lacks the grant.
	AgentNotEntitled      AgentReadiness = "not_entitled"
	AgentNeedsIntegration AgentReadiness = "needs_integration"
	AgentForbidden        AgentReadiness = "forbidden"
)

// PromptSourceLevel identifies one level in the shared prompt inheritance chain.
type PromptSourceLevel int16

const (
	PromptSourceBaseline PromptSourceLevel = iota + 1
	PromptSourceTenantDefault
	PromptSourceAgentOverride
)

// ValidationPhase distinguishes editing a draft from producing executable
// state, so product validators can gate release-only requirements such as
// provider credentials without blocking an ordinary save.
type ValidationPhase string

const (
	ValidateDraft   ValidationPhase = "draft"
	ValidateRelease ValidationPhase = "release"
)

// PromptResetScope selects the editable prompt levels removed by a reset.
type PromptResetScope string

const (
	ResetAgentOverride PromptResetScope = "agent_override"
	ResetTenantDefault PromptResetScope = "type_default"
	ResetToBaseline    PromptResetScope = "platform_baseline"
)

// StudioActor identifies the tenant and actor responsible for a Studio mutation.
type StudioActor struct {
	TenantID int64
	ActorID  int64
}

// ModelReference identifies one provider model without exposing an SDK type.
type ModelReference struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// AgentModelSelection assigns models to the standard Studio capabilities.
type AgentModelSelection struct {
	Text  *ModelReference `json:"text,omitempty"`
	Image *ModelReference `json:"image,omitempty"`
	Video *ModelReference `json:"video,omitempty"`
}

// AgentApprovalPolicy is structured policy enforced outside model prompts.
type AgentApprovalPolicy struct {
	RequireApproval bool `json:"require_approval"`
}

// PromptValue is the instruction and optional output contract at one source level.
type PromptValue struct {
	Instruction string
	Output      string
}

// PromptOverride is an agent prompt value with append-or-replace behavior.
type PromptOverride struct {
	PromptValue
	Overwrite bool
}

// PromptSourceRow is one resolved compiler input with its provenance.
type PromptSourceRow struct {
	PromptSectionID int64             `json:"prompt_section_id"`
	Caption         string            `json:"caption"`
	Description     string            `json:"description"`
	DisplayOrder    int64             `json:"display_order"`
	SourceLevel     PromptSourceLevel `json:"source_level"`
	SourceKey       string            `json:"source_key"`
	Overwrite       bool              `json:"overwrite"`
	Instruction     string            `json:"instruction"`
	Output          string            `json:"output,omitempty"`
}

// ResolvedPrompts contains ordered source candidates for one agent language.
type ResolvedPrompts struct {
	AgentID      string            `json:"agent_id"`
	AgentTypeID  string            `json:"agent_type_id"`
	BaselineKey  string            `json:"baseline_key"`
	LanguageCode string            `json:"language_code"`
	Rows         []PromptSourceRow `json:"rows"`
}

// CompiledPromptSection is one frozen runtime prompt section.
type CompiledPromptSection struct {
	Sequence        int64  `json:"sequence"`
	PromptSectionID int64  `json:"prompt_section_id"`
	Caption         string `json:"caption"`
	Description     string `json:"description"`
	Instruction     string `json:"instruction"`
	Output          string `json:"output,omitempty"`
}

// CompiledPrompt is the immutable prompt snapshot for one language.
type CompiledPrompt struct {
	LanguageCode string                  `json:"language_code"`
	Sections     []CompiledPromptSection `json:"sections"`
	Digest       string                  `json:"digest"`
}

// AgentPromptSection exposes prompt provenance and effective content to Studio.
type AgentPromptSection struct {
	PromptSectionID int64
	Caption         string
	Description     string
	Baseline        PromptValue
	TenantDefault   *PromptValue
	AgentOverride   *PromptOverride
	Effective       PromptValue
}

// AgentLanguageDraft contains editable prompt sections for one language.
type AgentLanguageDraft struct {
	LanguageCode string
	Sections     []AgentPromptSection
}

// AgentDrift reports changes between an active release and current prompt sources.
type AgentDrift struct {
	ActiveVersion    string
	ChangedLanguages []string
	Causes           []string
}

// AgentDraft is the revision-checked mutable Studio representation of an agent.
type AgentDraft struct {
	AgentID                       string
	AgentTypeID                   string
	DisplayName                   string
	Active                        bool
	Enabled                       bool
	Default                       bool
	ApprovalPolicy                AgentApprovalPolicy
	Models                        AgentModelSelection
	Languages                     []AgentLanguageDraft
	Extension                     json.RawMessage
	Drift                         *AgentDrift
	ExpectedDraftRevision         int64
	ExpectedPromptProfileRevision int64
}

// AgentSummary is the compact list representation used by Studio navigation.
type AgentSummary struct {
	AgentID               string
	AgentTypeID           string
	DisplayName           string
	Purpose               string
	Active                bool
	Enabled               bool
	Default               bool
	Readiness             AgentReadiness
	ReadinessReason       string
	DraftRevision         int64
	PromptProfileRevision int64
	PublishedVersion      string
	PublishedAt           *time.Time
	LastRunAt             *time.Time
}

// AgentFieldError identifies one invalid Studio field.
type AgentFieldError struct {
	Field   string
	Message string
}

// AgentTestRequest describes one bounded test of the current saved draft.
type AgentTestRequest struct {
	AgentID      string
	LanguageCode string
	Task         string
	InputData    string
}

// AgentTestResult records test output, definition provenance, latency, and usage.
type AgentTestResult struct {
	AgentID      string
	LanguageCode string
	Model        ModelReference
	Digest       string
	Output       string
	LatencyMs    int64
	Usage        Usage
	Sections     []string
}

// AgentPublishRequest carries optimistic revisions and release notes.
type AgentPublishRequest struct {
	AgentID                       string
	ChangeSummary                 string
	ExpectedDraftRevision         int64
	ExpectedPromptProfileRevision int64
}

// AgentRestoreRequest identifies the immutable version copied into a new release.
type AgentRestoreRequest struct {
	AgentID string
	Version string
}

// AgentResetRequest identifies prompt scope and optional section/language filters.
type AgentResetRequest struct {
	AgentID                       string
	Scope                         PromptResetScope
	PromptSectionID               int64
	LanguageCode                  string
	ExpectedDraftRevision         int64
	ExpectedPromptProfileRevision int64
}

// AgentSetDefaultRequest selects the agent assigned to its logical kind alias.
type AgentSetDefaultRequest struct {
	AgentID               string
	ExpectedAliasRevision int64
}

// AgentSetEnabledRequest changes only the operational switch.
type AgentSetEnabledRequest struct {
	AgentID               string
	Enabled               bool
	ExpectedDraftRevision int64
}

// AgentEnabledState is the revision returned by a kill-switch update.
type AgentEnabledState struct {
	Enabled       bool
	DraftRevision int64
}

// AgentRelease is immutable publication metadata returned by Studio history.
type AgentRelease struct {
	AgentID               string
	AgentTypeID           string
	Version               string
	Enabled               bool
	Models                AgentModelSelection
	ApprovalPolicy        AgentApprovalPolicy
	DefinitionDigest      string
	DraftRevision         int64
	PromptProfileRevision int64
	ChangeSummary         string
	PublishedBy           *int64
	PublishedAt           time.Time
	RestoredFromVersion   string
	Active                bool
	Languages             []string
}

// AgentReleaseSection is one compiled section from an immutable release.
type AgentReleaseSection struct {
	LanguageCode string
	CompiledPromptSection
}

// AgentStudioEvent is one queryable lifecycle audit entry.
type AgentStudioEvent struct {
	Event      string
	Detail     string
	ActorID    *int64
	OccurredAt time.Time
}

// TenantIdentity is the agent-platform registration of a keel business partner.
type TenantIdentity struct {
	TenantKey  string
	HomeRegion string
}

// AgentSeed is one idempotent agent provisioning request: the identity, its
// mutable draft configuration, and the logical alias it should serve.
type AgentSeed struct {
	AgentID         string
	AgentTypeID     string
	AliasID         string
	DisplayName     string
	Enabled         bool
	RequireApproval bool
	Models          AgentModelSelection
}

// DeployedAgent is one tenant alias with its operational state and the
// immutable definition currently serving traffic. Definition is nil when the
// alias has no stable deployment.
type DeployedAgent struct {
	AliasID     string
	AgentID     string
	AgentTypeID string
	Active      bool
	Enabled     bool
	Version     string
	Definition  *AgentDefinition
}

// AgentAlias maps a tenant logical role to one named agent.
type AgentAlias struct {
	AliasID     string
	AgentTypeID string
	AgentID     string
	Revision    int64
	UpdatedBy   *int64
	UpdatedAt   time.Time
}

// PromptBaselineSelection supplies product-specific baseline precedence.
type PromptBaselineSelection struct {
	Keys []string
}

// AgentTypeDescriptor supplies product-owned labels without hard-coded catalogs.
type AgentTypeDescriptor struct {
	AgentTypeID string
	DisplayName string
	Purpose     string
}

// ModelRate is one currency-denominated price for a model usage category.
type ModelRate struct {
	UsageCategory string
	Unit          string
	AmountMinor   int64
	Currency      string
}

// StudioModel describes one selectable model and its supported capabilities.
type StudioModel struct {
	Reference         ModelReference
	DisplayName       string
	Capabilities      []string
	ContextTokenLimit int64
	OutputTokenLimit  int64
	Active            bool
	Rates             []ModelRate
}

// ModelUsage is the billable work one model performed: tokens for text, assets
// for images, and whole seconds for video.
type ModelUsage struct {
	InputTokens  int64
	OutputTokens int64
	Images       int64
	VideoSeconds int64
}
