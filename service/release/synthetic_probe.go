package release

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// SyntheticProbe is one scripted request with its assertions; Holdout probes
// run against the rollback target so the two can be compared.
type SyntheticProbe struct {
	Case    domain.ContractTestCase
	Holdout bool
}

// ProbeRunner executes synthetic probes and holdout traffic through the
// contract-test executor so a release is exercised outside user sessions.
type ProbeRunner struct {
	Executor  contract.ContractTestExecutor
	Evaluator contract.ContractAssertionEvaluator
	Bundles   contract.ReleaseBundleStore
	Probes    []SyntheticProbe
	Now       func() time.Time
}

var _ contract.SyntheticProber = (*ProbeRunner)(nil)

func (runner *ProbeRunner) now() time.Time {
	if runner.Now != nil {
		return runner.Now()
	}
	return time.Now()
}

func (runner *ProbeRunner) Probe(ctx context.Context, platformVersion string) ([]domain.SyntheticProbeResult, error) {
	if runner.Executor == nil || runner.Evaluator == nil {
		return nil, fmt.Errorf("probe runner: executor and evaluator are required")
	}
	holdoutTarget := ""
	if runner.Bundles != nil {
		bundle, err := runner.Bundles.Get(ctx, platformVersion)
		if err != nil {
			return nil, err
		}
		holdoutTarget = bundle.RollbackTarget
	}
	results := make([]domain.SyntheticProbeResult, 0, len(runner.Probes))
	for _, probe := range runner.Probes {
		target := platformVersion
		if probe.Holdout {
			if holdoutTarget == "" {
				return results, fmt.Errorf("%w: holdout probe %s needs a rollback target", domain.ErrValidation, probe.Case.TestCaseID)
			}
			target = holdoutTarget
		}
		started := runner.now()
		turn, err := runner.Executor.Execute(ctx, target, probe.Case)
		if err != nil {
			return results, fmt.Errorf("probe %s: %w", probe.Case.TestCaseID, err)
		}
		failures, err := runner.Evaluator.Evaluate(ctx, probe.Case, turn)
		if err != nil {
			return results, fmt.Errorf("evaluate probe %s: %w", probe.Case.TestCaseID, err)
		}
		results = append(results, domain.SyntheticProbeResult{
			PlatformVersion: target, ProbeID: probe.Case.TestCaseID, Holdout: probe.Holdout,
			Passed: len(failures) == 0, Failures: append([]string(nil), failures...),
			Latency: runner.now().Sub(started), ObservedAt: started,
		})
	}
	return results, nil
}
