package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Bypass scopes name the gate a waiver removes.
const (
	BypassScopeEvidence = "evidence"
	BypassScopeMinimums = "minimums"
	BypassScopeAll      = "all"
)

const (
	actorController = "rollout-controller"
	actorOperator   = "operator"
)

// rolloutOrder is the forward progression; stages absent from the plan are skipped.
var rolloutOrder = []domain.RolloutStage{
	domain.StageBuild, domain.StageOfflineReplay, domain.StageShadow, domain.StageInternalCanary,
	domain.StageTenantCanary, domain.StageRegionalRamp, domain.StageGlobalDefault, domain.StageRetired,
}

// StagePolicy is the ring, traffic share, and evidence minimums of one stage.
type StagePolicy struct {
	Ring              string
	TrafficPercentage int
	MinSamples        int64
	MinDuration       time.Duration
}

// RolloutPlan configures the stages a release passes through, in rolloutOrder.
type RolloutPlan map[domain.RolloutStage]StagePolicy

// DefaultRolloutPlan is the reference progression with conservative minimums.
func DefaultRolloutPlan() RolloutPlan {
	return RolloutPlan{
		domain.StageBuild:          {},
		domain.StageOfflineReplay:  {MinSamples: 100},
		domain.StageShadow:         {TrafficPercentage: 5, MinSamples: 1_000, MinDuration: time.Hour},
		domain.StageInternalCanary: {Ring: "internal", TrafficPercentage: 100, MinSamples: 500, MinDuration: time.Hour},
		domain.StageTenantCanary:   {Ring: "canary", TrafficPercentage: 10, MinSamples: 5_000, MinDuration: 6 * time.Hour},
		domain.StageRegionalRamp:   {Ring: "regional", TrafficPercentage: 50, MinSamples: 20_000, MinDuration: 24 * time.Hour},
		domain.StageGlobalDefault:  {Ring: "global", TrafficPercentage: 100},
	}
}

// GuardrailPolicy tunes how unhealthy verdicts turn into rollbacks.
type GuardrailPolicy struct {
	// HardMetrics roll back and quarantine on the first unhealthy verdict.
	HardMetrics map[string]bool
	// SoftBreachWindows is how many consecutive soft breaches roll back.
	SoftBreachWindows int
	// HealthyToClear is how many consecutive healthy verdicts clear the soft counter.
	HealthyToClear int
	// Cooldown blocks advancement for this long after the last breach.
	Cooldown time.Duration
}

// DefaultGuardrailPolicy names the q65 hard guardrails and a three-window soft rule.
func DefaultGuardrailPolicy() GuardrailPolicy {
	return GuardrailPolicy{
		HardMetrics: map[string]bool{
			"isolation_breach": true, "safety_severe": true, "corrupt_output": true, "availability": true,
		},
		SoftBreachWindows: 3,
		HealthyToClear:    2,
		Cooldown:          30 * time.Minute,
	}
}

// RolloutController drives platform releases through the staged state machine
// under a per-release lease and a generation CAS, auditing every change.
type RolloutController struct {
	States contract.RolloutStateStore
	Lease  contract.RolloutLease
	Audit  contract.AuditSink
	// Health is preferred; Legacy maps true/false/error to healthy/unhealthy/inconclusive.
	Health contract.DetailedRolloutHealthEvaluator
	Legacy contract.RolloutHealthEvaluator
	// Bundles supplies the certified rollback target; nil falls back to the current default.
	Bundles contract.ReleaseBundleStore
	// Aliases and Capacity are the rollback hooks; nil skips them.
	Aliases  contract.PlatformAliasSwitcher
	Capacity contract.CapacityRestorer
	// Shadow is required when the plan contains the shadow stage.
	Shadow                 contract.ShadowTrafficSampler
	MaxShadowAmplification float64
	Plan                   RolloutPlan
	Guardrails             GuardrailPolicy
	Owner                  string
	LeaseTTL               time.Duration
	Now                    func() time.Time
}

var (
	_ contract.PausableRolloutController = (*RolloutController)(nil)
	_ contract.RolloutTicker             = (*RolloutController)(nil)
)

func (controller *RolloutController) validate() error {
	switch {
	case controller.States == nil || controller.Lease == nil || controller.Audit == nil:
		return fmt.Errorf("rollout controller: state store, lease, and audit sink are required")
	case controller.Health == nil && controller.Legacy == nil:
		return fmt.Errorf("rollout controller: a health evaluator is required")
	case len(controller.Plan) == 0:
		return fmt.Errorf("%w: rollout plan is required", domain.ErrValidation)
	case strings.TrimSpace(controller.Owner) == "" || controller.LeaseTTL <= 0:
		return fmt.Errorf("%w: lease owner and positive lease ttl are required", domain.ErrValidation)
	case controller.Guardrails.SoftBreachWindows <= 0 || controller.Guardrails.HealthyToClear <= 0 || controller.Guardrails.Cooldown < 0:
		return fmt.Errorf("%w: soft breach windows and healthy-to-clear must be positive", domain.ErrValidation)
	}
	if _, shadow := controller.Plan[domain.StageShadow]; shadow && (controller.Shadow == nil || controller.MaxShadowAmplification <= 0) {
		return fmt.Errorf("%w: shadow stage requires a sampler and a positive amplification bound", domain.ErrValidation)
	}
	for stage, policy := range controller.Plan {
		if policy.TrafficPercentage < 0 || policy.TrafficPercentage > 100 || policy.MinSamples < 0 || policy.MinDuration < 0 {
			return fmt.Errorf("%w: stage %s has an invalid policy", domain.ErrValidation, stage)
		}
	}
	return nil
}

func (controller *RolloutController) now() time.Time {
	if controller.Now != nil {
		return controller.Now()
	}
	return time.Now()
}

// Start records a release at the build stage with generation one.
func (controller *RolloutController) Start(ctx context.Context, target domain.RolloutTarget) error {
	if err := controller.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(target.PlatformVersion) == "" {
		return fmt.Errorf("%w: platform version is required", domain.ErrValidation)
	}
	first := controller.firstStage()
	state := controller.stateFor(target.PlatformVersion, first, controller.now())
	if err := controller.States.Create(ctx, state); err != nil {
		return err
	}
	return controller.audit(ctx, domain.RolloutTransition{
		PlatformVersion: target.PlatformVersion, From: first, To: first, FromGeneration: 0,
		Actor: actorOperator, Reason: "rollout started", OccurredAt: state.StageStartedAt,
	})
}

// Advance applies one evaluation cycle and fails when the gates hold the release.
func (controller *RolloutController) Advance(ctx context.Context, platformVersion string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	return controller.withLease(ctx, platformVersion, func(state domain.RolloutState) error {
		if state.Paused {
			return fmt.Errorf("%w: rollout %s is paused: %s", domain.ErrConflict, platformVersion, state.PauseReason)
		}
		outcome, err := controller.evaluate(ctx, state)
		if err != nil {
			return err
		}
		if outcome != domain.RolloutHealthy {
			return fmt.Errorf("%w: rollout %s did not advance: %s", domain.ErrNotReady, platformVersion, outcome)
		}
		return nil
	})
}

// Halt is the operator pause.
func (controller *RolloutController) Halt(ctx context.Context, platformVersion, reason string) error {
	return controller.Pause(ctx, platformVersion, actorOperator, reason)
}

// Rollback withdraws a release from new assignments; it is idempotent.
func (controller *RolloutController) Rollback(ctx context.Context, platformVersion string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	return controller.withLease(ctx, platformVersion, func(state domain.RolloutState) error {
		return controller.rollback(ctx, state, actorOperator, "operator rollback", false)
	})
}

func (controller *RolloutController) Pause(ctx context.Context, platformVersion, actor, reason string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: pause reason is required", domain.ErrValidation)
	}
	return controller.withLease(ctx, platformVersion, func(state domain.RolloutState) error {
		if state.Paused {
			return nil
		}
		next := state
		next.Paused, next.PauseReason = true, reason
		return controller.transition(ctx, state, next, actor, reason)
	})
}

func (controller *RolloutController) Resume(ctx context.Context, platformVersion, actor, reason string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	return controller.withLease(ctx, platformVersion, func(state domain.RolloutState) error {
		if !state.Paused {
			return nil
		}
		next := state
		next.Paused, next.PauseReason = false, ""
		return controller.transition(ctx, state, next, actor, "resume: "+reason)
	})
}

// Bypass records an approved waiver; approval must come from someone other than the requester.
func (controller *RolloutController) Bypass(ctx context.Context, bypass domain.RolloutBypass) error {
	if err := controller.validate(); err != nil {
		return err
	}
	now := controller.now()
	switch {
	case strings.TrimSpace(bypass.PlatformVersion) == "" || strings.TrimSpace(bypass.Reason) == "":
		return fmt.Errorf("%w: bypass platform version and reason are required", domain.ErrValidation)
	case strings.TrimSpace(bypass.RequestedBy) == "" || strings.TrimSpace(bypass.ApprovedBy) == "" || bypass.RequestedBy == bypass.ApprovedBy:
		return fmt.Errorf("%w: bypass needs an approver distinct from the requester", domain.ErrForbidden)
	case bypass.Scope != BypassScopeEvidence && bypass.Scope != BypassScopeMinimums && bypass.Scope != BypassScopeAll:
		return fmt.Errorf("%w: bypass scope must be evidence, minimums, or all", domain.ErrValidation)
	case !bypass.ExpiresAt.After(now):
		return fmt.Errorf("%w: bypass must expire in the future", domain.ErrValidation)
	}
	if _, ok := controller.Plan[bypass.Stage]; !ok {
		return fmt.Errorf("%w: bypass stage %q is not in the plan", domain.ErrValidation, bypass.Stage)
	}
	bypass.CreatedAt = now
	if err := controller.States.RecordBypass(ctx, bypass); err != nil {
		return err
	}
	return controller.audit(ctx, domain.RolloutTransition{
		PlatformVersion: bypass.PlatformVersion, From: bypass.Stage, To: bypass.Stage, Actor: bypass.ApprovedBy,
		Reason: fmt.Sprintf("bypass %s requested by %s: %s", bypass.Scope, bypass.RequestedBy, bypass.Reason), OccurredAt: now,
	})
}

func (controller *RolloutController) Quarantine(ctx context.Context, platformVersion, actor, reason string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	return controller.withLease(ctx, platformVersion, func(state domain.RolloutState) error {
		return controller.quarantine(ctx, state, actor, reason)
	})
}

// Tick evaluates every live release whose lease this controller can take.
func (controller *RolloutController) Tick(ctx context.Context) error {
	if err := controller.validate(); err != nil {
		return err
	}
	live, err := controller.States.Live(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range live {
		if candidate.Paused {
			continue
		}
		err := controller.withLease(ctx, candidate.PlatformVersion, func(state domain.RolloutState) error {
			if state.Paused || isTerminal(state.Stage) {
				return nil
			}
			_, err := controller.evaluate(ctx, state)
			return err
		})
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			failures = append(failures, fmt.Errorf("tick %s: %w", candidate.PlatformVersion, err))
		}
	}
	return errors.Join(failures...)
}

// evaluate applies the health verdict to one leased state and reports the
// verdict that drove the decision.
func (controller *RolloutController) evaluate(ctx context.Context, state domain.RolloutState) (domain.RolloutVerdict, error) {
	if isTerminal(state.Stage) {
		return "", fmt.Errorf("%w: rollout %s is %s", domain.ErrConflict, state.PlatformVersion, state.Stage)
	}
	now := controller.now()
	health, err := controller.health(ctx, state)
	if err != nil {
		return "", err
	}
	waived, err := controller.activeBypass(ctx, state, now)
	if err != nil {
		return "", err
	}
	next := state
	switch health.Verdict {
	case domain.RolloutUnhealthy:
		return domain.RolloutUnhealthy, controller.breach(ctx, state, health, now)
	case domain.RolloutInconclusive:
		if !waived[BypassScopeEvidence] {
			next.Paused, next.PauseReason = true, "inconclusive: "+describe(health)
			return domain.RolloutInconclusive, controller.transition(ctx, state, next, actorController, next.PauseReason)
		}
	case domain.RolloutHealthy:
		next.ConsecutiveHealthy++
		if next.ConsecutiveHealthy >= controller.Guardrails.HealthyToClear {
			next.ConsecutiveBreaches = 0
		}
	default:
		return "", fmt.Errorf("%w: unknown rollout verdict %q", domain.ErrValidation, health.Verdict)
	}
	if !waived[BypassScopeMinimums] {
		if hold := controller.holdReason(state, health, now); hold != "" {
			if next != state {
				return domain.RolloutInconclusive, controller.transition(ctx, state, next, actorController, hold)
			}
			return domain.RolloutInconclusive, nil
		}
	}
	if state.Stage == domain.StageShadow {
		ratio, err := controller.Shadow.Amplification(ctx, state.PlatformVersion)
		if err != nil {
			return "", fmt.Errorf("shadow amplification: %w", err)
		}
		if ratio > controller.MaxShadowAmplification {
			next.Paused, next.PauseReason = true, fmt.Sprintf("shadow amplification %.3f exceeds bound %.3f", ratio, controller.MaxShadowAmplification)
			return domain.RolloutInconclusive, controller.transition(ctx, state, next, actorController, next.PauseReason)
		}
	}
	return domain.RolloutHealthy, controller.advance(ctx, state, next, health)
}

func (controller *RolloutController) holdReason(state domain.RolloutState, health domain.RolloutHealth, now time.Time) string {
	switch {
	case health.Samples < state.MinSamples:
		return fmt.Sprintf("waiting for samples %d/%d", health.Samples, state.MinSamples)
	case now.Sub(state.StageStartedAt) < state.MinDuration:
		return fmt.Sprintf("waiting for stage duration %s/%s", now.Sub(state.StageStartedAt).Round(time.Second), state.MinDuration)
	case !state.LastBreachAt.IsZero() && now.Sub(state.LastBreachAt) < controller.Guardrails.Cooldown:
		return fmt.Sprintf("cooling down after breach at %s", state.LastBreachAt.Format(time.RFC3339))
	}
	return ""
}

func (controller *RolloutController) advance(ctx context.Context, state, next domain.RolloutState, health domain.RolloutHealth) error {
	target := controller.nextStage(state.Stage)
	if target == "" {
		return fmt.Errorf("%w: rollout %s has no stage after %s", domain.ErrConflict, state.PlatformVersion, state.Stage)
	}
	if target == domain.StageGlobalDefault {
		if err := controller.retireOtherDefaults(ctx, state.PlatformVersion); err != nil {
			return err
		}
	}
	promoted := controller.stateFor(state.PlatformVersion, target, controller.now())
	promoted.Generation, promoted.LeaseOwner, promoted.LeaseUntil = next.Generation, next.LeaseOwner, next.LeaseUntil
	reason := fmt.Sprintf("healthy: samples=%d effect=%.4f confidence=%.3f", health.Samples, health.EffectSize, health.Confidence)
	return controller.transition(ctx, state, promoted, actorController, reason)
}

func (controller *RolloutController) breach(ctx context.Context, state domain.RolloutState, health domain.RolloutHealth, now time.Time) error {
	metric := health.BreachedMetric
	if metric == "" {
		metric = health.Breach.Metric
	}
	hard := health.Breach.Kind == domain.BreachHard || controller.Guardrails.HardMetrics[metric]
	if hard {
		reason := "hard guardrail breach: " + metric
		if err := controller.rollback(ctx, state, actorController, reason, true); err != nil {
			return err
		}
		return nil
	}
	next := state
	next.ConsecutiveBreaches++
	next.ConsecutiveHealthy = 0
	next.LastBreachAt = now
	if next.ConsecutiveBreaches >= controller.Guardrails.SoftBreachWindows {
		return controller.rollback(ctx, state, actorController,
			fmt.Sprintf("soft guardrail %s breached %d consecutive windows", metric, next.ConsecutiveBreaches), false)
	}
	return controller.transition(ctx, state, next, actorController,
		fmt.Sprintf("soft breach %s %d/%d", metric, next.ConsecutiveBreaches, controller.Guardrails.SoftBreachWindows))
}

// rollback stops new assignments and, when quarantine is set, marks the release unfit;
// already withdrawn releases are left untouched.
func (controller *RolloutController) rollback(ctx context.Context, state domain.RolloutState, actor, reason string, quarantine bool) error {
	if state.Stage == domain.StageQuarantined || (state.Stage == domain.StageRolledBack && !quarantine) {
		return nil
	}
	if state.Stage != domain.StageRolledBack {
		target, err := controller.rollbackTarget(ctx, state.PlatformVersion)
		if err != nil {
			return err
		}
		// Move routing and restore fallback capacity before declaring the
		// rollout rolled back. Both collaborators are idempotent, so a failed
		// attempt can safely retry while the durable state remains live.
		if controller.Aliases != nil && target != "" {
			if err := controller.Aliases.Point(ctx, state.Ring, target); err != nil {
				return fmt.Errorf("rollback %s: point alias: %w", state.PlatformVersion, err)
			}
		}
		if controller.Capacity != nil && target != "" {
			if err := controller.Capacity.Restore(ctx, target); err != nil {
				return fmt.Errorf("rollback %s: restore capacity: %w", state.PlatformVersion, err)
			}
		}
		next := state
		next.Stage, next.TrafficPercentage, next.Paused, next.PauseReason = domain.StageRolledBack, 0, false, ""
		next.StageStartedAt = controller.now()
		if err := controller.transition(ctx, state, next, actor, reason+" (rollback target "+orNone(target)+")"); err != nil {
			return err
		}
		state = next
		state.Generation++
	}
	if quarantine {
		return controller.quarantine(ctx, state, actor, reason)
	}
	return nil
}

func (controller *RolloutController) quarantine(ctx context.Context, state domain.RolloutState, actor, reason string) error {
	if state.Stage == domain.StageQuarantined {
		return nil
	}
	next := state
	next.Stage, next.TrafficPercentage, next.Paused, next.PauseReason = domain.StageQuarantined, 0, false, ""
	next.StageStartedAt = controller.now()
	return controller.transition(ctx, state, next, actor, "quarantine: "+reason)
}

func (controller *RolloutController) rollbackTarget(ctx context.Context, platformVersion string) (string, error) {
	if controller.Bundles != nil {
		bundle, err := controller.Bundles.Get(ctx, platformVersion)
		switch {
		case err == nil && bundle.RollbackTarget != "":
			return bundle.RollbackTarget, nil
		case err != nil && !errors.Is(err, domain.ErrNotFound):
			return "", fmt.Errorf("rollback target for %s: %w", platformVersion, err)
		}
	}
	live, err := controller.States.Live(ctx)
	if err != nil {
		return "", err
	}
	for _, candidate := range live {
		if candidate.Stage == domain.StageGlobalDefault && candidate.PlatformVersion != platformVersion {
			return candidate.PlatformVersion, nil
		}
	}
	return "", nil
}

func (controller *RolloutController) retireOtherDefaults(ctx context.Context, platformVersion string) error {
	live, err := controller.States.Live(ctx)
	if err != nil {
		return err
	}
	for _, other := range live {
		if other.Stage != domain.StageGlobalDefault || other.PlatformVersion == platformVersion {
			continue
		}
		err := controller.withLease(ctx, other.PlatformVersion, func(state domain.RolloutState) error {
			next := controller.stateFor(state.PlatformVersion, domain.StageRetired, controller.now())
			next.Generation, next.LeaseOwner, next.LeaseUntil = state.Generation, state.LeaseOwner, state.LeaseUntil
			return controller.transition(ctx, state, next, actorController, "superseded by "+platformVersion)
		})
		if err != nil {
			return fmt.Errorf("retire %s: %w", other.PlatformVersion, err)
		}
	}
	return nil
}

func (controller *RolloutController) health(ctx context.Context, state domain.RolloutState) (domain.RolloutHealth, error) {
	target := domain.RolloutTarget{PlatformVersion: state.PlatformVersion, TenantRing: state.Ring, Percentage: state.TrafficPercentage}
	if controller.Health != nil {
		health, err := controller.Health.Evaluate(ctx, target)
		if err != nil {
			return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "evaluation error: " + err.Error()}, nil
		}
		return health, nil
	}
	healthy, err := controller.Legacy.Healthy(ctx, target)
	switch {
	case err != nil:
		return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "evaluation error: " + err.Error()}, nil
	case healthy:
		return domain.RolloutHealth{Verdict: domain.RolloutHealthy, Samples: state.MinSamples}, nil
	}
	return domain.RolloutHealth{Verdict: domain.RolloutUnhealthy, BreachedMetric: "legacy"}, nil
}

func (controller *RolloutController) activeBypass(ctx context.Context, state domain.RolloutState, now time.Time) (map[string]bool, error) {
	bypasses, err := controller.States.Bypasses(ctx, state.PlatformVersion)
	if err != nil {
		return nil, err
	}
	waived := map[string]bool{}
	for _, bypass := range bypasses {
		if bypass.Stage != state.Stage || !bypass.ExpiresAt.After(now) || bypass.ApprovedBy == "" || bypass.ApprovedBy == bypass.RequestedBy {
			continue
		}
		if bypass.Scope == BypassScopeAll {
			waived[BypassScopeEvidence], waived[BypassScopeMinimums] = true, true
		} else {
			waived[bypass.Scope] = true
		}
	}
	return waived, nil
}

func (controller *RolloutController) withLease(ctx context.Context, platformVersion string, apply func(domain.RolloutState) error) error {
	acquired, err := controller.Lease.Acquire(ctx, platformVersion, controller.Owner, controller.LeaseTTL)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("%w: rollout %s is leased by another controller", domain.ErrConflict, platformVersion)
	}
	defer func() { _ = controller.Lease.Release(ctx, platformVersion, controller.Owner) }()
	state, err := controller.States.Get(ctx, platformVersion)
	if err != nil {
		return err
	}
	state.LeaseOwner = controller.Owner
	return apply(state)
}

// transition applies the CAS write, then audits; both must succeed.
func (controller *RolloutController) transition(ctx context.Context, from, next domain.RolloutState, actor, reason string) error {
	next.LeaseOwner = controller.Owner
	record := domain.RolloutTransition{
		PlatformVersion: from.PlatformVersion, From: from.Stage, To: next.Stage,
		FromGeneration: from.Generation, Actor: actor, Reason: reason, OccurredAt: controller.now(),
	}
	if err := controller.States.Transition(ctx, record, next); err != nil {
		return err
	}
	return controller.audit(ctx, record)
}

func (controller *RolloutController) audit(ctx context.Context, record domain.RolloutTransition) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode rollout audit: %w", err)
	}
	if err := controller.Audit.Record(ctx, domain.AuditEvent{Category: "rollout." + string(record.To), Payload: payload, OccurredAt: record.OccurredAt}); err != nil {
		return fmt.Errorf("audit rollout %s: %w", record.PlatformVersion, err)
	}
	return nil
}

func (controller *RolloutController) stateFor(platformVersion string, stage domain.RolloutStage, at time.Time) domain.RolloutState {
	policy := controller.Plan[stage]
	return domain.RolloutState{
		PlatformVersion: platformVersion, Stage: stage, Ring: policy.Ring, TrafficPercentage: policy.TrafficPercentage,
		Generation: 1, StageStartedAt: at, MinSamples: policy.MinSamples, MinDuration: policy.MinDuration,
	}
}

func (controller *RolloutController) firstStage() domain.RolloutStage {
	for _, stage := range rolloutOrder {
		if _, ok := controller.Plan[stage]; ok {
			return stage
		}
	}
	return domain.StageBuild
}

func (controller *RolloutController) nextStage(current domain.RolloutStage) domain.RolloutStage {
	seen := false
	for _, stage := range rolloutOrder {
		if stage == current {
			seen = true
			continue
		}
		if seen && (stage == domain.StageRetired || hasStage(controller.Plan, stage)) {
			return stage
		}
	}
	return ""
}

func hasStage(plan RolloutPlan, stage domain.RolloutStage) bool {
	_, ok := plan[stage]
	return ok
}

func isTerminal(stage domain.RolloutStage) bool {
	return stage == domain.StageRetired || stage == domain.StageRolledBack || stage == domain.StageQuarantined
}

func describe(health domain.RolloutHealth) string {
	if health.BreachedMetric != "" {
		return health.BreachedMetric
	}
	return fmt.Sprintf("samples=%d fresh_at=%s", health.Samples, health.TelemetryFreshAt.Format(time.RFC3339))
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
