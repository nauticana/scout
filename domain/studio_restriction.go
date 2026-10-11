package domain

// RestrictionAuthorizationObject is the keel authorization object the restriction layers are
// granted on. A tenant has one layer, so both actions are checked at the tenant-wide value
// RestrictionAuthorizationValue.
const (
	RestrictionAuthorizationObject = "AGENT_RESTRICTION"
	RestrictionAuthorizationValue  = "tenant"
)

// RestrictionAction is an AGENT_RESTRICTION action. VIEW shows the platform layer and the
// tenant's; REPLACE_TENANT replaces the tenant's as a whole. No action writes the platform
// layer: only a service principal does.
type RestrictionAction string

const (
	RestrictionView          RestrictionAction = "VIEW"
	RestrictionReplaceTenant RestrictionAction = "REPLACE_TENANT"
)

// RestrictionLayerName names one of the two restriction layers.
type RestrictionLayerName string

const (
	RestrictionPlatform RestrictionLayerName = "platform"
	RestrictionTenant   RestrictionLayerName = "tenant"
)

// RestrictionKind is what a restriction is: a deny statement or a guardrail rule.
type RestrictionKind string

const (
	RestrictionDenial    RestrictionKind = "denial"
	RestrictionGuardrail RestrictionKind = "guardrail"
)

// RestrictedSetting is one effective setting of an agent that a restriction overrides.
// Members are the tools of a tool set it takes away, or the allow statements of a policy
// it denies.
type RestrictedSetting struct {
	ResourceKind ResourceKind
	ResourceID   string
	Members      []string
}

// RestrictionOverride is one restriction and the effective settings of an agent it
// overrides. Conditional marks a denial that applies only to decisions whose environment
// holds its conditions.
type RestrictionOverride struct {
	Layer         RestrictionLayerName
	Kind          RestrictionKind
	RestrictionID string
	Conditional   bool
	Settings      []RestrictedSetting
}

// RestrictionOverview is what Studio shows of the restrictions in force for a tenant. For
// an agent, Overrides lists the restrictions that override its effective settings, and
// SettingsProblem says why its settings do not compile when they do not.
type RestrictionOverview struct {
	Layers          RestrictionLayers
	AgentID         string
	Overrides       []RestrictionOverride
	SettingsProblem string
}
