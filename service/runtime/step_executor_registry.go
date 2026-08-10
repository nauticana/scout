package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StepExecutorRegistry resolves immutable step-kind registrations safely across workers.
type StepExecutorRegistry struct {
	mu        sync.RWMutex
	executors map[string]contract.StepExecutor
}

// NewStepExecutorRegistry creates an empty step executor registry.
func NewStepExecutorRegistry() *StepExecutorRegistry {
	return &StepExecutorRegistry{executors: make(map[string]contract.StepExecutor)}
}

// Register binds one unique step kind to an executor.
func (registry *StepExecutorRegistry) Register(stepKind string, executor contract.StepExecutor) error {
	stepKind = strings.TrimSpace(stepKind)
	if stepKind == "" || executor == nil {
		return fmt.Errorf("%w: step kind and executor are required", domain.ErrValidation)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.executors == nil {
		registry.executors = make(map[string]contract.StepExecutor)
	}
	if _, exists := registry.executors[stepKind]; exists {
		return fmt.Errorf("%w: step executor %q is already registered", domain.ErrConflict, stepKind)
	}
	registry.executors[stepKind] = executor
	return nil
}

// ExecutorFor returns the executor registered for a step kind.
func (registry *StepExecutorRegistry) ExecutorFor(ctx context.Context, stepKind string) (contract.StepExecutor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stepKind = strings.TrimSpace(stepKind)
	if stepKind == "" {
		return nil, fmt.Errorf("%w: step kind is required", domain.ErrValidation)
	}
	registry.mu.RLock()
	executor := registry.executors[stepKind]
	registry.mu.RUnlock()
	if executor == nil {
		return nil, fmt.Errorf("%w: step executor %q", domain.ErrNotFound, stepKind)
	}
	return executor, nil
}

var _ contract.StepExecutorRegistry = (*StepExecutorRegistry)(nil)
