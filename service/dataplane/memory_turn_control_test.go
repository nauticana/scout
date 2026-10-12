package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

var memoryOwner = domain.TurnOwner{TenantID: 1, RequestID: "r", Fence: 1}

func TestMemoryTurnControlCancelsWatchedTurn(t *testing.T) {
	control := &MemoryTurnControl{}
	watch, err := control.Watch(context.Background(), memoryOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()

	if err := control.RequestHold(context.Background(), 1, "r"); err != nil || !watch.HoldRequested() || watch.Context().Err() != nil {
		t.Fatalf("a hold request flags the turn without cancelling it: %v", err)
	}
	if err := control.Cancel(context.Background(), 1, "r", "user pressed stop"); err != nil {
		t.Fatal(err)
	}
	<-watch.Context().Done()
	if cause := context.Cause(watch.Context()); !errors.Is(cause, domain.ErrTurnCanceled) {
		t.Fatalf("cause = %v", cause)
	}
}

func TestMemoryTurnControlUnknownAndDuplicate(t *testing.T) {
	control := &MemoryTurnControl{}
	if err := control.Cancel(context.Background(), 1, "missing", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if err := control.RequestHold(context.Background(), 1, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown hold = %v", err)
	}
	if _, err := control.Watch(context.Background(), domain.TurnOwner{}); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("missing owner = %v", err)
	}
	watch, err := control.Watch(context.Background(), memoryOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Watch(context.Background(), memoryOwner); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	watch.Release()
	// After release the request can be watched again and Cancel finds nothing.
	if err := control.Cancel(context.Background(), 1, "r", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("released = %v", err)
	}
	second, err := control.Watch(context.Background(), memoryOwner)
	if err != nil {
		t.Fatal(err)
	}
	watch.Release()
	if err := control.Cancel(context.Background(), 1, "r", "still registered"); err != nil {
		t.Fatalf("old release removed new watch: %v", err)
	}
	second.Release()
}
