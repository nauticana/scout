package api

import (
	"encoding/json"
	"time"
)

const (
	// StudioTemplatesPath serves GET templates?agent_type=: the templates the caller's tenant may use.
	StudioTemplatesPath = StudioBasePath + "templates"
	// StudioTemplatePath serves POST template: save an agent as a new template or a new version.
	StudioTemplatePath = StudioBasePath + "template"
	// StudioTemplatePublishPath serves POST template/publish: share a template or withdraw it.
	StudioTemplatePublishPath = StudioBasePath + "template/publish"
	// StudioTemplateStatusPath serves GET template/status?agent_name=: what an agent derives from.
	StudioTemplateStatusPath = StudioBasePath + "template/status"
)

// CharterObjectRef is a Charter objectRef.
type CharterObjectRef struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Namespace string `json:"namespace,omitempty"`
	External  bool   `json:"external,omitempty"`
}

// DerivedFrom is the source, version, and "sha256:<hex>" digest an agent derives from.
type DerivedFrom struct {
	Source  CharterObjectRef `json:"source"`
	Version string           `json:"version"`
	Digest  string           `json:"digest"`
}

// TemplateBinding is one binding a template carries; a prompt section is named by caption and language.
type TemplateBinding struct {
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id,omitempty"`
	Caption      string          `json:"caption,omitempty"`
	Language     string          `json:"language,omitempty"`
	MergeMode    string          `json:"merge_mode"`
	Sealed       bool            `json:"sealed,omitempty"`
	Value        json.RawMessage `json:"value"`
}

// TemplateAudience is who besides the owner may use a template.
type TemplateAudience struct {
	All     bool    `json:"all"`
	Tenants []int64 `json:"tenants"`
}

// AgentTemplate is a template's newest version. owner is own, shared, or platform; source_agent and
// audience are shown to the owner only. slots name the knowledge and entitlement values a create maps.
type AgentTemplate struct {
	TemplateID    string              `json:"template_id"`
	Version       int64               `json:"version"`
	Owner         string              `json:"owner"`
	AgentType     string              `json:"agent_type"`
	TypeVersion   string              `json:"type_version,omitempty"`
	DisplayName   string              `json:"display_name"`
	Description   string              `json:"description,omitempty"`
	ChangeSummary string              `json:"change_summary,omitempty"`
	Slots         map[string][]string `json:"slots,omitempty"`
	Bindings      []TemplateBinding   `json:"bindings"`
	Digest        string              `json:"digest"`
	CreatedAt     time.Time           `json:"created_at"`
	SourceAgent   string              `json:"source_agent,omitempty"`
	Audience      *TemplateAudience   `json:"audience,omitempty"`
}

// AgentTemplateSaveRequest saves agent_name's own bindings as a new template, which needs
// idempotency_key, or as a new version of template_id. slots map each knowledge and entitlement value
// of the agent to the slot name the template stores instead.
type AgentTemplateSaveRequest struct {
	AgentName      string                       `json:"agent_name"`
	TemplateID     string                       `json:"template_id,omitempty"`
	IdempotencyKey string                       `json:"idempotency_key,omitempty"`
	DisplayName    string                       `json:"display_name"`
	Description    string                       `json:"description,omitempty"`
	ChangeSummary  string                       `json:"change_summary,omitempty"`
	Slots          map[string]map[string]string `json:"slots,omitempty"`
}

// AgentTemplatePublication shares a template with every tenant (all) or the listed tenants, or with
// withdraw stops sharing it with them.
type AgentTemplatePublication struct {
	TemplateID string  `json:"template_id"`
	All        bool    `json:"all,omitempty"`
	Tenants    []int64 `json:"tenants,omitempty"`
	Withdraw   bool    `json:"withdraw,omitempty"`
}

// AgentTemplateStatus reports what an agent derives from; for a template, newer says a later version
// the tenant may use exists, and available is false once the tenant may no longer use the template.
type AgentTemplateStatus struct {
	AgentName       string       `json:"agent_name"`
	DerivedFrom     *DerivedFrom `json:"derived_from,omitempty"`
	TemplateID      string       `json:"template_id,omitempty"`
	TemplateVersion int64        `json:"template_version,omitempty"`
	LatestVersion   int64        `json:"latest_version,omitempty"`
	Newer           bool         `json:"newer"`
	Available       bool         `json:"available"`
}
