package isolation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestMemoryLoopDetectorTripsAtThreshold(t *testing.T) {
	detector := &MemoryLoopDetector{Threshold: 3, MaxConversations: 4096}
	ctx := context.Background()
	for i := range 2 {
		if err := detector.Observe(ctx, 1, "c", "same-step"); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}
	if err := detector.Observe(ctx, 1, "c", "same-step"); !errors.Is(err, domain.ErrLoopDetected) {
		t.Fatalf("third observe = %v", err)
	}
	// A different fingerprint or conversation is unaffected.
	if err := detector.Observe(ctx, 1, "c", "other-step"); err != nil {
		t.Fatal(err)
	}
	if err := detector.Observe(ctx, 1, "c2", "same-step"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryLoopDetectorResetAndWindow(t *testing.T) {
	now := time.Unix(0, 0)
	detector := &MemoryLoopDetector{Threshold: 2, Window: time.Minute, MaxConversations: 4096, Now: func() time.Time { return now }}
	ctx := context.Background()

	if err := detector.Observe(ctx, 1, "c", "f"); err != nil {
		t.Fatal(err)
	}
	if err := detector.Reset(ctx, 1, "c"); err != nil {
		t.Fatal(err)
	}
	if err := detector.Observe(ctx, 1, "c", "f"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	if err := detector.Observe(ctx, 1, "c", "f"); err != nil {
		t.Fatalf("expired history must not trip: %v", err)
	}
}

func TestMemoryLoopDetectorValidation(t *testing.T) {
	detector := &MemoryLoopDetector{Threshold: 1, MaxConversations: 4096}
	if err := detector.Observe(context.Background(), 0, "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
	broken := &MemoryLoopDetector{}
	if err := broken.Observe(context.Background(), 1, "c", "f"); err == nil {
		t.Fatal("missing threshold must error")
	}
}

func TestMemoryLoopDetectorConcurrentObservations(t *testing.T) {
	detector := &MemoryLoopDetector{Threshold: 101, MaxConversations: 4096}
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := detector.Observe(context.Background(), 1, "c", "f"); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if err := detector.Observe(context.Background(), 1, "c", "f"); !errors.Is(err, domain.ErrLoopDetected) {
		t.Fatalf("threshold = %v", err)
	}
}
