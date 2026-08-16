package release

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

var testStart = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)

type controllerFixture struct {
	controller *RolloutController
	states     *fake.RolloutStateStore
	audit      *fake.RecordingAuditSink
	aliases    *fake.PlatformAliasSwitcher
	capacity   *fake.CapacityRestorer
	health     domain.RolloutHealth
	healthErr  error
	now        time.Time
}

func newControllerFixture(t *testing.T, stage domain.RolloutStage) *controllerFixture {
	t.Helper()
	fixture := &controllerFixture{
		states:   fake.NewRolloutStateStore(),
		audit:    &fake.RecordingAuditSink{},
		aliases:  &fake.PlatformAliasSwitcher{},
		capacity: &fake.CapacityRestorer{},
		now:      testStart,
		health:   domain.RolloutHealth{Verdict: domain.RolloutHealthy, Samples: 1_000_000},
	}
	fixture.controller = &RolloutController{
		States: fixture.states,
		Lease:  fixture.states,
		Audit:  fixture.audit,
		Health: fake.DetailedRolloutHealthEvaluatorFunc(func(context.Context, domain.RolloutTarget) (domain.RolloutHealth, error) {
			return fixture.health, fixture.healthErr
		}),
		Bundles:                &fake.ReleaseBundleStore{Bundles: map[string]domain.ReleaseBundle{"2026.08.1": {PlatformVersion: "2026.08.1", RollbackTarget: "2026.07.9"}}},
		Aliases:                fixture.aliases,
		Capacity:               fixture.capacity,
		Shadow:                 &fake.ShadowTrafficSampler{Ratio: 0.05},
		MaxShadowAmplification: 0.1,
		Plan:                   DefaultRolloutPlan(),
		Guardrails:             DefaultGuardrailPolicy(),
		Owner:                  "controller-a",
		LeaseTTL:               time.Minute,
		Now:                    func() time.Time { return fixture.now },
	}
	policy := fixture.controller.Plan[stage]
	if err := fixture.states.Create(context.Background(), domain.RolloutState{
		PlatformVersion: "2026.08.1", Stage: stage, Ring: policy.Ring, TrafficPercentage: policy.TrafficPercentage,
		StageStartedAt: testStart.Add(-48 * time.Hour), MinSamples: policy.MinSamples, MinDuration: policy.MinDuration,
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *controllerFixture) state(t *testing.T) domain.RolloutState {
	t.Helper()
	state, err := fixture.states.Get(context.Background(), "2026.08.1")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRolloutControllerAdvancesThroughStages(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); err != nil {
		t.Fatal(err)
	}
	state := fixture.state(t)
	if state.Stage != domain.StageRegionalRamp || state.Generation != 2 || state.Ring != "regional" || state.TrafficPercentage != 50 {
		t.Fatalf("state = %+v", state)
	}
	if len(fixture.states.Transitions) != 1 || fixture.states.Transitions[0].FromGeneration != 1 {
		t.Fatalf("transitions = %+v", fixture.states.Transitions)
	}
	if got := fixture.audit.Categories(); len(got) != 1 || got[0] != "rollout.regional_ramp" {
		t.Fatalf("audit = %v", got)
	}
}

func TestRolloutControllerHoldsOnMinimums(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*controllerFixture)
	}{
		{"insufficient samples", func(fixture *controllerFixture) {
			fixture.health = domain.RolloutHealth{Verdict: domain.RolloutHealthy, Samples: 1}
		}},
		{"stage too young", func(fixture *controllerFixture) {
			state := fixture.state(&testing.T{})
			state.StageStartedAt = fixture.now.Add(-time.Minute)
			fixture.states.States["2026.08.1"] = state
		}},
		{"cooldown after breach", func(fixture *controllerFixture) {
			state := fixture.states.States["2026.08.1"]
			state.LastBreachAt = fixture.now.Add(-time.Minute)
			fixture.states.States["2026.08.1"] = state
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newControllerFixture(t, domain.StageTenantCanary)
			testCase.prepare(fixture)
			err := fixture.controller.Advance(context.Background(), "2026.08.1")
			if !errors.Is(err, domain.ErrNotReady) {
				t.Fatalf("err = %v", err)
			}
			if stage := fixture.state(t).Stage; stage != domain.StageTenantCanary {
				t.Fatalf("stage = %s", stage)
			}
		})
	}
}

func TestRolloutControllerPausesOnInconclusiveEvidence(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	fixture.health = domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "telemetry stale"}
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
	state := fixture.state(t)
	if !state.Paused || state.Stage != domain.StageTenantCanary || state.PauseReason == "" {
		t.Fatalf("state = %+v", state)
	}
	// A paused rollout refuses to advance until an operator resumes it.
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("paused advance err = %v", err)
	}
	if err := fixture.controller.Resume(context.Background(), "2026.08.1", "operator-a", "telemetry restored"); err != nil {
		t.Fatal(err)
	}
	if fixture.state(t).Paused {
		t.Fatal("resume did not clear the pause")
	}
}

func TestRolloutControllerEvaluationErrorIsInconclusive(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	fixture.healthErr = errors.New("metrics backend unreachable")
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
	if !fixture.state(t).Paused {
		t.Fatal("evaluation error did not pause the rollout")
	}
}

func TestRolloutControllerLegacyEvaluatorMapsThreeStates(t *testing.T) {
	cases := []struct {
		name    string
		healthy bool
		err     error
		stage   domain.RolloutStage
		paused  bool
	}{
		{"true advances", true, nil, domain.StageRegionalRamp, false},
		{"false rolls back", false, nil, domain.StageTenantCanary, false},
		{"error pauses", false, errors.New("no data"), domain.StageTenantCanary, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newControllerFixture(t, domain.StageTenantCanary)
			fixture.controller.Health = nil
			fixture.controller.Legacy = fake.RolloutHealthEvaluatorFunc(func(context.Context, domain.RolloutTarget) (bool, error) {
				return testCase.healthy, testCase.err
			})
			_ = fixture.controller.Advance(context.Background(), "2026.08.1")
			state := fixture.state(t)
			if state.Stage != testCase.stage || state.Paused != testCase.paused {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

func TestRolloutControllerHardBreachRollsBackAndQuarantines(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageRegionalRamp)
	fixture.health = domain.RolloutHealth{Verdict: domain.RolloutUnhealthy, BreachedMetric: "isolation_breach", Samples: 10}
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
	state := fixture.state(t)
	if state.Stage != domain.StageQuarantined || state.TrafficPercentage != 0 {
		t.Fatalf("state = %+v", state)
	}
	if fixture.aliases.Pointed["regional"] != "2026.07.9" || fixture.capacity.Restored["2026.07.9"] != 1 {
		t.Fatalf("alias = %v capacity = %v", fixture.aliases.Pointed, fixture.capacity.Restored)
	}
	if got := fixture.audit.Categories(); len(got) != 2 || got[0] != "rollout.rolled_back" || got[1] != "rollout.quarantined" {
		t.Fatalf("audit = %v", got)
	}
}

func TestRolloutControllerSoftBreachNeedsConsecutiveWindows(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageRegionalRamp)
	fixture.health = domain.RolloutHealth{Verdict: domain.RolloutUnhealthy, BreachedMetric: "p95_latency", Samples: 100_000}
	for window := 1; window < fixture.controller.Guardrails.SoftBreachWindows; window++ {
		_ = fixture.controller.Advance(context.Background(), "2026.08.1")
		state := fixture.state(t)
		if state.Stage != domain.StageRegionalRamp || state.ConsecutiveBreaches != window {
			t.Fatalf("window %d state = %+v", window, state)
		}
	}
	_ = fixture.controller.Advance(context.Background(), "2026.08.1")
	if stage := fixture.state(t).Stage; stage != domain.StageRolledBack {
		t.Fatalf("stage = %s", stage)
	}
}

func TestRolloutControllerHealthyWindowsClearSoftBreaches(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageRegionalRamp)
	fixture.health = domain.RolloutHealth{Verdict: domain.RolloutUnhealthy, BreachedMetric: "p95_latency", Samples: 100_000}
	_ = fixture.controller.Advance(context.Background(), "2026.08.1")
	fixture.health = domain.RolloutHealth{Verdict: domain.RolloutHealthy, Samples: 100_000}
	fixture.now = fixture.now.Add(2 * fixture.controller.Guardrails.Cooldown)
	for range fixture.controller.Guardrails.HealthyToClear {
		_ = fixture.controller.Advance(context.Background(), "2026.08.1")
	}
	if breaches := fixture.state(t).ConsecutiveBreaches; breaches != 0 {
		t.Fatalf("consecutive breaches = %d", breaches)
	}
}

func TestRolloutControllerStaleGenerationLosesCAS(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	stale := fixture.state(t)
	// Another controller advanced the release first.
	if err := fixture.states.Transition(context.Background(),
		domain.RolloutTransition{PlatformVersion: "2026.08.1", FromGeneration: stale.Generation},
		domain.RolloutState{PlatformVersion: "2026.08.1", Stage: domain.StageRegionalRamp}); err != nil {
		t.Fatal(err)
	}
	err := fixture.states.Transition(context.Background(),
		domain.RolloutTransition{PlatformVersion: "2026.08.1", FromGeneration: stale.Generation},
		domain.RolloutState{PlatformVersion: "2026.08.1", Stage: domain.StageGlobalDefault})
	if !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestRolloutControllerLeasedByAnotherOwner(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	state := fixture.states.States["2026.08.1"]
	state.LeaseOwner = "controller-b"
	fixture.states.States["2026.08.1"] = state
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestRolloutControllerBypassRequiresDistinctApprover(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	bypass := domain.RolloutBypass{
		PlatformVersion: "2026.08.1", Stage: domain.StageTenantCanary, Scope: BypassScopeEvidence,
		Reason: "telemetry pipeline migration", RequestedBy: "operator-a", ApprovedBy: "operator-a",
		ExpiresAt: fixture.now.Add(time.Hour),
	}
	if err := fixture.controller.Bypass(context.Background(), bypass); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self-approved err = %v", err)
	}
	bypass.ApprovedBy = "release-manager"
	if err := fixture.controller.Bypass(context.Background(), bypass); err != nil {
		t.Fatal(err)
	}
	fixture.health = domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "telemetry stale", Samples: 1_000_000}
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); err != nil {
		t.Fatal(err)
	}
	if stage := fixture.state(t).Stage; stage != domain.StageRegionalRamp {
		t.Fatalf("stage = %s", stage)
	}
}

func TestRolloutControllerRollbackIsIdempotent(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageRegionalRamp)
	if err := fixture.controller.Rollback(context.Background(), "2026.08.1"); err != nil {
		t.Fatal(err)
	}
	generation := fixture.state(t).Generation
	if err := fixture.controller.Rollback(context.Background(), "2026.08.1"); err != nil {
		t.Fatal(err)
	}
	state := fixture.state(t)
	if state.Stage != domain.StageRolledBack || state.Generation != generation {
		t.Fatalf("state = %+v", state)
	}
	if len(fixture.audit.Events) != 1 {
		t.Fatalf("audit events = %d", len(fixture.audit.Events))
	}
}

func TestRolloutControllerShadowAmplificationBoundPauses(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageShadow)
	fixture.controller.Shadow = &fake.ShadowTrafficSampler{Ratio: 0.5}
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
	state := fixture.state(t)
	if !state.Paused || state.Stage != domain.StageShadow {
		t.Fatalf("state = %+v", state)
	}
}

func TestRolloutControllerDoesNotCommitRollbackBeforeTrafficMoves(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageRegionalRamp)
	fixture.aliases.Err = errors.New("alias unavailable")
	if err := fixture.controller.Rollback(context.Background(), "2026.08.1"); err == nil {
		t.Fatal("expected alias failure")
	}
	if state := fixture.state(t); state.Stage != domain.StageRegionalRamp || state.Generation != 1 {
		t.Fatalf("state = %+v", state)
	}
	fixture.aliases.Err = nil
	fixture.capacity.Err = errors.New("capacity unavailable")
	if err := fixture.controller.Rollback(context.Background(), "2026.08.1"); err == nil {
		t.Fatal("expected capacity failure")
	}
	if state := fixture.state(t); state.Stage != domain.StageRegionalRamp || state.Generation != 1 {
		t.Fatalf("state = %+v", state)
	}
}

func TestRolloutControllerGlobalDefaultRetiresPredecessor(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageRegionalRamp)
	if err := fixture.states.Create(context.Background(), domain.RolloutState{
		PlatformVersion: "2026.07.9", Stage: domain.StageGlobalDefault, Ring: "global", TrafficPercentage: 100,
		StageStartedAt: testStart.Add(-720 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.controller.Advance(context.Background(), "2026.08.1"); err != nil {
		t.Fatal(err)
	}
	previous, err := fixture.states.Get(context.Background(), "2026.07.9")
	if err != nil {
		t.Fatal(err)
	}
	if previous.Stage != domain.StageRetired {
		t.Fatalf("previous stage = %s", previous.Stage)
	}
	if fixture.state(t).Stage != domain.StageGlobalDefault {
		t.Fatalf("stage = %s", fixture.state(t).Stage)
	}
}

func TestRolloutControllerTickSkipsPausedAndTerminal(t *testing.T) {
	fixture := newControllerFixture(t, domain.StageTenantCanary)
	state := fixture.states.States["2026.08.1"]
	state.Paused, state.PauseReason = true, "operator hold"
	fixture.states.States["2026.08.1"] = state
	if err := fixture.controller.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.state(t); got.Stage != domain.StageTenantCanary || got.Generation != 1 {
		t.Fatalf("state = %+v", got)
	}
}

func TestRolloutControllerRejectsInvalidConfiguration(t *testing.T) {
	controller := &RolloutController{States: fake.NewRolloutStateStore(), Lease: fake.NewRolloutStateStore(), Audit: &fake.RecordingAuditSink{}}
	if err := controller.Start(context.Background(), domain.RolloutTarget{PlatformVersion: "2026.08.1"}); err == nil {
		t.Fatal("missing evaluator accepted")
	}
}
