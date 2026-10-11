package domain

import "time"

// ContractTestCase defines one agent compatibility assertion.
type ContractTestCase struct {
	TestCaseID   string
	AgentID      string
	AgentVersion string
	Input        []byte
	Assertions   []byte
}

// ContractTestResult contains the outcome of one compatibility test.
type ContractTestResult struct {
	TestCaseID string
	Passed     bool
	Failures   []string
}

// RolloutTarget identifies a platform build and tenant rollout ring.
type RolloutTarget struct {
	PlatformVersion string
	TenantRing      string
	Percentage      int
}

// RolloutStatus is the traffic state of a platform release in one tenant ring.
type RolloutStatus string

const (
	RolloutStatusPending    RolloutStatus = "P"
	RolloutStatusActive     RolloutStatus = "A"
	RolloutStatusPaused     RolloutStatus = "U"
	RolloutStatusCompleted  RolloutStatus = "C"
	RolloutStatusRolledBack RolloutStatus = "R"
)

// RolloutVerdict is a three-state health outcome; inconclusive pauses promotion.
type RolloutVerdict string

const (
	RolloutHealthy      RolloutVerdict = "H"
	RolloutUnhealthy    RolloutVerdict = "U"
	RolloutInconclusive RolloutVerdict = "I"
)

// Valid reports whether verdict is one of the rollout_health_verdict codes.
func (verdict RolloutVerdict) Valid() bool {
	switch verdict {
	case RolloutHealthy, RolloutUnhealthy, RolloutInconclusive:
		return true
	default:
		return false
	}
}

// RolloutHealth carries a verdict with the evidence behind it.
type RolloutHealth struct {
	Verdict          RolloutVerdict
	BreachedMetric   string
	Breach           GuardrailBreach
	Samples          int64
	Window           time.Duration
	EffectSize       float64
	Confidence       float64
	TelemetryFreshAt time.Time
}

// RolloutStage is one step of the platform release state machine.
type RolloutStage string

const (
	StageBuild          RolloutStage = "BD"
	StageOfflineReplay  RolloutStage = "OR"
	StageShadow         RolloutStage = "SH"
	StageInternalCanary RolloutStage = "IC"
	StageTenantCanary   RolloutStage = "TC"
	StageRegionalRamp   RolloutStage = "RR"
	StageGlobalDefault  RolloutStage = "GD"
	StageRetired        RolloutStage = "RT"
	StageRolledBack     RolloutStage = "RB"
	StageQuarantined    RolloutStage = "QU"
)

// RolloutState is the durable, generation-fenced position of one platform release.
type RolloutState struct {
	PlatformVersion     string
	Stage               RolloutStage
	Ring                string
	TrafficPercentage   int
	Generation          int64
	Paused              bool
	PauseReason         string
	StageStartedAt      time.Time
	MinSamples          int64
	MinDuration         time.Duration
	ConsecutiveBreaches int
	ConsecutiveHealthy  int
	LastBreachAt        time.Time
	LeaseOwner          string
	LeaseUntil          time.Time
}

// RolloutTransition is one audited stage change, fenced by the generation it left.
type RolloutTransition struct {
	PlatformVersion string
	From            RolloutStage
	To              RolloutStage
	FromGeneration  int64
	Actor           string
	Reason          string
	OccurredAt      time.Time
}

// RolloutBypass is an approved, scoped waiver of one stage's evidence gate.
type RolloutBypass struct {
	ID              int64
	PlatformVersion string
	Stage           RolloutStage
	Scope           string
	Reason          string
	RequestedBy     string
	ApprovedBy      string
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// BreachKind separates guardrails that roll back at once from those needing consecutive evidence.
type BreachKind string

const (
	BreachHard BreachKind = "hard"
	BreachSoft BreachKind = "soft"
)

// GuardrailBreach describes the metric behind an unhealthy verdict.
type GuardrailBreach struct {
	Kind        BreachKind
	Metric      string
	Window      time.Duration
	Consecutive int
}

// ReleaseBundle is the certified, signed set of versions one platform release ships.
type ReleaseBundle struct {
	PlatformVersion          string
	Versions                 ComponentVersions
	ProviderVersion          string
	Tokenizer                string
	Runtime                  string
	DecodingDefaults         []byte
	Embedding                string
	Reranker                 string
	IndexGeneration          string
	ToolVersions             map[string]string
	SafetyPolicyVersion      string
	MigrationSet             []string
	RollbackTarget           string
	ResidencyPolicy          string
	Provenance               string
	Signature                string
	SignerKeyID              string
	CompatibilityConstraints []byte
	Digest                   string
}

// PinScope orders pins: compliance pins beat tenant pins.
type PinScope string

const (
	PinScopeCompliance PinScope = "C"
	PinScopeTenant     PinScope = "T"
)

// Valid reports whether scope is one of the version_pin_scope codes.
func (scope PinScope) Valid() bool {
	return scope == PinScopeCompliance || scope == PinScopeTenant
}

// VersionPin holds one tenant agent on an immutable version for a bounded period.
type VersionPin struct {
	ID                       int64
	Scope                    PinScope
	TenantID                 int64
	AgentID                  string
	Version                  string
	Region                   string
	Reason                   string
	Owner                    string
	ApprovedBy               string
	Signature                string
	EffectiveAt              time.Time
	ExpiresAt                time.Time
	CompatiblePolicyVersions []string
	CompatibleIndexVersions  []string
	CreatedAt                time.Time
}

// ExperimentCohort assigns a stable-hash share of a tenant agent's subjects to a version.
type ExperimentCohort struct {
	TenantID     int64
	AgentID      string
	ExperimentID string
	Version      string
	Percentage   int
	Salt         string
}

// VersionSource names the precedence rule that selected an agent version.
type VersionSource string

const (
	VersionFromCompliancePin VersionSource = "compliance_pin"
	VersionFromTenantPin     VersionSource = "tenant_pin"
	VersionFromPilot         VersionSource = "pilot"
	VersionFromCohort        VersionSource = "experiment_cohort"
	VersionFromCanary        VersionSource = "canary"
	VersionFromStable        VersionSource = "stable"
	// VersionFromRequest is a release the caller pinned a new conversation to.
	VersionFromRequest VersionSource = "request_pin"
)

// VersionRequest asks which release a new conversation starts on. Subject is the person the
// conversation acts for, taken from the trusted session; zero when it acts for no one.
type VersionRequest struct {
	TenantID       int64
	AgentID        string
	ConversationID string
	Subject        PrincipalRef
}

// AgentVersionResolution is a selected agent version with the rule that chose it; Pilot is
// the membership that won when Source is VersionFromPilot.
type AgentVersionResolution struct {
	Version string
	Source  VersionSource
	PinID   int64
	Pilot   PilotSubject
}

// AgentDeployment is a tenant agent's stable and canary traffic policy.
type AgentDeployment struct {
	TenantID         int64
	AgentID          string
	StableVersion    string
	CanaryVersion    string
	CanaryPercentage int
}

// ConversationRelease is the pair of immutable identities a conversation was created on.
type ConversationRelease struct {
	TenantID        int64
	ConversationID  string
	AgentVersion    string
	PlatformVersion string
	ResolvedAt      time.Time
}

// ConversationVersion is the release a conversation runs on and why it was chosen.
type ConversationVersion struct {
	ConversationID   string
	AgentID          string
	AliasID          string
	Version          string
	Source           VersionSource
	PinReason        string
	DefinitionDigest string
	StartedAt        time.Time
}

// SessionDrainPolicy bounds how a rolled-back release lets live conversations finish.
type SessionDrainPolicy struct {
	Window                 time.Duration
	CancelOnCriticalSafety bool
}

// RollbackDrillReport records one rehearsed rollback and every check it made.
type RollbackDrillReport struct {
	PlatformVersion string
	RollbackTarget  string
	StartedAt       time.Time
	Duration        time.Duration
	Checks          []DrillCheck
	Passed          bool
}

// DrillCheck is one named rollback assertion.
type DrillCheck struct {
	Name    string
	Passed  bool
	Detail  string
	Elapsed time.Duration
}

// SyntheticProbeResult is the outcome of one synthetic probe against a release.
type SyntheticProbeResult struct {
	PlatformVersion string
	ProbeID         string
	Holdout         bool
	Passed          bool
	Failures        []string
	Latency         time.Duration
	ObservedAt      time.Time
}
