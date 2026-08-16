package fake

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ContractTestExecutorFunc adapts a function to contract.ContractTestExecutor.
type ContractTestExecutorFunc func(context.Context, string, domain.ContractTestCase) (domain.TurnResult, error)

// Execute invokes the configured function.
func (function ContractTestExecutorFunc) Execute(ctx context.Context, platformVersion string, testCase domain.ContractTestCase) (domain.TurnResult, error) {
	return function(ctx, platformVersion, testCase)
}

// ContractAssertionEvaluatorFunc adapts a function to contract.ContractAssertionEvaluator.
type ContractAssertionEvaluatorFunc func(context.Context, domain.ContractTestCase, domain.TurnResult) ([]string, error)

// Evaluate invokes the configured function.
func (function ContractAssertionEvaluatorFunc) Evaluate(ctx context.Context, testCase domain.ContractTestCase, result domain.TurnResult) ([]string, error) {
	return function(ctx, testCase, result)
}

var _ contract.ContractTestExecutor = ContractTestExecutorFunc(nil)
var _ contract.ContractAssertionEvaluator = ContractAssertionEvaluatorFunc(nil)

// RolloutHealthEvaluatorFunc adapts a function to contract.RolloutHealthEvaluator.
type RolloutHealthEvaluatorFunc func(context.Context, domain.RolloutTarget) (bool, error)

// Healthy invokes the configured function.
func (function RolloutHealthEvaluatorFunc) Healthy(ctx context.Context, target domain.RolloutTarget) (bool, error) {
	return function(ctx, target)
}

// DetailedRolloutHealthEvaluatorFunc adapts a function to contract.DetailedRolloutHealthEvaluator.
type DetailedRolloutHealthEvaluatorFunc func(context.Context, domain.RolloutTarget) (domain.RolloutHealth, error)

// Evaluate invokes the configured function.
func (function DetailedRolloutHealthEvaluatorFunc) Evaluate(ctx context.Context, target domain.RolloutTarget) (domain.RolloutHealth, error) {
	return function(ctx, target)
}

// RecordingAuditSink records every event it receives.
type RecordingAuditSink struct {
	Events []domain.AuditEvent
	Err    error
}

// Record appends the event unless a failure is configured.
func (sink *RecordingAuditSink) Record(_ context.Context, event domain.AuditEvent) error {
	if sink.Err != nil {
		return sink.Err
	}
	sink.Events = append(sink.Events, event)
	return nil
}

// Categories returns the recorded event categories in order.
func (sink *RecordingAuditSink) Categories() []string {
	categories := make([]string, 0, len(sink.Events))
	for _, event := range sink.Events {
		categories = append(categories, event.Category)
	}
	return categories
}

// RolloutStateStore is an in-memory RolloutStateStore with generation CAS.
type RolloutStateStore struct {
	States      map[string]domain.RolloutState
	Transitions []domain.RolloutTransition
	BypassRows  map[string][]domain.RolloutBypass
}

// NewRolloutStateStore returns an empty in-memory state store.
func NewRolloutStateStore() *RolloutStateStore {
	return &RolloutStateStore{States: map[string]domain.RolloutState{}, BypassRows: map[string][]domain.RolloutBypass{}}
}

// Get returns the stored state.
func (store *RolloutStateStore) Get(_ context.Context, platformVersion string) (domain.RolloutState, error) {
	state, ok := store.States[platformVersion]
	if !ok {
		return domain.RolloutState{}, fmt.Errorf("%w: rollout state %s", domain.ErrNotFound, platformVersion)
	}
	return state, nil
}

// Create stores a new state at generation one.
func (store *RolloutStateStore) Create(_ context.Context, state domain.RolloutState) error {
	if _, exists := store.States[state.PlatformVersion]; exists {
		return fmt.Errorf("%w: rollout state %s", domain.ErrConflict, state.PlatformVersion)
	}
	state.Generation = 1
	store.States[state.PlatformVersion] = state
	return nil
}

// Transition applies the CAS write and records the transition.
func (store *RolloutStateStore) Transition(_ context.Context, transition domain.RolloutTransition, next domain.RolloutState) error {
	current, ok := store.States[next.PlatformVersion]
	if !ok {
		return fmt.Errorf("%w: rollout state %s", domain.ErrNotFound, next.PlatformVersion)
	}
	if current.Generation != transition.FromGeneration || current.LeaseOwner != next.LeaseOwner {
		return fmt.Errorf("%w: rollout %s generation %d", domain.ErrRevisionConflict, next.PlatformVersion, transition.FromGeneration)
	}
	next.Generation = current.Generation + 1
	store.States[next.PlatformVersion] = next
	store.Transitions = append(store.Transitions, transition)
	return nil
}

// Live returns every nonterminal state.
func (store *RolloutStateStore) Live(context.Context) ([]domain.RolloutState, error) {
	live := make([]domain.RolloutState, 0, len(store.States))
	for _, state := range store.States {
		switch state.Stage {
		case domain.StageRetired, domain.StageRolledBack, domain.StageQuarantined:
			continue
		}
		live = append(live, state)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].PlatformVersion < live[j].PlatformVersion })
	return live, nil
}

// RecordBypass stores an approved waiver.
func (store *RolloutStateStore) RecordBypass(_ context.Context, bypass domain.RolloutBypass) error {
	store.BypassRows[bypass.PlatformVersion] = append(store.BypassRows[bypass.PlatformVersion], bypass)
	return nil
}

// Bypasses returns the recorded waivers for a release.
func (store *RolloutStateStore) Bypasses(_ context.Context, platformVersion string) ([]domain.RolloutBypass, error) {
	return store.BypassRows[platformVersion], nil
}

// Acquire takes the lease unless another owner holds it.
func (store *RolloutStateStore) Acquire(_ context.Context, platformVersion, owner string, ttl time.Duration) (bool, error) {
	state, ok := store.States[platformVersion]
	if !ok {
		return false, fmt.Errorf("%w: rollout state %s", domain.ErrNotFound, platformVersion)
	}
	if state.LeaseOwner != "" && state.LeaseOwner != owner {
		return false, nil
	}
	state.LeaseOwner = owner
	store.States[platformVersion] = state
	return true, nil
}

// Release drops the lease held by owner.
func (store *RolloutStateStore) Release(_ context.Context, platformVersion, owner string) error {
	state, ok := store.States[platformVersion]
	if ok && state.LeaseOwner == owner {
		state.LeaseOwner = ""
		store.States[platformVersion] = state
	}
	return nil
}

// ReleaseBundleStore is an in-memory ReleaseBundleStore.
type ReleaseBundleStore struct {
	Bundles map[string]domain.ReleaseBundle
}

// Put stores a bundle.
func (store *ReleaseBundleStore) Put(_ context.Context, bundle domain.ReleaseBundle) error {
	if store.Bundles == nil {
		store.Bundles = map[string]domain.ReleaseBundle{}
	}
	store.Bundles[bundle.PlatformVersion] = bundle
	return nil
}

// Get returns a stored bundle.
func (store *ReleaseBundleStore) Get(_ context.Context, platformVersion string) (domain.ReleaseBundle, error) {
	bundle, ok := store.Bundles[platformVersion]
	if !ok {
		return domain.ReleaseBundle{}, fmt.Errorf("%w: release bundle %s", domain.ErrNotFound, platformVersion)
	}
	return bundle, nil
}

// ConversationReleaseStore is an in-memory ConversationReleaseStore.
type ConversationReleaseStore struct {
	Releases map[string]domain.ConversationRelease
}

func conversationKey(tenantID int64, conversationID string) string {
	return fmt.Sprintf("%d|%s", tenantID, conversationID)
}

// Get returns the stored identities of a conversation.
func (store *ConversationReleaseStore) Get(_ context.Context, tenantID int64, conversationID string) (domain.ConversationRelease, error) {
	release, ok := store.Releases[conversationKey(tenantID, conversationID)]
	if !ok {
		return domain.ConversationRelease{}, fmt.Errorf("%w: release for conversation %s", domain.ErrNotFound, conversationID)
	}
	return release, nil
}

// Put records the identities once.
func (store *ConversationReleaseStore) Put(_ context.Context, release domain.ConversationRelease) error {
	if store.Releases == nil {
		store.Releases = map[string]domain.ConversationRelease{}
	}
	key := conversationKey(release.TenantID, release.ConversationID)
	if existing, ok := store.Releases[key]; ok {
		if existing.PlatformVersion != release.PlatformVersion || existing.AgentVersion != release.AgentVersion {
			return fmt.Errorf("%w: conversation %s already has a release", domain.ErrConflict, release.ConversationID)
		}
		return nil
	}
	store.Releases[key] = release
	return nil
}

// Migrate moves a conversation onto another platform release.
func (store *ConversationReleaseStore) Migrate(_ context.Context, tenantID int64, conversationID, platformVersion string, at time.Time) error {
	key := conversationKey(tenantID, conversationID)
	release, ok := store.Releases[key]
	if !ok {
		return fmt.Errorf("%w: release for conversation %s", domain.ErrNotFound, conversationID)
	}
	release.PlatformVersion, release.ResolvedAt = platformVersion, at
	store.Releases[key] = release
	return nil
}

// VersionPinStore is an in-memory VersionPinStore.
type VersionPinStore struct {
	Pins []domain.VersionPin
}

// Put appends a pin and returns its id.
func (store *VersionPinStore) Put(_ context.Context, pin domain.VersionPin) (int64, error) {
	pin.ID = int64(len(store.Pins) + 1)
	store.Pins = append(store.Pins, pin)
	return pin.ID, nil
}

// Active returns pins effective at the given instant, compliance scope first.
func (store *VersionPinStore) Active(_ context.Context, tenantID int64, agentID string, at time.Time) ([]domain.VersionPin, error) {
	active := make([]domain.VersionPin, 0, len(store.Pins))
	for _, pin := range store.Pins {
		if pin.TenantID != tenantID || pin.AgentID != agentID || pin.EffectiveAt.After(at) {
			continue
		}
		if !pin.ExpiresAt.IsZero() && !pin.ExpiresAt.After(at) {
			continue
		}
		active = append(active, pin)
	}
	sort.SliceStable(active, func(i, j int) bool {
		return active[i].Scope == domain.PinScopeCompliance && active[j].Scope != domain.PinScopeCompliance
	})
	return active, nil
}

// Expire ends a pin at the given instant.
func (store *VersionPinStore) Expire(_ context.Context, tenantID, pinID int64, at time.Time) error {
	for index, pin := range store.Pins {
		if pin.TenantID == tenantID && pin.ID == pinID {
			store.Pins[index].ExpiresAt = at
			return nil
		}
	}
	return fmt.Errorf("%w: version pin %d", domain.ErrNotFound, pinID)
}

// ExperimentCohortResolverFunc adapts a function to contract.ExperimentCohortResolver.
type ExperimentCohortResolverFunc func(context.Context, int64, string, string) (domain.ExperimentCohort, bool, error)

// Resolve invokes the configured function.
func (function ExperimentCohortResolverFunc) Resolve(ctx context.Context, tenantID int64, agentID, subjectKey string) (domain.ExperimentCohort, bool, error) {
	return function(ctx, tenantID, agentID, subjectKey)
}

// AgentDeploymentStore is an in-memory AgentDeploymentStore.
type AgentDeploymentStore struct {
	Deployments map[string]domain.AgentDeployment
	Previous    map[string]string
}

func deploymentKey(tenantID int64, agentID string) string {
	return fmt.Sprintf("%d|%s", tenantID, agentID)
}

// Get returns the stored deployment.
func (store *AgentDeploymentStore) Get(_ context.Context, tenantID int64, agentID string) (domain.AgentDeployment, error) {
	deployment, ok := store.Deployments[deploymentKey(tenantID, agentID)]
	if !ok {
		return domain.AgentDeployment{}, fmt.Errorf("%w: deployment for agent %s", domain.ErrNotFound, agentID)
	}
	return deployment, nil
}

// SetCanary assigns canary traffic.
func (store *AgentDeploymentStore) SetCanary(ctx context.Context, tenantID int64, agentID, version string, percentage int) error {
	deployment, err := store.Get(ctx, tenantID, agentID)
	if err != nil {
		return err
	}
	deployment.CanaryVersion, deployment.CanaryPercentage = version, percentage
	store.Deployments[deploymentKey(tenantID, agentID)] = deployment
	return nil
}

// Promote makes a version stable.
func (store *AgentDeploymentStore) Promote(ctx context.Context, tenantID int64, agentID, version string) error {
	deployment, err := store.Get(ctx, tenantID, agentID)
	if err != nil {
		return err
	}
	deployment.StableVersion, deployment.CanaryVersion, deployment.CanaryPercentage = version, "", 0
	store.Deployments[deploymentKey(tenantID, agentID)] = deployment
	return nil
}

// RestorePrevious clears the canary or restores the configured previous version.
func (store *AgentDeploymentStore) RestorePrevious(ctx context.Context, tenantID int64, agentID string) (string, error) {
	deployment, err := store.Get(ctx, tenantID, agentID)
	if err != nil {
		return "", err
	}
	if deployment.CanaryVersion != "" {
		deployment.CanaryVersion, deployment.CanaryPercentage = "", 0
		store.Deployments[deploymentKey(tenantID, agentID)] = deployment
		return deployment.StableVersion, nil
	}
	previous, ok := store.Previous[deploymentKey(tenantID, agentID)]
	if !ok {
		return "", fmt.Errorf("%w: no version before %s", domain.ErrNotFound, deployment.StableVersion)
	}
	deployment.StableVersion = previous
	store.Deployments[deploymentKey(tenantID, agentID)] = deployment
	return previous, nil
}

// TenantPlatformReleaseResolverFunc adapts a function to contract.TenantPlatformReleaseResolver.
type TenantPlatformReleaseResolverFunc func(context.Context, int64, string) (string, error)

// Current invokes the configured function.
func (function TenantPlatformReleaseResolverFunc) Current(ctx context.Context, tenantID int64, conversationID string) (string, error) {
	return function(ctx, tenantID, conversationID)
}

// TurnCancellerFunc adapts a function to contract.TurnCanceller.
type TurnCancellerFunc func(context.Context, int64, string, string) error

// Cancel invokes the configured function.
func (function TurnCancellerFunc) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	return function(ctx, tenantID, requestID, reason)
}

// PlatformAliasSwitcher records every alias change.
type PlatformAliasSwitcher struct {
	Pointed map[string]string
	Err     error
}

// Point records the ring to release mapping.
func (switcher *PlatformAliasSwitcher) Point(_ context.Context, ring, platformVersion string) error {
	if switcher.Err != nil {
		return switcher.Err
	}
	if switcher.Pointed == nil {
		switcher.Pointed = map[string]string{}
	}
	switcher.Pointed[ring] = platformVersion
	return nil
}

// CapacityRestorer counts restore calls per release.
type CapacityRestorer struct {
	Restored map[string]int
	Err      error
}

// Restore records the call.
func (restorer *CapacityRestorer) Restore(_ context.Context, platformVersion string) error {
	if restorer.Err != nil {
		return restorer.Err
	}
	if restorer.Restored == nil {
		restorer.Restored = map[string]int{}
	}
	restorer.Restored[platformVersion]++
	return nil
}

// ShadowTrafficSampler reports fixed sampling and amplification.
type ShadowTrafficSampler struct {
	Sampled  bool
	Ratio    float64
	Err      error
	Requests int
}

// Sample records the call and returns the configured decision.
func (sampler *ShadowTrafficSampler) Sample(context.Context, string, domain.TurnRequest) (bool, error) {
	sampler.Requests++
	return sampler.Sampled, sampler.Err
}

// Amplification returns the configured ratio.
func (sampler *ShadowTrafficSampler) Amplification(context.Context, string) (float64, error) {
	return sampler.Ratio, sampler.Err
}

// AlertOwnershipCheckerFunc adapts a function to contract.AlertOwnershipChecker.
type AlertOwnershipCheckerFunc func(context.Context, string) (string, error)

// Owner invokes the configured function.
func (function AlertOwnershipCheckerFunc) Owner(ctx context.Context, platformVersion string) (string, error) {
	return function(ctx, platformVersion)
}

var (
	_ contract.RolloutHealthEvaluator         = RolloutHealthEvaluatorFunc(nil)
	_ contract.DetailedRolloutHealthEvaluator = DetailedRolloutHealthEvaluatorFunc(nil)
	_ contract.AuditSink                      = (*RecordingAuditSink)(nil)
	_ contract.RolloutStateStore              = (*RolloutStateStore)(nil)
	_ contract.RolloutLease                   = (*RolloutStateStore)(nil)
	_ contract.ReleaseBundleStore             = (*ReleaseBundleStore)(nil)
	_ contract.ConversationReleaseStore       = (*ConversationReleaseStore)(nil)
	_ contract.VersionPinStore                = (*VersionPinStore)(nil)
	_ contract.ExperimentCohortResolver       = ExperimentCohortResolverFunc(nil)
	_ contract.AgentDeploymentStore           = (*AgentDeploymentStore)(nil)
	_ contract.TenantPlatformReleaseResolver  = TenantPlatformReleaseResolverFunc(nil)
	_ contract.TurnCanceller                  = TurnCancellerFunc(nil)
	_ contract.PlatformAliasSwitcher          = (*PlatformAliasSwitcher)(nil)
	_ contract.CapacityRestorer               = (*CapacityRestorer)(nil)
	_ contract.ShadowTrafficSampler           = (*ShadowTrafficSampler)(nil)
	_ contract.AlertOwnershipChecker          = AlertOwnershipCheckerFunc(nil)
)
