package release

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ContractTestRunner executes compatibility cases and evaluates their assertions.
type ContractTestRunner struct {
	Executor  contract.ContractTestExecutor
	Evaluator contract.ContractAssertionEvaluator
}

// Run evaluates cases in input order and returns partial results on infrastructure failure.
func (runner *ContractTestRunner) Run(ctx context.Context, platformVersion string, cases []domain.ContractTestCase) ([]domain.ContractTestResult, error) {
	if strings.TrimSpace(platformVersion) == "" {
		return nil, fmt.Errorf("%w: platform version is required", domain.ErrValidation)
	}
	if runner.Executor == nil || runner.Evaluator == nil {
		return nil, fmt.Errorf("contract test runner: executor and evaluator are required")
	}
	results := make([]domain.ContractTestResult, 0, len(cases))
	seen := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		if strings.TrimSpace(testCase.TestCaseID) == "" || strings.TrimSpace(testCase.AgentID) == "" || strings.TrimSpace(testCase.AgentVersion) == "" {
			return results, fmt.Errorf("%w: contract test case, agent, and version are required", domain.ErrValidation)
		}
		if _, exists := seen[testCase.TestCaseID]; exists {
			return results, fmt.Errorf("%w: duplicate contract test case %q", domain.ErrValidation, testCase.TestCaseID)
		}
		seen[testCase.TestCaseID] = struct{}{}
		result, err := runner.Executor.Execute(ctx, platformVersion, testCase)
		if err != nil {
			return results, fmt.Errorf("execute contract test %q: %w", testCase.TestCaseID, err)
		}
		failures, err := runner.Evaluator.Evaluate(ctx, testCase, result)
		if err != nil {
			return results, fmt.Errorf("evaluate contract test %q: %w", testCase.TestCaseID, err)
		}
		results = append(results, domain.ContractTestResult{
			TestCaseID: testCase.TestCaseID,
			Passed:     len(failures) == 0,
			Failures:   append([]string(nil), failures...),
		})
	}
	return results, nil
}

var _ contract.AgentContractTestRunner = (*ContractTestRunner)(nil)
