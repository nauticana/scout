package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// AgentContractTestRunner validates agents against a proposed platform build.
type AgentContractTestRunner interface {
	// Run evaluates a proposed platform build against tenant agent contracts.
	Run(ctx context.Context, platformVersion string, cases []domain.ContractTestCase) ([]domain.ContractTestResult, error)
}

// ContractTestExecutor runs one case against a proposed platform build.
type ContractTestExecutor interface {
	// Execute returns the governed runtime result for one compatibility case.
	Execute(ctx context.Context, platformVersion string, testCase domain.ContractTestCase) (domain.TurnResult, error)
}

// ContractAssertionEvaluator evaluates one runtime result against encoded assertions.
type ContractAssertionEvaluator interface {
	// Evaluate returns every failed assertion for one compatibility case.
	Evaluate(ctx context.Context, testCase domain.ContractTestCase, result domain.TurnResult) ([]string, error)
}

// TenantCorpusSampler selects representative tenant agents for compatibility tests.
type TenantCorpusSampler interface {
	// Sample returns a risk-stratified corpus across tenant agents and capabilities.
	Sample(ctx context.Context, platformVersion string, limit int) ([]domain.ContractTestCase, error)
}

// PlatformReleaseRolloutController manages platform releases by tenant ring.
type PlatformReleaseRolloutController interface {
	// Start begins a staged platform rollout at the first tenant ring.
	Start(ctx context.Context, target domain.RolloutTarget) error
	// Advance moves a healthy platform build to the next tenant ring.
	Advance(ctx context.Context, platformVersion string) error
	// Halt stops assigning new traffic to a platform build.
	Halt(ctx context.Context, platformVersion, reason string) error
	// Rollback restores affected tenant rings to the prior platform build.
	Rollback(ctx context.Context, platformVersion string) error
}

// RolloutHealthEvaluator determines whether a staged release may advance.
type RolloutHealthEvaluator interface {
	// Healthy evaluates latency, errors, cost, quality, and contract regressions.
	Healthy(ctx context.Context, target domain.RolloutTarget) (bool, error)
}

// DetailedRolloutHealthEvaluator reports evidence and inconclusive telemetry.
type DetailedRolloutHealthEvaluator interface {
	Evaluate(ctx context.Context, target domain.RolloutTarget) (domain.RolloutHealth, error)
}

// RolloutStateStore persists generation-fenced rollout positions and their audit trail.
type RolloutStateStore interface {
	// Get returns the current position of one platform release.
	Get(ctx context.Context, platformVersion string) (domain.RolloutState, error)
	// Create records a release at its first stage with generation one.
	Create(ctx context.Context, state domain.RolloutState) error
	// Transition atomically applies next when the stored generation still equals
	// transition.FromGeneration and the lease is held by next.LeaseOwner; it records
	// the transition row and returns ErrRevisionConflict on a lost race.
	Transition(ctx context.Context, transition domain.RolloutTransition, next domain.RolloutState) error
	// Live lists releases whose stage still accepts controller decisions.
	Live(ctx context.Context) ([]domain.RolloutState, error)
	// RecordBypass persists an approved evidence-gate waiver.
	RecordBypass(ctx context.Context, bypass domain.RolloutBypass) error
	// Bypasses returns every recorded waiver for one release.
	Bypasses(ctx context.Context, platformVersion string) ([]domain.RolloutBypass, error)
}

// RolloutLease serializes controller decisions per release across replicas.
type RolloutLease interface {
	// Acquire takes or renews the release lease for owner; false when another owner holds it.
	Acquire(ctx context.Context, platformVersion, owner string, ttl time.Duration) (bool, error)
	// Release drops the lease when owner still holds it.
	Release(ctx context.Context, platformVersion, owner string) error
}

// RolloutOperatorControls are the audited manual levers over an automated rollout.
type RolloutOperatorControls interface {
	// Pause pins traffic where it is and stops automatic advancement.
	Pause(ctx context.Context, platformVersion, actor, reason string) error
	// Resume lets the controller evaluate and advance again.
	Resume(ctx context.Context, platformVersion, actor, reason string) error
	// Bypass records an approved waiver of one stage's inconclusive-evidence and minimum gates.
	Bypass(ctx context.Context, bypass domain.RolloutBypass) error
	// Quarantine marks a release unfit for any assignment until explicitly cleared.
	Quarantine(ctx context.Context, platformVersion, actor, reason string) error
}

// PausableRolloutController is a rollout controller with operator controls.
type PausableRolloutController interface {
	PlatformReleaseRolloutController
	RolloutOperatorControls
}

// RolloutTicker drives one evaluation cycle over every live release.
type RolloutTicker interface {
	// Tick evaluates health per live release and applies advance, pause, or rollback.
	Tick(ctx context.Context) error
}

// ShadowTrafficSampler mirrors a bounded share of authenticated, redacted requests
// to a shadow release; the copy never produces user-visible output or side effects.
type ShadowTrafficSampler interface {
	// Sample reports whether this request is mirrored to platformVersion.
	Sample(ctx context.Context, platformVersion string, request domain.TurnRequest) (bool, error)
	// Amplification returns shadow-to-live request ratio observed in the current window.
	Amplification(ctx context.Context, platformVersion string) (float64, error)
}

// ReleaseBundleStore persists the signed component set a platform release certifies.
type ReleaseBundleStore interface {
	// Put stores a bundle once; the digest is recomputed and must match.
	Put(ctx context.Context, bundle domain.ReleaseBundle) error
	// Get returns the bundle for one platform version, digest verified.
	Get(ctx context.Context, platformVersion string) (domain.ReleaseBundle, error)
}

// ConversationReleaseStore persists the immutable release identities of a conversation.
type ConversationReleaseStore interface {
	// Get returns both identities; ErrNotFound when the conversation has none yet.
	Get(ctx context.Context, tenantID int64, conversationID string) (domain.ConversationRelease, error)
	// Put records the platform identity once against the conversation's persisted
	// agent version; a mismatch or a second Put fails with ErrConflict.
	Put(ctx context.Context, release domain.ConversationRelease) error
	// Migrate moves a conversation whose release was withdrawn onto a safe
	// release at a turn boundary; the agent version never changes.
	Migrate(ctx context.Context, tenantID int64, conversationID, platformVersion string, at time.Time) error
}

// TenantPlatformReleaseResolver picks the platform release new conversations of a tenant start on.
type TenantPlatformReleaseResolver interface {
	// Current returns the live release for one new conversation of the tenant.
	Current(ctx context.Context, tenantID int64, conversationID string) (string, error)
}

// VersionPinStore persists agent version pins.
type VersionPinStore interface {
	// Put stores a pin and returns its id.
	Put(ctx context.Context, pin domain.VersionPin) (int64, error)
	// Active returns pins effective at the given instant, compliance scope first.
	Active(ctx context.Context, tenantID int64, agentID string, at time.Time) ([]domain.VersionPin, error)
	// Expire ends a pin now; audited by the caller.
	Expire(ctx context.Context, tenantID int64, pinID int64, at time.Time) error
}

// ExperimentCohortResolver assigns a subject to at most one experiment version.
type ExperimentCohortResolver interface {
	// Resolve returns the matching cohort and true, or false when the subject stays on the default.
	Resolve(ctx context.Context, tenantID int64, agentID, subjectKey string) (domain.ExperimentCohort, bool, error)
}

// PlatformAliasSwitcher points ring traffic at a release; idempotent by construction.
type PlatformAliasSwitcher interface {
	// Point directs new assignments in ring to platformVersion.
	Point(ctx context.Context, ring, platformVersion string) error
}

// CapacityRestorer returns serving capacity to the release traffic fell back to.
type CapacityRestorer interface {
	// Restore idempotently re-establishes capacity for platformVersion after a rollback.
	Restore(ctx context.Context, platformVersion string) error
}

// AlertOwnershipChecker confirms a release has a paged owner before it can be rolled back unattended.
type AlertOwnershipChecker interface {
	// Owner returns the on-call owner for the release's alerts.
	Owner(ctx context.Context, platformVersion string) (string, error)
}

// RollbackDrill rehearses a rollback against a release without live traffic.
type RollbackDrill interface {
	// Run performs the drill and returns every check it made.
	Run(ctx context.Context, platformVersion string) (domain.RollbackDrillReport, error)
}

// SyntheticProber runs release probes and holdout traffic outside user sessions.
type SyntheticProber interface {
	// Probe executes the configured probes against platformVersion.
	Probe(ctx context.Context, platformVersion string) ([]domain.SyntheticProbeResult, error)
}
