package dataplane

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func TestSessionCoordinatorReturnsCacheHit(t *testing.T) {
	want := domain.SessionSnapshot{ConversationID: "conversation", Revision: 3}
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{LoadFunc: func(context.Context, int64, string) (domain.SessionSnapshot, error) {
			t.Fatal("store must not be called")
			return domain.SessionSnapshot{}, nil
		}},
		Cache: &fake.HotSessionCache{GetFunc: func(context.Context, int64, string) (domain.SessionSnapshot, bool, error) {
			return want, true, nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	got, err := coordinator.Load(context.Background(), 7, "conversation")
	if err != nil || got.Revision != want.Revision {
		t.Fatalf("snapshot = %+v, error = %v", got, err)
	}
}

func TestSessionCoordinatorFallsBackAndReportsCacheFailure(t *testing.T) {
	cacheErr := errors.New("cache unavailable")
	want := domain.SessionSnapshot{ConversationID: "conversation", Revision: 3}
	put := false
	var reported error
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{LoadFunc: func(context.Context, int64, string) (domain.SessionSnapshot, error) {
			return want, nil
		}},
		Cache: &fake.HotSessionCache{
			GetFunc: func(context.Context, int64, string) (domain.SessionSnapshot, bool, error) {
				return domain.SessionSnapshot{}, false, cacheErr
			},
			PutFunc: func(context.Context, int64, domain.SessionSnapshot) error {
				put = true
				return nil
			},
		},
		Metrics: &fake.RuntimeMetrics{RecordDependencyFunc: func(_ context.Context, _ int64, dependency, _ string, _ domain.Usage, err error) {
			if dependency != "session_cache" {
				t.Fatalf("dependency = %q", dependency)
			}
			reported = err
		}},
	}
	got, err := coordinator.Load(context.Background(), 7, "conversation")
	if err != nil || got.Revision != want.Revision || !put || !errors.Is(reported, cacheErr) {
		t.Fatalf("snapshot = %+v, put = %v, reported = %v, error = %v", got, put, reported, err)
	}
}

func TestSessionCoordinatorPersistsBeforeInvalidation(t *testing.T) {
	var calls []string
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{
			CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error {
				calls = append(calls, "checkpoint")
				return nil
			},
			CompleteFunc: func(context.Context, int64, string, int64, domain.TurnResult) error {
				calls = append(calls, "complete")
				return nil
			},
		},
		Cache: &fake.HotSessionCache{InvalidateFunc: func(context.Context, int64, string) error {
			calls = append(calls, "invalidate")
			return nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	checkpoint := domain.StepCheckpoint{ConversationID: "conversation"}
	if err := coordinator.Checkpoint(context.Background(), 7, 2, checkpoint); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := coordinator.Complete(context.Background(), 7, "conversation", 3, domain.TurnResult{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := []string{"checkpoint", "invalidate", "complete", "invalidate"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestSessionCoordinatorDoesNotInvalidateAfterStoreFailure(t *testing.T) {
	want := errors.New("write failed")
	invalidated := false
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error {
			return want
		}},
		Cache: &fake.HotSessionCache{InvalidateFunc: func(context.Context, int64, string) error {
			invalidated = true
			return nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	err := coordinator.Checkpoint(context.Background(), 7, 2, domain.StepCheckpoint{ConversationID: "conversation"})
	if !errors.Is(err, want) || invalidated {
		t.Fatalf("error = %v, invalidated = %v", err, invalidated)
	}
}
