package fake

import (
	"context"

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
