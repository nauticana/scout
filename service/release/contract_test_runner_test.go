package release

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func TestContractTestRunnerPreservesOrderAndFailures(t *testing.T) {
	var executed []string
	runner := &ContractTestRunner{
		Executor: fake.ContractTestExecutorFunc(func(_ context.Context, version string, testCase domain.ContractTestCase) (domain.TurnResult, error) {
			if version != "build-1" {
				t.Fatalf("version = %q", version)
			}
			executed = append(executed, testCase.TestCaseID)
			return domain.TurnResult{Response: []byte(testCase.TestCaseID)}, nil
		}),
		Evaluator: fake.ContractAssertionEvaluatorFunc(func(_ context.Context, testCase domain.ContractTestCase, result domain.TurnResult) ([]string, error) {
			if string(result.Response) != testCase.TestCaseID {
				t.Fatalf("result = %+v", result)
			}
			if testCase.TestCaseID == "fail" {
				return []string{"response mismatch"}, nil
			}
			return nil, nil
		}),
	}
	cases := []domain.ContractTestCase{{TestCaseID: "pass", AgentID: "agent", AgentVersion: "v1"}, {TestCaseID: "fail", AgentID: "agent", AgentVersion: "v1"}}
	results, err := runner.Run(context.Background(), "build-1", cases)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(executed, []string{"pass", "fail"}) || !results[0].Passed || results[1].Passed {
		t.Fatalf("executed = %v, results = %+v", executed, results)
	}
}

func TestContractTestRunnerReturnsPartialResults(t *testing.T) {
	want := errors.New("runtime unavailable")
	runner := &ContractTestRunner{
		Executor: fake.ContractTestExecutorFunc(func(_ context.Context, _ string, testCase domain.ContractTestCase) (domain.TurnResult, error) {
			if testCase.TestCaseID == "second" {
				return domain.TurnResult{}, want
			}
			return domain.TurnResult{}, nil
		}),
		Evaluator: fake.ContractAssertionEvaluatorFunc(func(context.Context, domain.ContractTestCase, domain.TurnResult) ([]string, error) {
			return nil, nil
		}),
	}
	results, err := runner.Run(context.Background(), "build-1", []domain.ContractTestCase{{TestCaseID: "first", AgentID: "agent", AgentVersion: "v1"}, {TestCaseID: "second", AgentID: "agent", AgentVersion: "v1"}})
	if !errors.Is(err, want) || len(results) != 1 {
		t.Fatalf("results = %+v, error = %v", results, err)
	}
}

func TestContractTestRunnerRejectsDuplicateCases(t *testing.T) {
	runner := &ContractTestRunner{
		Executor: fake.ContractTestExecutorFunc(func(context.Context, string, domain.ContractTestCase) (domain.TurnResult, error) {
			return domain.TurnResult{}, nil
		}),
		Evaluator: fake.ContractAssertionEvaluatorFunc(func(context.Context, domain.ContractTestCase, domain.TurnResult) ([]string, error) {
			return nil, nil
		}),
	}
	_, err := runner.Run(context.Background(), "build-1", []domain.ContractTestCase{{TestCaseID: "same", AgentID: "agent", AgentVersion: "v1"}, {TestCaseID: "same", AgentID: "agent", AgentVersion: "v1"}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestContractTestRunnerBoundedConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	runner := &ContractTestRunner{
		Concurrency: 3,
		Executor: fake.ContractTestExecutorFunc(func(context.Context, string, domain.ContractTestCase) (domain.TurnResult, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := peak.Load()
				if current <= observed || peak.CompareAndSwap(observed, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			return domain.TurnResult{}, nil
		}),
		Evaluator: fake.ContractAssertionEvaluatorFunc(func(context.Context, domain.ContractTestCase, domain.TurnResult) ([]string, error) {
			return nil, nil
		}),
	}
	cases := make([]domain.ContractTestCase, 9)
	for i := range cases {
		cases[i] = domain.ContractTestCase{TestCaseID: fmt.Sprintf("case-%d", i), AgentID: "agent", AgentVersion: "v1"}
	}
	results, err := runner.Run(context.Background(), "build-1", cases)
	if err != nil || len(results) != 9 {
		t.Fatalf("results = %d, %v", len(results), err)
	}
	for i, result := range results {
		if result.TestCaseID != cases[i].TestCaseID {
			t.Fatalf("order broken at %d: %+v", i, result)
		}
	}
	if peak.Load() > 3 || peak.Load() < 2 {
		t.Fatalf("peak concurrency = %d", peak.Load())
	}
}
