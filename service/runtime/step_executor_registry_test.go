package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/internal/fake"
)

func TestStepExecutorRegistryRegistersAndResolves(t *testing.T) {
	registry := NewStepExecutorRegistry()
	executor := fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
		return domain.StepResult{}, nil
	})
	if err := registry.Register("model", executor); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := registry.ExecutorFor(context.Background(), "model")
	if err != nil {
		t.Fatalf("ExecutorFor: %v", err)
	}
	if _, ok := got.(fake.StepExecutorFunc); !ok {
		t.Fatalf("executor type = %T", got)
	}
}

func TestStepExecutorRegistryRejectsDuplicateAndMissingKinds(t *testing.T) {
	registry := NewStepExecutorRegistry()
	executor := fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
		return domain.StepResult{}, nil
	})
	if err := registry.Register("model", executor); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register("model", executor); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := registry.ExecutorFor(context.Background(), "tool"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing error = %v", err)
	}
	var nilExecutor contract.StepExecutor
	if err := registry.Register("", nilExecutor); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation error = %v", err)
	}
}
