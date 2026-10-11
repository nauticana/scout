package domain

import "time"

const (
	// StudioTemplate saves an agent as a template, at "agent_type:<id>".
	StudioTemplate StudioAction = "TEMPLATE"
	// StudioTemplatePublish shares a template with other tenants, at "template_audience:tenants"
	// or "template_audience:all".
	StudioTemplatePublish StudioAction = "TEMPLATE_PUBLISH"
)

// TemplateOwner says whose template a tenant sees: its own, another tenant's shared with it, or the platform's.
type TemplateOwner string

const (
	TemplateOwn      TemplateOwner = "own"
	TemplateShared   TemplateOwner = "shared"
	TemplatePlatform TemplateOwner = "platform"
)

// TemplatePayload is what a template version carries: agent-scope bindings whose knowledge and
// entitlement values are slot names, never resources of the owner. Slots lists those names per kind.
type TemplatePayload struct {
	Bindings []PortableBinding         `json:"bindings"`
	Slots    map[ResourceKind][]string `json:"slots,omitempty"`
}

// AgentTemplateVersion is one immutable version of a template. SourceAgentID and SourceDraftRevision
// are set for the owner only; OwnerTenantID is the owning tenant of a tenant template and is never shown.
type AgentTemplateVersion struct {
	TemplateID          string
	Version             int64
	Owner               TemplateOwner
	OwnerTenantID       int64
	AgentTypeID         string
	TypeVersion         string
	TypeDigest          string
	DisplayName         string
	Description         string
	ChangeSummary       string
	Payload             TemplatePayload
	Digest              string
	CreatedAt           time.Time
	SourceAgentID       string
	SourceDraftRevision int64
}

// TemplateAudience is who besides the owner may use a template: every tenant, or the listed ones.
type TemplateAudience struct {
	All     bool
	Tenants []int64
}

// AgentTemplate is a template's newest version; Audience is set for the owner only.
type AgentTemplate struct {
	Latest   AgentTemplateVersion
	Audience *TemplateAudience
}

// AgentTemplateSaveRequest saves the agent's own bindings as a new template, or as a new version of
// TemplateID. Slots map each knowledge and entitlement value of the agent to the slot name stored instead.
type AgentTemplateSaveRequest struct {
	AgentID        string
	TemplateID     string
	IdempotencyKey string
	DisplayName    string
	Description    string
	ChangeSummary  string
	Slots          map[ResourceKind]map[string]string
}

// AgentTemplatePublication shares a template with Audience, or with Withdraw stops sharing it.
type AgentTemplatePublication struct {
	TemplateID string
	Audience   TemplateAudience
	Withdraw   bool
}

// AgentTemplateStatus reports what an agent derives from and, for a template, whether a newer
// version the tenant may use exists.
type AgentTemplateStatus struct {
	AgentID       string
	DerivedFrom   *DerivedFrom
	TemplateID    string
	Version       int64
	LatestVersion int64
	Newer         bool
	Available     bool
}

// TemplateLimits bound a tenant's templates: templates of one type and versions of one template.
type TemplateLimits struct {
	MaxPerType  int
	MaxVersions int
}
