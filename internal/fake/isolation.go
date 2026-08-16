package fake

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/cache"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// LoopDetector contains configurable loop callbacks.
type LoopDetector struct {
	ObserveFunc func(context.Context, int64, string, string) error
	ResetFunc   func(context.Context, int64, string) error
}

// Observe invokes ObserveFunc.
func (detector *LoopDetector) Observe(ctx context.Context, tenantID int64, conversationID, fingerprint string) error {
	return detector.ObserveFunc(ctx, tenantID, conversationID, fingerprint)
}

// Reset invokes ResetFunc.
func (detector *LoopDetector) Reset(ctx context.Context, tenantID int64, conversationID string) error {
	return detector.ResetFunc(ctx, tenantID, conversationID)
}

// CostCircuitBreaker contains configurable cost callbacks.
type CostCircuitBreaker struct {
	AllowFunc  func(context.Context, int64, string, int64) error
	RecordFunc func(context.Context, int64, string, domain.Usage) error
}

// Allow invokes AllowFunc.
func (breaker *CostCircuitBreaker) Allow(ctx context.Context, tenantID int64, agentID string, projectedCostMinorUnits int64) error {
	return breaker.AllowFunc(ctx, tenantID, agentID, projectedCostMinorUnits)
}

// Record invokes RecordFunc.
func (breaker *CostCircuitBreaker) Record(ctx context.Context, tenantID int64, agentID string, usage domain.Usage) error {
	return breaker.RecordFunc(ctx, tenantID, agentID, usage)
}

var _ contract.LoopDetector = (*LoopDetector)(nil)
var _ contract.CostCircuitBreaker = (*CostCircuitBreaker)(nil)

// CacheCall is one recorded increment against the fake cache.
type CacheCall struct {
	Key string
	N   int64
}

// CacheService is an in-memory keel cache whose increments can be failed or blocked; TTLs are ignored.
type CacheService struct {
	mu     sync.Mutex
	counts map[string]int64
	values map[string]string
	fail   error
	block  chan struct{}
	calls  []CacheCall
}

var _ cache.CacheService = (*CacheService)(nil)

// SetFail makes every increment return err; nil restores service.
func (c *CacheService) SetFail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = err
}

// SetBlock makes every increment wait on gate (or ctx) before completing; nil unblocks.
func (c *CacheService) SetBlock(gate chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.block = gate
}

// Calls returns every increment in order.
func (c *CacheService) Calls() []CacheCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]CacheCall(nil), c.calls...)
}

// Count returns the current counter value for key.
func (c *CacheService) Count(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

func (c *CacheService) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.values[key]; ok {
		return value, nil
	}
	if count, ok := c.counts[key]; ok {
		return strconv.FormatInt(count, 10), nil
	}
	return "", cache.ErrCacheMiss
}

func (c *CacheService) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]string)
	}
	c.values[key] = value
	return nil
}

func (c *CacheService) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, key)
	delete(c.counts, key)
	return nil
}

func (c *CacheService) Increment(ctx context.Context, key string) (int64, error) {
	return c.IncrementByWithTTL(ctx, key, 1, 0)
}

func (c *CacheService) IncrementWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return c.IncrementByWithTTL(ctx, key, 1, ttl)
}

func (c *CacheService) IncrementByWithTTL(ctx context.Context, key string, n int64, _ time.Duration) (int64, error) {
	c.mu.Lock()
	gate, fail := c.block, c.fail
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, CacheCall{Key: key, N: n})
	if fail != nil {
		return 0, fail
	}
	if c.counts == nil {
		c.counts = make(map[string]int64)
	}
	c.counts[key] += n
	return c.counts[key], nil
}

func (c *CacheService) RPush(_ context.Context, key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]string)
	}
	c.values[key] += value + "\n"
	return nil
}

func (c *CacheService) LPopAll(_ context.Context, key string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	joined := c.values[key]
	delete(c.values, key)
	if joined == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(joined, "\n"), "\n"), nil
}

func (c *CacheService) Publish(context.Context, string, string) error { return nil }

func (c *CacheService) Subscribe(context.Context, string) (<-chan string, error) {
	return make(chan string), nil
}

func (c *CacheService) Close() error { return nil }
