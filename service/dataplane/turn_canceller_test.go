package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestMemoryTurnCancellerCancelsWatchedTurn(t *testing.T) {
	canceller := &MemoryTurnCanceller{}
	turnCtx, release, err := canceller.Watch(context.Background(), 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := canceller.Cancel(context.Background(), 1, "r", "user pressed stop"); err != nil {
		t.Fatal(err)
	}
	<-turnCtx.Done()
	if cause := context.Cause(turnCtx); !errors.Is(cause, domain.ErrTurnCanceled) {
		t.Fatalf("cause = %v", cause)
	}
}

func TestMemoryTurnCancellerUnknownAndDuplicate(t *testing.T) {
	canceller := &MemoryTurnCanceller{}
	if err := canceller.Cancel(context.Background(), 1, "missing", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	_, release, err := canceller.Watch(context.Background(), 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := canceller.Watch(context.Background(), 1, "r"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	release()
	// After release the request can be watched again and Cancel finds nothing.
	if err := canceller.Cancel(context.Background(), 1, "r", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("released = %v", err)
	}
	_, release2, err := canceller.Watch(context.Background(), 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := canceller.Cancel(context.Background(), 1, "r", "still registered"); err != nil {
		t.Fatalf("old release removed new watch: %v", err)
	}
	release2()
}
