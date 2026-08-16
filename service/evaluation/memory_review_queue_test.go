package evaluation

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestMemoryReviewQueueOrdersDedupesAndResolves(t *testing.T) {
	ctx := context.Background()
	queue := &MemoryReviewQueue{Now: fixedClock(testClock), Capacity: 2}
	item := domain.HumanReviewItem{ItemID: "i1", TenantID: 7, ManifestID: "m", ExampleID: "a", Reason: "low confidence"}

	if err := queue.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, item); err != nil {
		t.Fatalf("replay: %v", err)
	}
	second := item
	second.ItemID, second.ExampleID = "i2", "b"
	if err := queue.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	third := item
	third.ItemID = "i3"
	if err := queue.Enqueue(ctx, third); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("capacity = %v", err)
	}

	items, err := queue.Next(ctx, 7, 10)
	if err != nil || len(items) != 2 || items[0].ItemID != "i1" || !items[0].EnqueuedAt.Equal(testClock) {
		t.Fatalf("next = %+v, %v", items, err)
	}
	if other, _ := queue.Next(ctx, 8, 10); len(other) != 0 {
		t.Fatalf("cross-tenant leak: %+v", other)
	}

	if err := queue.Resolve(ctx, 7, "i1", domain.ExampleReview{Reviewer: "eve", Verdict: "accept"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Resolve(ctx, 7, "i1", domain.ExampleReview{Reviewer: "eve", Verdict: "accept"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("double resolve = %v", err)
	}
	if err := queue.Resolve(ctx, 7, "missing", domain.ExampleReview{Reviewer: "eve", Verdict: "accept"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown item = %v", err)
	}
	if err := queue.Resolve(ctx, 7, "i2", domain.ExampleReview{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty review = %v", err)
	}
	items, _ = queue.Next(ctx, 7, 10)
	if len(items) != 1 || items[0].ItemID != "i2" {
		t.Fatalf("pending after resolve = %+v", items)
	}
}
