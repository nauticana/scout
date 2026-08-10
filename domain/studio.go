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
)

// PromptSourceLevel identifies one level in the shared prompt inheritance chain.
type PromptSourceLevel int16

const (
	PromptSourceBaseline PromptSourceLevel = iota + 1
	PromptSourceTenantDefault
	PromptSourceAgentOverride
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
	ProviderID string
	ModelID    string
}

// AgentModelSelection assigns models to the standard Studio capabilities.
type AgentModelSelection struct {
	Text  *ModelReference
	Image *ModelReference
	Video *ModelReference
}

// AgentApprovalPolicy is structured policy enforced outside model prompts.
type AgentApprovalPolicy struct {
	RequireApproval bool
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
	PromptSectionID int64
	Caption         string
	Description     string
	DisplayOrder    int64
	SourceLevel     PromptSourceLevel
	SourceKey       string
	Overwrite       bool
	Instruction     string
	Output          string
}

// ResolvedPrompts contains ordered source candidates for one agent language.
type ResolvedPrompts struct {
	AgentID      string
	AgentKind    string
	BaselineKey  string
	LanguageCode string
	Rows         []PromptSourceRow
}

// CompiledPromptSection is one frozen runtime prompt section.
type CompiledPromptSection struct {
	Sequence        int64
	PromptSectionID int64
	Caption         string
	Description     string
	Instruction     string
	Output          string
}

// CompiledPrompt is the immutable prompt snapshot for one language.
type CompiledPrompt struct {
	LanguageCode string
	Sections     []CompiledPromptSection
	Digest       string
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
	AgentKind                     string
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
	AgentKind             string
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

// AgentRelease is immutable publication metadata returned by Studio history.
type AgentRelease struct {
	AgentID               string
	AgentKind             string
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

// AgentAlias maps a tenant logical role to one named agent.
type AgentAlias struct {
	AliasID   string
	AgentKind string
	AgentID   string
	Revision  int64
	UpdatedBy *int64
	UpdatedAt time.Time
}

// PromptBaselineSelection supplies product-specific baseline precedence.
type PromptBaselineSelection struct {
	Keys []string
}

// AgentKindDescriptor supplies product-owned labels without hard-coded catalogs.
type AgentKindDescriptor struct {
	AgentKind   string
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
