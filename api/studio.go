package api

import (
	"encoding/json"
	"time"
)

const (
	StudioBasePath        = "/api/agent-studio/"
	StudioAgentsPath      = StudioBasePath + "agents"
	StudioAgentPath       = StudioBasePath + "agent"
	StudioDraftPath       = StudioBasePath + "draft"
	StudioEnabledPath     = StudioBasePath + "enabled"
	StudioTestPath        = StudioBasePath + "test"
	StudioPublishPath     = StudioBasePath + "publish"
	StudioRestorePath     = StudioBasePath + "restore"
	StudioResetPath       = StudioBasePath + "reset"
	StudioSetDefaultPath  = StudioBasePath + "set-default"
	StudioHistoryPath     = StudioBasePath + "history"
	StudioAuditPath       = StudioBasePath + "audit"
	StudioSectionsPath    = StudioBasePath + "release-sections"
	StudioModelsPath      = StudioBasePath + "models"
	StudioCreatePath      = StudioBasePath + "create"
	StudioRenamePath      = StudioBasePath + "rename"
	StudioArchivePath     = StudioBasePath + "archive"
	StudioUnarchivePath   = StudioBasePath + "unarchive"
	StudioFeaturesPath    = StudioBasePath + "features"
	StudioSettingsPath    = StudioBasePath + "settings"
	StudioSettingPath     = StudioBasePath + "setting"
	StudioCanaryPath      = StudioBasePath + "canary"
	StudioPromotePath     = StudioBasePath + "promote"
	StudioRollbackPath    = StudioBasePath + "rollback"
	StudioExplainPath     = StudioBasePath + "explain"
	StudioDelegationsPath = StudioBasePath + "delegations"
	StudioDelegatePath    = StudioBasePath + "delegate"
	StudioUndelegatePath  = StudioBasePath + "undelegate"
	StudioUsagePath       = StudioBasePath + "usage"
	StudioPausePath       = StudioBasePath + "pause"
	StudioFeedbackPath    = StudioBasePath + "feedback"
	StudioRunsPath        = StudioBasePath + "runs"
)

// AgentRun is one settled execution of a release; next pages pass the last id as before.
type AgentRun struct {
	ID          int64     `json:"id"`
	Version     string    `json:"version"`
	TaskKind    string    `json:"task_kind,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	Status      string    `json:"status"`
	CompletedAt time.Time `json:"completed_at"`
}

// ReleaseFeedback counts the ratings one release received; counts[r-1] holds rating r.
type ReleaseFeedback struct {
	Version string   `json:"version"`
	Counts  [5]int64 `json:"counts"`
}

// AgentPauseRequest disables an agent; policy finish lets its live turns complete and
// cancel asks them to stop.
type AgentPauseRequest struct {
	AgentName             string `json:"agent_name"`
	Policy                string `json:"policy"`
	Reason                string `json:"reason"`
	ExpectedAgentRevision int64  `json:"expected_agent_revision"`
}

// AgentPauseResult is the paused agent's revision and the live turns asked to stop.
type AgentPauseResult struct {
	ExpectedAgentRevision int64 `json:"expected_agent_revision"`
	Cancelled             int   `json:"cancelled"`
}

// AgentUsageLine is settled usage of one release, model, person, and work item; cost is
// in minor units of currency, which has exponent decimal places. Usage no model
// performed, such as tool calls, has no provider or model. Turns counts the turns whose
// model work the line holds, or on a no-model line the turns without model work, so it
// is not additive across the models of one release.
type AgentUsageLine struct {
	Version        string `json:"version"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	ActorKind      string `json:"actor_kind,omitempty"`
	ActorID        string `json:"actor_id,omitempty"`
	WorkRef        string `json:"work_ref,omitempty"`
	Currency       string `json:"currency"`
	Exponent       int    `json:"exponent"`
	Turns          int64  `json:"turns"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	ToolCalls      int64  `json:"tool_calls"`
	CostMinorUnits int64  `json:"cost_minor_units"`
}

// DelegationGrant lets agent_name hand work within action_scope to grantee, bounded
// in re-delegation depth, budget, and time.
type DelegationGrant struct {
	GrantID          string     `json:"grant_id,omitempty"`
	AgentName        string     `json:"agent_name"`
	Grantee          string     `json:"grantee"`
	ActionScope      string     `json:"action_scope"`
	MaxDepth         int        `json:"max_depth"`
	BudgetMinorUnits int64      `json:"budget_minor_units,omitempty"`
	Currency         string     `json:"currency,omitempty"`
	ApprovalRequired bool       `json:"approval_required"`
	ValidFrom        *time.Time `json:"valid_from,omitempty"`
	ValidTo          time.Time  `json:"valid_to"`
}

// DelegationRevokeRequest ends one of agent_name's grants.
type DelegationRevokeRequest struct {
	AgentName string `json:"agent_name"`
	GrantID   string `json:"grant_id"`
}

// AgentTrafficRequest moves an agent's traffic: canary takes version and percentage,
// promote takes version, rollback takes neither.
type AgentTrafficRequest struct {
	AgentName  string `json:"agent_name"`
	Version    string `json:"version,omitempty"`
	Percentage int    `json:"percentage,omitempty"`
}

// AgentDeployment is the traffic policy new conversations of an agent take.
type AgentDeployment struct {
	AgentName        string `json:"agent_name"`
	StableVersion    string `json:"stable_version"`
	CanaryVersion    string `json:"canary_version,omitempty"`
	CanaryPercentage int    `json:"canary_percentage"`
}

// ConversationVersion names the release a conversation runs on and the rule that chose
// it: stable, canary, experiment_cohort, pilot, compliance_pin, tenant_pin, or request_pin.
type ConversationVersion struct {
	ConversationID   string    `json:"conversation_id"`
	AgentName        string    `json:"agent_name"`
	AgentType        string    `json:"agent_type,omitempty"`
	Version          string    `json:"version"`
	Source           string    `json:"source"`
	PinReason        string    `json:"pin_reason,omitempty"`
	DefinitionDigest string    `json:"definition_digest"`
	StartedAt        time.Time `json:"started_at"`
}

// AgentSettingSource is the binding that decided, or was superseded in, an effective value.
type AgentSettingSource struct {
	ScopeID         string `json:"scope_id"`
	ScopeKind       string `json:"scope_kind"`
	ResourceVersion string `json:"resource_version"`
	MergeMode       string `json:"merge_mode"`
	Sealed          bool   `json:"sealed"`
}

// AgentSetting is one binding in force on the agent's scope chain; editable marks the
// agent's and its type's scopes.
type AgentSetting struct {
	ScopeID         string          `json:"scope_id"`
	ScopeKind       string          `json:"scope_kind"`
	Editable        bool            `json:"editable"`
	ResourceKind    string          `json:"resource_kind"`
	ResourceID      string          `json:"resource_id"`
	ResourceVersion string          `json:"resource_version"`
	MergeMode       string          `json:"merge_mode"`
	Sealed          bool            `json:"sealed"`
	Value           json.RawMessage `json:"value"`
}

// AgentEffectiveSetting is what one resource resolves to and why.
type AgentEffectiveSetting struct {
	ResourceKind string               `json:"resource_kind"`
	ResourceID   string               `json:"resource_id"`
	Value        json.RawMessage      `json:"value"`
	Source       AgentSettingSource   `json:"source"`
	Superseded   []AgentSettingSource `json:"superseded,omitempty"`
}

// AgentSettings are an agent's bindings other than prompt sections and what they resolve
// to; problem says why they do not compile.
type AgentSettings struct {
	AgentName                    string                  `json:"agent_name"`
	AgentScopeID                 string                  `json:"agent_scope_id"`
	TypeScopeID                  string                  `json:"type_scope_id"`
	Bindings                     []AgentSetting          `json:"bindings"`
	Effective                    []AgentEffectiveSetting `json:"effective"`
	Problem                      string                  `json:"problem,omitempty"`
	ExpectedAgentRevision        int64                   `json:"expected_agent_revision"`
	ExpectedTypeDefaultsRevision int64                   `json:"expected_type_defaults_revision"`
}

// AgentSettingChange binds, or with remove ends, one resource at agent_scope_id or type_scope_id.
type AgentSettingChange struct {
	AgentName                    string          `json:"agent_name"`
	ScopeID                      string          `json:"scope_id"`
	ResourceKind                 string          `json:"resource_kind"`
	ResourceID                   string          `json:"resource_id"`
	MergeMode                    string          `json:"merge_mode,omitempty"`
	Sealed                       bool            `json:"sealed"`
	Value                        json.RawMessage `json:"value,omitempty"`
	Remove                       bool            `json:"remove"`
	ExpectedAgentRevision        int64           `json:"expected_agent_revision"`
	ExpectedTypeDefaultsRevision int64           `json:"expected_type_defaults_revision"`
}

// StudioFeature is one optional Studio API a product mounted, with the actions it
// serves. A client ignores an id it does not know; the caller's grants still decide.
type StudioFeature struct {
	ID      string   `json:"id"`
	Version int      `json:"version"`
	Actions []string `json:"actions"`
	// Object is the authorization object the actions are granted on; empty is AGENT_STUDIO.
	Object string `json:"object,omitempty"`
}

// AgentSummary is the studio-v2 agent list item.
type AgentSummary struct {
	AgentType            string     `json:"agent_type"`
	AgentName            string     `json:"agent_name"`
	DisplayName          string     `json:"display_name"`
	Purpose              string     `json:"purpose"`
	Enabled              bool       `json:"enabled"`
	Default              bool       `json:"is_default"`
	Archived             bool       `json:"archived"`
	Readiness            string     `json:"readiness"`
	ReadinessReason      string     `json:"readiness_reason"`
	TypeDefaultsRevision int64      `json:"type_defaults_revision"`
	AgentRevision        int64      `json:"agent_revision"`
	PublishedVersion     int64      `json:"published_version"`
	PublishedAt          *time.Time `json:"published_at,omitempty"`
	LastRunAt            *time.Time `json:"last_run_at,omitempty"`
}

// AgentFieldError is one studio-v2 field validation failure.
type AgentFieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationProblem is the studio-v2 validation response detail.
type ValidationProblem struct {
	Message string            `json:"message"`
	Fields  []AgentFieldError `json:"fields"`
}

// AgentModelSelection is the studio-v2 standard model selection.
type AgentModelSelection struct {
	TextModel  string `json:"text_model"`
	ImageModel string `json:"image_model"`
	VideoModel string `json:"video_model"`
}

// AgentPromptLayer is one contribution to a prompt section, widest scope first. The first
// is the platform baseline (scope_kind "platform"); the rest are the prompt_section bindings
// of the scopes on the agent's chain. A save writes the editable layers it is sent and ends
// an editable binding it is not sent; layers that are not editable are ignored on the way in.
type AgentPromptLayer struct {
	ScopeID     string `json:"scope_id"`
	ScopeKind   string `json:"scope_kind"`
	MergeMode   string `json:"merge_mode"`
	Sealed      bool   `json:"sealed"`
	Editable    bool   `json:"editable"`
	Instruction string `json:"instruction"`
	Output      string `json:"output"`
	// BoundBy is the user who last edited the layer; it is read-only and absent on the baseline.
	BoundBy *int64 `json:"bound_by,omitempty"`
}

// AgentPromptSection is the studio-v2 prompt inheritance view.
type AgentPromptSection struct {
	PromptHeaderID  int64              `json:"prompt_header_id"`
	Caption         string             `json:"caption"`
	Description     string             `json:"description"`
	Layers          []AgentPromptLayer `json:"layers"`
	EffectiveText   string             `json:"effective_text"`
	EffectiveOutput string             `json:"effective_output"`
}

// AgentLanguageDraft is the studio-v2 prompt draft for one language.
type AgentLanguageDraft struct {
	LanguageCode   string               `json:"language_code"`
	PromptSections []AgentPromptSection `json:"prompt_sections"`
}

// AgentDrift is the studio-v2 active-release drift report.
type AgentDrift struct {
	ActiveVersion    int64    `json:"active_version"`
	ChangedLanguages []string `json:"changed_languages"`
	Causes           []string `json:"causes"`
}

// AgentDraft is the studio-v2 revision-checked editable representation.
type AgentDraft struct {
	AgentType   string               `json:"agent_type"`
	AgentName   string               `json:"agent_name"`
	DisplayName string               `json:"display_name"`
	Enabled     bool                 `json:"enabled"`
	Default     bool                 `json:"is_default"`
	Models      AgentModelSelection  `json:"models"`
	Languages   []AgentLanguageDraft `json:"languages"`
	// AgentScopeID and TypeScopeID are the two scopes a client may add a layer at; response only.
	AgentScopeID                 string      `json:"agent_scope_id,omitempty"`
	TypeScopeID                  string      `json:"type_scope_id,omitempty"`
	Drift                        *AgentDrift `json:"drift,omitempty"`
	ExpectedTypeDefaultsRevision int64       `json:"expected_type_defaults_revision"`
	ExpectedAgentRevision        int64       `json:"expected_agent_revision"`
}

// AgentTestRequest is the studio-v2 saved-draft test request.
type AgentTestRequest struct {
	AgentName    string `json:"agent_name"`
	LanguageCode string `json:"language_code"`
	Task         string `json:"task"`
	InputData    string `json:"input_data"`
}

// AgentTestResult is the studio-v2 test result and usage view.
type AgentTestResult struct {
	AgentName    string   `json:"agent_name"`
	LanguageCode string   `json:"language_code"`
	Model        string   `json:"model"`
	Digest       string   `json:"digest"`
	Output       string   `json:"output"`
	LatencyMs    int64    `json:"latency_ms"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Credits      int64    `json:"credits"`
	Sections     []string `json:"sections"`
}

// AgentCreateRequest creates an agent of an existing type; copy_from and template_id are exclusive.
type AgentCreateRequest struct {
	AgentType      string `json:"agent_type"`
	DisplayName    string `json:"display_name"`
	CopyFrom       string `json:"copy_from,omitempty"`
	TemplateID     string `json:"template_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

// AgentRenameRequest changes an agent's display name.
type AgentRenameRequest struct {
	AgentName             string `json:"agent_name"`
	DisplayName           string `json:"display_name"`
	ExpectedAgentRevision int64  `json:"expected_agent_revision"`
}

// AgentArchiveRequest archives or restores an agent.
type AgentArchiveRequest struct {
	AgentName             string `json:"agent_name"`
	ExpectedAgentRevision int64  `json:"expected_agent_revision"`
}

// AgentPublishRequest is the studio-v2 optimistic publish request; canary_percentage
// between 1 and 99 deploys the release as a canary instead of stable.
type AgentPublishRequest struct {
	AgentName                    string `json:"agent_name"`
	ChangeSummary                string `json:"change_summary"`
	CanaryPercentage             int    `json:"canary_percentage,omitempty"`
	ExpectedAgentRevision        int64  `json:"expected_agent_revision"`
	ExpectedTypeDefaultsRevision int64  `json:"expected_type_defaults_revision"`
	// EffectiveAt (RFC 3339) in the future schedules the release instead of deploying it now.
	EffectiveAt *time.Time `json:"effective_at,omitempty"`
}

// AgentRestoreRequest is the studio-v2 restore request.
type AgentRestoreRequest struct {
	AgentName string `json:"agent_name"`
	Version   int64  `json:"version"`
}

// AgentResetRequest is the studio-v2 prompt reset request.
type AgentResetRequest struct {
	AgentName                    string `json:"agent_name"`
	Scope                        string `json:"scope"`
	PromptHeaderID               int64  `json:"prompt_header_id"`
	LanguageCode                 string `json:"language_code"`
	ExpectedAgentRevision        int64  `json:"expected_agent_revision"`
	ExpectedTypeDefaultsRevision int64  `json:"expected_type_defaults_revision"`
}

// AgentSetDefaultRequest is the studio-v2 logical-kind alias update.
type AgentSetDefaultRequest struct {
	AgentName                    string `json:"agent_name"`
	ExpectedTypeDefaultsRevision int64  `json:"expected_type_defaults_revision"`
}

// AgentSetEnabledRequest is the studio-v2 kill-switch request.
type AgentSetEnabledRequest struct {
	AgentName             string `json:"agent_name"`
	Enabled               bool   `json:"enabled"`
	Reason                string `json:"reason,omitempty"`
	ExpectedAgentRevision int64  `json:"expected_agent_revision"`
}

// AgentEnabledState is the studio-v2 kill-switch result.
type AgentEnabledState struct {
	Enabled               bool  `json:"enabled"`
	ExpectedAgentRevision int64 `json:"expected_agent_revision"`
}

// AgentRelease is the studio-v2 immutable release history item.
type AgentRelease struct {
	AgentName        string              `json:"agent_name"`
	AgentType        string              `json:"agent_type"`
	Version          int64               `json:"version"`
	Enabled          bool                `json:"enabled"`
	Models           AgentModelSelection `json:"models"`
	DefinitionDigest string              `json:"definition_digest"`
	ChangeSummary    string              `json:"change_summary"`
	PublishedBy      int64               `json:"published_by"`
	PublishedAt      time.Time           `json:"published_at"`
	Active           bool                `json:"active"`
	Languages        []string            `json:"languages"`
	// Activation is set when the publish was scheduled.
	Activation *ReleaseActivation `json:"activation,omitempty"`
}

// AgentAuditEvent is one studio-v2 lifecycle event. UserID is set only when a person acted.
type AgentAuditEvent struct {
	Event     string    `json:"event"`
	Detail    string    `json:"detail"`
	ActorKind string    `json:"actor_kind,omitempty"`
	ActorID   string    `json:"actor_id,omitempty"`
	UserID    int64     `json:"user_id,omitempty"`
	EventTime time.Time `json:"event_time"`
}

// AgentReleaseSection is one studio-v2 immutable prompt section.
type AgentReleaseSection struct {
	LanguageCode   string `json:"language_code"`
	PromptHeaderID int64  `json:"prompt_header_id"`
	Caption        string `json:"caption"`
	Description    string `json:"description"`
	Instruction    string `json:"instruction"`
	Output         string `json:"output"`
	Sequence       int64  `json:"sequence"`
	// Source is the layer that decided the section when the release was compiled.
	Source AgentPromptSource `json:"source"`
}

// AgentPromptSource is the provenance frozen into a release section.
type AgentPromptSource struct {
	ScopeID   string `json:"scope_id"`
	ScopeKind string `json:"scope_kind"`
	MergeMode string `json:"merge_mode"`
	Sealed    bool   `json:"sealed"`
}

// StudioModel is the studio-v2 model catalog item with display credit guidance.
type StudioModel struct {
	ID                 string  `json:"id"`
	Provider           string  `json:"provider"`
	ModelType          string  `json:"model_type"`
	DisplayName        string  `json:"display_name"`
	InputCreditsPer1k  float64 `json:"input_credits_per_1k"`
	OutputCreditsPer1k float64 `json:"output_credits_per_1k"`
	ImageCredits       float64 `json:"image_credits"`
	VideoCreditsPerSec float64 `json:"video_credits_per_sec"`
}
