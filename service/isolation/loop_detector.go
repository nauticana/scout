package isolation

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
)

// MemoryLoopDetector flags a conversation when one step fingerprint repeats past a threshold.
type MemoryLoopDetector struct {
	// Threshold is the repeat count that trips detection; it is required.
	Threshold int
	// Window bounds how long idle history is retained; zero keeps it until Reset.
	Window time.Duration
	// MaxConversations bounds tracked history; zero uses a default.
	MaxConversations int
	// MaxFingerprints bounds history inside one conversation; default 1024.
	MaxFingerprints int
	Now             func() time.Time

	once   sync.Once
	mu     sync.Mutex
	states *lru.Cache[string, map[string]int]
}

var _ contract.LoopDetector = (*MemoryLoopDetector)(nil)

func (detector *MemoryLoopDetector) init() {
	detector.once.Do(func() {
		capacity := detector.MaxConversations
		if capacity <= 0 {
			capacity = defaultMaxTenants
		}
		detector.states = lru.New[string, map[string]int](capacity, detector.Now)
	})
}

// Observe counts one fingerprint and errs with domain.ErrLoopDetected at the threshold.
func (detector *MemoryLoopDetector) Observe(ctx context.Context, tenantID int64, conversationID, fingerprint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if detector.Threshold <= 0 {
		return fmt.Errorf("memory loop detector: threshold must be positive")
	}
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("%w: tenant, conversation, and fingerprint are required", domain.ErrValidation)
	}
	detector.init()
	key := loopKey(tenantID, conversationID)
	detector.mu.Lock()
	defer detector.mu.Unlock()
	counts, ok := detector.states.Get(key)
	if !ok {
		counts = make(map[string]int)
	}
	maxFingerprints := detector.MaxFingerprints
	if maxFingerprints <= 0 {
		maxFingerprints = 1024
	}
	if counts[fingerprint] == 0 && len(counts) >= maxFingerprints {
		return fmt.Errorf("%w: conversation fingerprint capacity reached", domain.ErrRateLimited)
	}
	counts[fingerprint]++
	detector.states.Set(key, counts, detector.Window)
	if counts[fingerprint] >= detector.Threshold {
		return fmt.Errorf("%w: fingerprint repeated %d times", domain.ErrLoopDetected, counts[fingerprint])
	}
	return nil
}

// Reset clears loop history after a turn reaches a terminal state.
func (detector *MemoryLoopDetector) Reset(ctx context.Context, tenantID int64, conversationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	detector.init()
	detector.mu.Lock()
	defer detector.mu.Unlock()
	detector.states.Delete(loopKey(tenantID, conversationID))
	return nil
}

func loopKey(tenantID int64, conversationID string) string {
	return strconv.FormatInt(tenantID, 10) + "/" + conversationID
}
