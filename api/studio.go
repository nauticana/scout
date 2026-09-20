package api

import "time"

const (
	StudioBasePath       = "/api/agent-studio/"
	StudioAgentsPath     = StudioBasePath + "agents"
	StudioAgentPath      = StudioBasePath + "agent"
	StudioDraftPath      = StudioBasePath + "draft"
	StudioEnabledPath    = StudioBasePath + "enabled"
	StudioTestPath       = StudioBasePath + "test"
	StudioPublishPath    = StudioBasePath + "publish"
	StudioRestorePath    = StudioBasePath + "restore"
	StudioResetPath      = StudioBasePath + "reset"
	StudioSetDefaultPath = StudioBasePath + "set-default"
	StudioHistoryPath    = StudioBasePath + "history"
	StudioAuditPath      = StudioBasePath + "audit"
	StudioSectionsPath   = StudioBasePath + "release-sections"
	StudioModelsPath     = StudioBasePath + "models"
)

// AgentSummary is the studio-v2 agent list item.
type AgentSummary struct {
	AgentType            string     `json:"agent_type"`
	AgentName            string     `json:"agent_name"`
	DisplayName          string     `json:"display_name"`
	Purpose              string     `json:"purpose"`
	Enabled              bool       `json:"enabled"`
	Default              bool       `json:"is_default"`
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

// AgentApprovalPolicy is the studio-v2 approval policy.
type AgentApprovalPolicy struct {
	RequireApproval bool `json:"require_approval"`
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
	AgentType      string               `json:"agent_type"`
	AgentName      string               `json:"agent_name"`
	DisplayName    string               `json:"display_name"`
	Enabled        bool                 `json:"enabled"`
	Default        bool                 `json:"is_default"`
	ApprovalPolicy AgentApprovalPolicy  `json:"approval_policy"`
	Models         AgentModelSelection  `json:"models"`
	Languages      []AgentLanguageDraft `json:"languages"`
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

// AgentPublishRequest is the studio-v2 optimistic publish request.
type AgentPublishRequest struct {
	AgentName                    string `json:"agent_name"`
	ChangeSummary                string `json:"change_summary"`
	ExpectedAgentRevision        int64  `json:"expected_agent_revision"`
	ExpectedTypeDefaultsRevision int64  `json:"expected_type_defaults_revision"`
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
	RequireApproval  bool                `json:"require_approval"`
	DefinitionDigest string              `json:"definition_digest"`
	ChangeSummary    string              `json:"change_summary"`
	PublishedBy      int64               `json:"published_by"`
	PublishedAt      time.Time           `json:"published_at"`
	Active           bool                `json:"active"`
	Languages        []string            `json:"languages"`
}

// AgentAuditEvent is one studio-v2 lifecycle event.
type AgentAuditEvent struct {
	Event     string    `json:"event"`
	Detail    string    `json:"detail"`
	UserID    int64     `json:"user_id"`
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
