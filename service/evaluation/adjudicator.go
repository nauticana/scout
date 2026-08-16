package evaluation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Adjudicator is an in-process FailureAdjudicator: failures with the same
// content digest fold into one bucket per tenant, and only an explicit human
// verdict marks a bucket accepted for golden-set promotion.
type Adjudicator struct {
	// Capacity bounds tracked buckets across tenants; zero means 4096.
	Capacity int
	Now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*domain.ProductionFailure
}

var _ contract.FailureAdjudicator = (*Adjudicator)(nil)

func (adjudicator *Adjudicator) now() time.Time {
	if adjudicator.Now != nil {
		return adjudicator.Now()
	}
	return time.Now()
}

func (adjudicator *Adjudicator) capacity() int {
	if adjudicator.Capacity > 0 {
		return adjudicator.Capacity
	}
	return 4096
}

func bucketKey(tenantID int64, failureID string) string {
	return fmt.Sprintf("%d/%s", tenantID, failureID)
}

// Report folds the failure into its digest bucket and returns the bucket state.
func (adjudicator *Adjudicator) Report(_ context.Context, tenantID int64, sampleID string, failure []byte) (domain.ProductionFailure, error) {
	if tenantID <= 0 || strings.TrimSpace(sampleID) == "" || len(failure) == 0 {
		return domain.ProductionFailure{}, fmt.Errorf("%w: tenant, sample id, and failure content are required", domain.ErrValidation)
	}
	digest := sha256Hex(failure)
	failureID := digest[:32]
	now := adjudicator.now()
	adjudicator.mu.Lock()
	defer adjudicator.mu.Unlock()
	if adjudicator.buckets == nil {
		adjudicator.buckets = make(map[string]*domain.ProductionFailure)
	}
	key := bucketKey(tenantID, failureID)
	bucket := adjudicator.buckets[key]
	if bucket == nil {
		if len(adjudicator.buckets) >= adjudicator.capacity() {
			return domain.ProductionFailure{}, fmt.Errorf("%w: adjudication backlog is full", domain.ErrRateLimited)
		}
		bucket = &domain.ProductionFailure{FailureID: failureID, TenantID: tenantID, SampleID: sampleID, Digest: digest, FirstSeenAt: now}
		adjudicator.buckets[key] = bucket
	}
	bucket.Occurrences++
	bucket.LastSeenAt = now
	return *bucket, nil
}

// Adjudicate records the reviewer's verdict once.
func (adjudicator *Adjudicator) Adjudicate(_ context.Context, tenantID int64, failureID, reviewer string, accepted bool) (domain.ProductionFailure, error) {
	if tenantID <= 0 || strings.TrimSpace(failureID) == "" || strings.TrimSpace(reviewer) == "" {
		return domain.ProductionFailure{}, fmt.Errorf("%w: tenant, failure id, and reviewer are required", domain.ErrValidation)
	}
	adjudicator.mu.Lock()
	defer adjudicator.mu.Unlock()
	bucket := adjudicator.buckets[bucketKey(tenantID, failureID)]
	if bucket == nil {
		return domain.ProductionFailure{}, fmt.Errorf("%w: failure %q", domain.ErrNotFound, failureID)
	}
	if bucket.Adjudicated {
		return domain.ProductionFailure{}, fmt.Errorf("%w: failure %q is already adjudicated", domain.ErrConflict, failureID)
	}
	bucket.Adjudicated, bucket.Accepted, bucket.Reviewer = true, accepted, reviewer
	return *bucket, nil
}

// Pending returns unadjudicated buckets, most frequent first.
func (adjudicator *Adjudicator) Pending(_ context.Context, tenantID int64, limit int) ([]domain.ProductionFailure, error) {
	if tenantID <= 0 || limit <= 0 {
		return nil, fmt.Errorf("%w: tenant and positive limit are required", domain.ErrValidation)
	}
	adjudicator.mu.Lock()
	defer adjudicator.mu.Unlock()
	var pending []domain.ProductionFailure
	for _, bucket := range adjudicator.buckets {
		if bucket.TenantID == tenantID && !bucket.Adjudicated {
			pending = append(pending, *bucket)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Occurrences != pending[j].Occurrences {
			return pending[i].Occurrences > pending[j].Occurrences
		}
		return pending[i].FailureID < pending[j].FailureID
	})
	if len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}
