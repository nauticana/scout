package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestStepExecutorRegistryRegistersAndResolves(t *testing.T) {
	registry := NewStepExecutorRegistry()
	executor := fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
		return domain.StepResult{}, nil
	})
	if err := registry.Register(domain.StepKindModel, executor); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := registry.ExecutorFor(context.Background(), domain.StepKindModel)
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
	if err := registry.Register(domain.StepKindModel, executor); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(domain.StepKindModel, executor); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := registry.ExecutorFor(context.Background(), domain.StepKindTool); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing error = %v", err)
	}
	var nilExecutor contract.StepExecutor
	if err := registry.Register(domain.StepKindTool, nilExecutor); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation error = %v", err)
	}
	for _, kind := range []string{"", "model", "tool_loop", " M"} {
		if err := registry.Register(kind, executor); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Register(%q) error = %v, want validation", kind, err)
		}
		if _, err := registry.ExecutorFor(context.Background(), kind); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("ExecutorFor(%q) error = %v, want validation", kind, err)
		}
	}
}
