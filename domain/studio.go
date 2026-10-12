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

// ScopeKindPlatform labels the product baseline a prompt chain starts from; it is
// not a row of config_scope, because a baseline belongs to no tenant.
const ScopeKindPlatform = "platform"

// ValidationPhase distinguishes editing a draft from producing executable
// state, so product validators can gate release-only requirements such as
// provider credentials without blocking an ordinary save.
type ValidationPhase string

const (
	ValidateDraft   ValidationPhase = "draft"
	ValidateRelease ValidationPhase = "release"
	// ValidateArchive lets a product refuse to archive an agent its own records still name, such as a schedule.
	ValidateArchive ValidationPhase = "archive"
)

// PromptResetScope selects the editable prompt levels removed by a reset.
type PromptResetScope string

const (
	ResetAgentOverride PromptResetScope = "agent_override"
	ResetTenantDefault PromptResetScope = "type_default"
	ResetToBaseline    PromptResetScope = "platform_baseline"
)

// StudioAction is an AGENT_STUDIO authorization action. Studio checks it at the
// value "agent:<id>", or "agent_type:<id>" when creating an agent that has no id yet.
type StudioAction string

const (
	StudioView    StudioAction = "VIEW"
	StudioCreate  StudioAction = "CREATE"
	StudioEdit    StudioAction = "EDIT"
	StudioTest    StudioAction = "TEST"
	StudioPublish StudioAction = "PUBLISH"
	StudioRestore StudioAction = "RESTORE"
	StudioArchive StudioAction = "ARCHIVE"
)

// StudioActor identifies the tenant and the principal responsible for a Studio
// mutation: a person, or a service such as a product installer.
type StudioActor struct {
	TenantID  int64
	Principal PrincipalRef
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

// PromptValue is an instruction and its optional output contract.
type PromptValue struct {
	Instruction string
	Output      string
}

// PromptLayer is one contribution to a prompt section, widest scope first: the
// platform baseline, then the prompt_section binding of each scope on the agent's
// chain. Sealed is set by the layer's own scope; a narrower layer under it is refused.
type PromptLayer struct {
	ScopeID   string    `json:"scope_id"`
	ScopeKind string    `json:"scope_kind"`
	MergeMode MergeMode `json:"merge_mode"`
	Sealed    bool      `json:"sealed"`
	// Editable marks the two scopes Studio writes: the agent's own and its type's tenant default.
	Editable bool `json:"editable"`
	// Version and BoundBy carry the binding's provenance; both are empty on the baseline.
	Version     string `json:"version,omitempty"`
	BoundBy     *int64 `json:"bound_by,omitempty"`
	Instruction string `json:"instruction"`
	Output      string `json:"output,omitempty"`
}

// PromptSectionSource is one section's layers for one agent language.
type PromptSectionSource struct {
	PromptSectionID int64         `json:"prompt_section_id"`
	Caption         string        `json:"caption"`
	Description     string        `json:"description"`
	DisplayOrder    int64         `json:"display_order"`
	Layers          []PromptLayer `json:"layers"`
}

// PromptScopes names the two scopes of the tenant's tree that Studio edits for one agent.
type PromptScopes struct {
	AgentScopeID string
	TypeScopeID  string
}

// ResolvedPrompts contains the layered sources of one agent language.
type ResolvedPrompts struct {
	AgentID      string                `json:"agent_id"`
	AgentTypeID  string                `json:"agent_type_id"`
	BaselineKey  string                `json:"baseline_key"`
	LanguageCode string                `json:"language_code"`
	Scopes       PromptScopes          `json:"-"`
	Sections     []PromptSectionSource `json:"sections"`
}

// CompiledPromptSection is one frozen runtime prompt section.
type CompiledPromptSection struct {
	Sequence        int64  `json:"sequence"`
	PromptSectionID int64  `json:"prompt_section_id"`
	Caption         string `json:"caption"`
	Description     string `json:"description"`
	Instruction     string `json:"instruction"`
	Output          string `json:"output,omitempty"`
	// Source says which layer decided the section; it is provenance and no part of the prompt digest.
	Source Provenance `json:"source"`
}

// CompiledPrompt is the immutable prompt snapshot for one language.
type CompiledPrompt struct {
	LanguageCode string                  `json:"language_code"`
	Sections     []CompiledPromptSection `json:"sections"`
	Digest       string                  `json:"digest"`
}

// AgentPromptSection exposes a section's layers and effective content to Studio. A save
// writes the editable layers: one that is absent ends its binding.
type AgentPromptSection struct {
	PromptSectionID int64
	Caption         string
	Description     string
	Layers          []PromptLayer
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
	AgentID     string
	AgentTypeID string
	DisplayName string
	Active      bool
	Archived    bool
	Enabled     bool
	Default     bool
	Models      AgentModelSelection
	Languages   []AgentLanguageDraft
	// PromptScopes are the two scopes a layer may be written at; set on read, ignored on save.
	PromptScopes                  PromptScopes
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
	Archived              bool
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
	// Limits are the limits frozen with the tested definition; the tester bounds the run by
	// MaxDuration. Studio and the evaluation worker set them, never the caller.
	Limits ExecutionLimits
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

// AgentCreateRequest creates a disabled, unpublished agent of an existing type. Its
// settings come from CopyFrom, an agent of the same tenant and type, or from
// TemplateID, or else from the type's tenant defaults. A replay of IdempotencyKey by
// the same actor returns the agent it created.
type AgentCreateRequest struct {
	AgentTypeID    string
	DisplayName    string
	CopyFrom       string
	TemplateID     string
	IdempotencyKey string
	// TemplateVersion selects a version of TemplateID; 0 takes the newest the tenant may use.
	TemplateVersion int64
	// References map each knowledge and entitlement slot of the template to a value of the tenant.
	References map[ResourceKind]map[string]string
}

// AgentArchiveRequest archives or restores an agent behind its draft revision.
type AgentArchiveRequest struct {
	AgentID               string
	ExpectedDraftRevision int64
}

// AgentRenameRequest changes only the display name; the agent id is permanent.
type AgentRenameRequest struct {
	AgentID               string
	DisplayName           string
	ExpectedDraftRevision int64
}

// AgentPublishRequest carries optimistic revisions and release notes.
type AgentPublishRequest struct {
	AgentID                       string
	ChangeSummary                 string
	ExpectedDraftRevision         int64
	ExpectedPromptProfileRevision int64
	// CanaryPercentage between 1 and 99 gives the release that share of new conversations
	// beside the current stable release; 0 or 100 makes it stable.
	CanaryPercentage int
	// EffectiveAt in the future schedules the release to activate then; zero or past activates it now.
	EffectiveAt time.Time
}

// PublishCandidate is a release about to be written: its frozen definition, the settings
// compiled with it (empty for a restore, which copies its source's), and who publishes it.
type PublishCandidate struct {
	Actor      StudioActor
	Definition AgentDefinition
	Settings   EffectiveRelease
	// RestoredFrom is the version a restore copies; empty for a publish.
	RestoredFrom string
	// Activation marks a scheduled release going live, already admitted when it was published.
	Activation bool
}

// AgentSetting is one binding in force at a scope on an agent's chain. Editable marks the
// agent's own scope and its type's tenant-default scope, the two Studio writes.
type AgentSetting struct {
	ScopeID         string
	ScopeKind       string
	Editable        bool
	ResourceKind    ResourceKind
	ResourceID      string
	ResourceVersion string
	MergeMode       MergeMode
	Sealed          bool
	Value           json.RawMessage
}

// AgentSettings are the bindings on an agent's chain, other than prompt sections, and what
// they resolve to. Problem names why they do not compile; publication refuses them then.
type AgentSettings struct {
	AgentID               string
	Scopes                PromptScopes
	Bindings              []AgentSetting
	Effective             []ResourceExplanation
	Problem               string
	DraftRevision         int64
	PromptProfileRevision int64
}

// AgentSettingChange binds, or with Remove ends, one resource at the agent's scope or its
// type's scope. Other resources are untouched. The agent scope is behind the draft revision
// and the type scope behind the prompt profile revision.
type AgentSettingChange struct {
	AgentID                       string
	ScopeID                       string
	ResourceKind                  ResourceKind
	ResourceID                    string
	MergeMode                     MergeMode
	Sealed                        bool
	Value                         json.RawMessage
	Remove                        bool
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

// AgentSetEnabledRequest changes only the operational switch; Reason is recorded with it.
type AgentSetEnabledRequest struct {
	AgentID               string
	Enabled               bool
	Reason                string
	ExpectedDraftRevision int64
}

// PausePolicy says what happens to an agent's live turns when it is paused: they
// finish, they are asked to stop, or they hold. Either way no turn starts after the pause.
type PausePolicy string

const (
	PauseFinish PausePolicy = "finish"
	PauseCancel PausePolicy = "cancel"
	// PauseSuspend holds queued and parked turns at once and asks running ones to hold at their
	// next durable checkpoint; unpausing releases none of them.
	PauseSuspend PausePolicy = "suspend"
)

// AgentPauseRequest disables an agent and applies Policy to its live turns.
type AgentPauseRequest struct {
	AgentID               string
	Policy                PausePolicy
	Reason                string
	ExpectedDraftRevision int64
}

// AgentPauseResult is the disabled agent's revision and what its policy did to the live
// turns: asked to stop, held at once, or asked to hold at their next checkpoint.
type AgentPauseResult struct {
	DraftRevision int64
	Cancelled     int
	Held          int
	HoldRequested int
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
	DefinitionDigest      string
	DraftRevision         int64
	PromptProfileRevision int64
	ChangeSummary         string
	PublishedBy           *int64
	PublishedAt           time.Time
	RestoredFromVersion   string
	Active                bool
	Languages             []string
	// Activation is the pending activation of a scheduled publish.
	Activation *ReleaseActivation
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
	Actor      PrincipalRef
	OccurredAt time.Time
}

// TenantIdentity is the agent-platform registration of a keel business partner.
type TenantIdentity struct {
	TenantKey  string
	HomeRegion string
	// DefaultPolicy becomes current only while the tenant has no runtime policy; nil provisions none.
	DefaultPolicy *RuntimePolicyVersion
}

// AgentSeed is one idempotent agent provisioning request: the identity, its
// mutable draft configuration, and the logical alias it should serve.
type AgentSeed struct {
	AgentID     string
	AgentTypeID string
	AliasID     string
	DisplayName string
	Enabled     bool
	// Autonomy, when set, is bound at the agent's scope as its operating mode.
	Autonomy AutonomyMode
	Models   AgentModelSelection
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
	// Tools and ToolLoop, and Skills, are the most a release of the type binds; the agent's
	// tool and skill settings narrow them.
	Tools    []ToolReference
	ToolLoop *ToolLoopConfig
	Skills   []SkillReference
	// Guardrails are enforced rules every release of the type carries; an agent may add rules
	// or tighten one by id, never drop or loosen one.
	Guardrails []GuardrailRule
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
	Capabilities      []ModelCapability
	ContextTokenLimit int64
	OutputTokenLimit  int64
	Active            bool
	Rates             []ModelRate
}

// ModelUsage is the billable work one model performed: tokens for text, assets
// for images, whole seconds for video, and grounding searches.
type ModelUsage struct {
	InputTokens   int64
	OutputTokens  int64
	Images        int64
	VideoSeconds  int64
	SearchQueries int64
}
