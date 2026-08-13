package release

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ContractTestRunner executes compatibility cases and evaluates their assertions.
type ContractTestRunner struct {
	Executor  contract.ContractTestExecutor
	Evaluator contract.ContractAssertionEvaluator
	// Concurrency bounds parallel case execution; values below two run sequentially.
	Concurrency int
}

// Run evaluates cases with bounded concurrency, preserving input order; on
// failure it returns the completed prefix and the first error observed.
func (runner *ContractTestRunner) Run(ctx context.Context, platformVersion string, cases []domain.ContractTestCase) ([]domain.ContractTestResult, error) {
	if strings.TrimSpace(platformVersion) == "" {
		return nil, fmt.Errorf("%w: platform version is required", domain.ErrValidation)
	}
	if runner.Executor == nil || runner.Evaluator == nil {
		return nil, fmt.Errorf("contract test runner: executor and evaluator are required")
	}
	seen := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		if strings.TrimSpace(testCase.TestCaseID) == "" || strings.TrimSpace(testCase.AgentID) == "" || strings.TrimSpace(testCase.AgentVersion) == "" {
			return nil, fmt.Errorf("%w: contract test case, agent, and version are required", domain.ErrValidation)
		}
		if _, exists := seen[testCase.TestCaseID]; exists {
			return nil, fmt.Errorf("%w: duplicate contract test case %q", domain.ErrValidation, testCase.TestCaseID)
		}
		seen[testCase.TestCaseID] = struct{}{}
	}
	if len(cases) == 0 {
		return []domain.ContractTestResult{}, nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(max(runner.Concurrency, 1), len(cases))
	results := make([]domain.ContractTestResult, len(cases))
	completed := make([]bool, len(cases))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error

	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for index := range jobs {
				if runCtx.Err() != nil {
					return
				}
				result, err := runner.runCase(runCtx, platformVersion, cases[index])
				if err != nil {
					// The first failure cancels remaining work.
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
				results[index] = result
				completed[index] = true
			}
		}()
	}
dispatch:
	for index := range cases {
		select {
		case jobs <- index:
		case <-runCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr == nil {
		if err := ctx.Err(); err != nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		prefix := 0
		for prefix < len(completed) && completed[prefix] {
			prefix++
		}
		return results[:prefix], firstErr
	}
	return results, nil
}

func (runner *ContractTestRunner) runCase(ctx context.Context, platformVersion string, testCase domain.ContractTestCase) (domain.ContractTestResult, error) {
	result, err := runner.Executor.Execute(ctx, platformVersion, testCase)
	if err != nil {
		return domain.ContractTestResult{}, fmt.Errorf("execute contract test %q: %w", testCase.TestCaseID, err)
	}
	failures, err := runner.Evaluator.Evaluate(ctx, testCase, result)
	if err != nil {
		return domain.ContractTestResult{}, fmt.Errorf("evaluate contract test %q: %w", testCase.TestCaseID, err)
	}
	return domain.ContractTestResult{
		TestCaseID: testCase.TestCaseID,
		Passed:     len(failures) == 0,
		Failures:   append([]string(nil), failures...),
	}, nil
}

var _ contract.AgentContractTestRunner = (*ContractTestRunner)(nil)
