package release

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RollbackDrillHarness rehearses a rollback against the persisted state
// machine, the alias and capacity hooks, session drain, and alert ownership,
// without touching live traffic.
type RollbackDrillHarness struct {
	Bundles  contract.ReleaseBundleStore
	States   contract.RolloutStateStore
	Aliases  contract.PlatformAliasSwitcher
	Capacity contract.CapacityRestorer
	Drain    *SessionDrainer
	Alerts   contract.AlertOwnershipChecker
	// SampleConversation is a real or synthetic conversation used to check drain behaviour.
	SampleConversation domain.ConversationRelease
	Now                func() time.Time
}

var _ contract.RollbackDrill = (*RollbackDrillHarness)(nil)

func (harness *RollbackDrillHarness) now() time.Time {
	if harness.Now != nil {
		return harness.Now()
	}
	return time.Now()
}

func (harness *RollbackDrillHarness) Run(ctx context.Context, platformVersion string) (domain.RollbackDrillReport, error) {
	if harness.Bundles == nil || harness.States == nil {
		return domain.RollbackDrillReport{}, fmt.Errorf("rollback drill: bundle and state stores are required")
	}
	started := harness.now()
	report := domain.RollbackDrillReport{PlatformVersion: platformVersion, StartedAt: started, Passed: true}
	bundle, err := harness.Bundles.Get(ctx, platformVersion)
	if err != nil {
		return domain.RollbackDrillReport{}, err
	}
	report.RollbackTarget = bundle.RollbackTarget
	checks := []struct {
		name string
		run  func(context.Context) error
	}{
		{"rollback_target_declared", func(context.Context) error {
			if bundle.RollbackTarget == "" {
				return errors.New("bundle names no rollback target")
			}
			_, err := harness.States.Get(ctx, bundle.RollbackTarget)
			return err
		}},
		{"alias_propagation", func(ctx context.Context) error {
			if harness.Aliases == nil {
				return errors.New("no alias switcher configured")
			}
			state, err := harness.States.Get(ctx, platformVersion)
			if err != nil {
				return err
			}
			return harness.Aliases.Point(ctx, state.Ring, bundle.RollbackTarget)
		}},
		{"capacity_restoration", func(ctx context.Context) error {
			if harness.Capacity == nil {
				return errors.New("no capacity restorer configured")
			}
			return harness.Capacity.Restore(ctx, bundle.RollbackTarget)
		}},
		{"session_drain", func(ctx context.Context) error {
			if harness.Drain == nil {
				return errors.New("no session drainer configured")
			}
			sample := harness.SampleConversation
			if sample.PlatformVersion == "" {
				sample.PlatformVersion = platformVersion
			}
			_, err := harness.Drain.AdmitTurn(ctx, sample)
			return err
		}},
		{"alert_ownership", func(ctx context.Context) error {
			if harness.Alerts == nil {
				return errors.New("no alert ownership checker configured")
			}
			owner, err := harness.Alerts.Owner(ctx, platformVersion)
			if err != nil {
				return err
			}
			if owner == "" {
				return errors.New("release has no alert owner")
			}
			return nil
		}},
	}
	for _, check := range checks {
		checkStarted := harness.now()
		err := check.run(ctx)
		result := domain.DrillCheck{Name: check.name, Passed: err == nil, Elapsed: harness.now().Sub(checkStarted)}
		if err != nil {
			result.Detail = err.Error()
			report.Passed = false
		}
		report.Checks = append(report.Checks, result)
	}
	report.Duration = harness.now().Sub(started)
	return report, nil
}
