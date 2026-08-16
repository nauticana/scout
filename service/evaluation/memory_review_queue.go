package evaluation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemoryReviewQueue is an in-process HumanReviewQueue for tests and single-node development.
type MemoryReviewQueue struct {
	Now func() time.Time
	// Capacity bounds pending items; zero means 1024.
	Capacity int

	mu    sync.Mutex
	items []domain.HumanReviewItem
}

var _ contract.HumanReviewQueue = (*MemoryReviewQueue)(nil)

func (queue *MemoryReviewQueue) now() time.Time {
	if queue.Now != nil {
		return queue.Now()
	}
	return time.Now()
}

// Enqueue appends an item; a replayed item id is a no-op.
func (queue *MemoryReviewQueue) Enqueue(_ context.Context, item domain.HumanReviewItem) error {
	if item.TenantID <= 0 || strings.TrimSpace(item.ItemID) == "" {
		return fmt.Errorf("%w: review item tenant and id are required", domain.ErrValidation)
	}
	capacity := queue.Capacity
	if capacity <= 0 {
		capacity = 1024
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for _, existing := range queue.items {
		if existing.TenantID == item.TenantID && existing.ItemID == item.ItemID {
			return nil
		}
	}
	if len(queue.items) >= capacity {
		return fmt.Errorf("%w: review queue is full", domain.ErrRateLimited)
	}
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = queue.now()
	}
	queue.items = append(queue.items, item)
	return nil
}

// Next returns pending items for the tenant in enqueue order.
func (queue *MemoryReviewQueue) Next(_ context.Context, tenantID int64, limit int) ([]domain.HumanReviewItem, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	var pending []domain.HumanReviewItem
	for _, item := range queue.items {
		if item.TenantID == tenantID && !item.Resolved {
			pending = append(pending, item)
			if len(pending) == limit {
				break
			}
		}
	}
	return pending, nil
}

// Resolve records the reviewer's verdict.
func (queue *MemoryReviewQueue) Resolve(_ context.Context, tenantID int64, itemID string, review domain.ExampleReview) error {
	if strings.TrimSpace(review.Reviewer) == "" || strings.TrimSpace(review.Verdict) == "" {
		return fmt.Errorf("%w: reviewer and verdict are required", domain.ErrValidation)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for i := range queue.items {
		if queue.items[i].TenantID == tenantID && queue.items[i].ItemID == itemID {
			if queue.items[i].Resolved {
				return fmt.Errorf("%w: review item %q is already resolved", domain.ErrConflict, itemID)
			}
			if review.ReviewedAt.IsZero() {
				review.ReviewedAt = queue.now()
			}
			queue.items[i].Review = review
			queue.items[i].Resolved = true
			return nil
		}
	}
	return fmt.Errorf("%w: review item %q", domain.ErrNotFound, itemID)
}
