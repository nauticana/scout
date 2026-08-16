package isolation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/nauticana/keel/cache"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/limiter"
)

// WindowLimit is one shared fixed-window admission scope; both zero disables it.
type WindowLimit struct {
	Limit  int64
	Window time.Duration
}

func (l WindowLimit) enabled() bool { return l.Limit > 0 && l.Window > 0 }

func (l WindowLimit) valid() bool { return l.Limit == 0 && l.Window == 0 || l.enabled() }

// DistributedRateLimiterConfig configures shared tenant and fleet admission over a keel cache.
type DistributedRateLimiterConfig struct {
	Turn, Tool, Model                WindowLimit
	FleetTurn, FleetTool, FleetModel WindowLimit
	// KeyPrefix namespaces the counters in the shared store.
	KeyPrefix string
	// StoreTimeout bounds one store round trip; a slower store counts as unreachable.
	StoreTimeout time.Duration
	// FallbackFraction is the share of each limit one replica admits locally while the store is unreachable;
	// with R replicas the fleet-wide overshoot is bounded by R×FallbackFraction×limit.
	FallbackFraction float64
	// FallbackMaxTenants bounds the local fallback buckets.
	FallbackMaxTenants int
	// RecoveryProbe is how long the limiter stays degraded before a single caller probes the store again.
	RecoveryProbe time.Duration
}

// DistributedTenantRateLimiter enforces tenant and fleet admission across replicas with fixed-window
// counters in a shared keel cache; while the store is unreachable it degrades to reduced local buckets.
type DistributedTenantRateLimiter struct {
	Now func() time.Time

	store    cache.CacheService
	config   DistributedRateLimiterConfig
	metrics  contract.RuntimeMetrics
	fallback *limiter.TenantRateLimiter
	counter  *sharedCounter

	mu            sync.Mutex
	degraded      bool
	degradedUntil time.Time
	probing       bool
	generation    uint64
	closed        bool
}

var _ contract.TenantRateLimiter = (*DistributedTenantRateLimiter)(nil)

const rateLimitStoreDependency = "rate_limit_store"

// NewDistributedTenantRateLimiter validates config and binds the limiter to a shared store; metrics may be nil.
func NewDistributedTenantRateLimiter(store cache.CacheService, metrics contract.RuntimeMetrics, config DistributedRateLimiterConfig) (*DistributedTenantRateLimiter, error) {
	if store == nil {
		return nil, fmt.Errorf("distributed rate limiter: store is required")
	}
	for _, limit := range []WindowLimit{config.Turn, config.Tool, config.Model, config.FleetTurn, config.FleetTool, config.FleetModel} {
		if !limit.valid() {
			return nil, fmt.Errorf("distributed rate limiter: limit and window must both be zero or positive")
		}
	}
	if config.KeyPrefix == "" {
		return nil, fmt.Errorf("distributed rate limiter: key prefix is required")
	}
	if config.StoreTimeout <= 0 || config.RecoveryProbe <= 0 {
		return nil, fmt.Errorf("distributed rate limiter: store timeout and recovery probe must be positive")
	}
	if config.FallbackFraction <= 0 || config.FallbackFraction > 1 || math.IsNaN(config.FallbackFraction) {
		return nil, fmt.Errorf("distributed rate limiter: fallback fraction must be in (0, 1]")
	}
	if config.FallbackMaxTenants <= 0 {
		return nil, fmt.Errorf("distributed rate limiter: fallback max tenants must be positive")
	}
	l := &DistributedTenantRateLimiter{store: store, metrics: metrics, config: config}
	local := func(limit WindowLimit) limiter.RateLimit {
		if !limit.enabled() {
			return limiter.RateLimit{}
		}
		return limiter.RateLimit{
			PerSecond: float64(limit.Limit) / limit.Window.Seconds() * config.FallbackFraction,
			Burst:     max(1, int(math.Ceil(float64(limit.Limit)*config.FallbackFraction))),
		}
	}
	l.fallback = &limiter.TenantRateLimiter{
		Turn: local(config.Turn), Tool: local(config.Tool), Model: local(config.Model),
		FleetTurn: local(config.FleetTurn), FleetTool: local(config.FleetTool), FleetModel: local(config.FleetModel),
		MaxTenants: config.FallbackMaxTenants,
		Now:        l.now,
	}
	l.counter = &sharedCounter{store: store, timeout: config.StoreTimeout}
	return l, nil
}

func (l *DistributedTenantRateLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// AllowTurn enforces the shared turn admission rate.
func (l *DistributedTenantRateLimiter) AllowTurn(ctx context.Context, tenant domain.TenantContext) error {
	return l.allow(ctx, "turn", tenant, l.config.Turn, l.config.FleetTurn,
		func(ctx context.Context) error { return l.fallback.AllowTurn(ctx, tenant) })
}

// AllowToolCall enforces the shared tool-call rate.
func (l *DistributedTenantRateLimiter) AllowToolCall(ctx context.Context, call domain.ToolCall) error {
	return l.allow(ctx, "tool", call.TenantContext, l.config.Tool, l.config.FleetTool,
		func(ctx context.Context) error { return l.fallback.AllowToolCall(ctx, call) })
}

// AllowModelCall enforces the shared inference-call rate.
func (l *DistributedTenantRateLimiter) AllowModelCall(ctx context.Context, request domain.ModelRequest) error {
	return l.allow(ctx, "model", request.TenantContext, l.config.Model, l.config.FleetModel,
		func(ctx context.Context) error { return l.fallback.AllowModelCall(ctx, request) })
}

// Degraded reports whether admission currently runs on the local fallback.
func (l *DistributedTenantRateLimiter) Degraded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.degraded
}

// Close stops admission; it is idempotent and owns no goroutines to wait for.
func (l *DistributedTenantRateLimiter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *DistributedTenantRateLimiter) allow(ctx context.Context, scope string, tenant domain.TenantContext, tenantLimit, fleetLimit WindowLimit, local func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant.TenantID <= 0 {
		return fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if !tenantLimit.enabled() && !fleetLimit.enabled() {
		return nil
	}
	now := l.now()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return fmt.Errorf("%w: distributed rate limiter is closed", domain.ErrConflict)
	}
	useFallback, probe, generation := l.routeLocked(now)
	l.mu.Unlock()
	if useFallback {
		return local(ctx)
	}
	err := l.allowShared(ctx, scope, tenant.TenantID, tenantLimit, fleetLimit, now)
	var outage *storeOutage
	if errors.As(err, &outage) {
		l.markOutage(ctx, tenant, now, outage.err)
		return local(ctx)
	}
	if probe {
		l.markRecovered(ctx, tenant, now, generation)
	}
	return err
}

// routeLocked decides shared vs. local admission; one caller per RecoveryProbe becomes the probe.
func (l *DistributedTenantRateLimiter) routeLocked(now time.Time) (useFallback, probe bool, generation uint64) {
	if !l.degraded {
		return false, false, l.generation
	}
	if now.Before(l.degradedUntil) || l.probing {
		return true, false, l.generation
	}
	l.probing = true
	return false, true, l.generation
}

func (l *DistributedTenantRateLimiter) markOutage(ctx context.Context, tenant domain.TenantContext, now time.Time, cause error) {
	l.mu.Lock()
	entered := !l.degraded
	l.degraded = true
	l.probing = false
	l.degradedUntil = now.Add(l.config.RecoveryProbe)
	l.generation++
	l.mu.Unlock()
	if l.metrics == nil {
		return
	}
	l.metrics.RecordDependency(ctx, tenant.TenantID, rateLimitStoreDependency, "increment", domain.Usage{}, cause)
	if entered {
		l.observe(ctx, tenant, now, domain.OutcomeDegraded, "store_unavailable")
	}
}

// markRecovered clears degraded mode only when no newer outage arrived while the probe was in flight.
func (l *DistributedTenantRateLimiter) markRecovered(ctx context.Context, tenant domain.TenantContext, now time.Time, generation uint64) {
	l.mu.Lock()
	recovered := l.degraded && l.probing && l.generation == generation
	if recovered {
		l.degraded = false
	}
	l.probing = false
	l.mu.Unlock()
	if recovered && l.metrics != nil {
		l.metrics.RecordDependency(ctx, tenant.TenantID, rateLimitStoreDependency, "increment", domain.Usage{}, nil)
		l.observe(ctx, tenant, now, domain.OutcomeOK, "")
	}
}

func (l *DistributedTenantRateLimiter) observe(ctx context.Context, tenant domain.TenantContext, now time.Time, outcome domain.ObservationOutcome, errorClass string) {
	recorder, ok := l.metrics.(contract.ObservationRecorder)
	if !ok {
		return
	}
	recorder.RecordObservation(ctx, domain.Observation{
		TenantID: tenant.TenantID, TenantTier: tenant.Tier, Region: tenant.Region,
		Stage: domain.StageAdmission, Component: rateLimitStoreDependency,
		StartedAt: now, Outcome: outcome, ErrorClass: errorClass,
	})
}

// storeOutage marks a store failure so allow can distinguish it from a limit rejection.
type storeOutage struct{ err error }

func (e *storeOutage) Error() string { return "rate limit store unavailable: " + e.err.Error() }

func (e *storeOutage) Unwrap() error { return e.err }

// allowShared increments the tenant window first and the fleet window second; a fleet rejection
// compensates the tenant increment. Between those two increments a concurrent caller can observe the
// uncompensated tenant count, so the fleet limit may over-admit by up to the number of in-flight
// admissions until a single multi-scope compare-and-increment primitive exists in keel.
func (l *DistributedTenantRateLimiter) allowShared(ctx context.Context, scope string, tenantID int64, tenantLimit, fleetLimit WindowLimit, now time.Time) error {
	tenantKey, tenantTTL, tenantRetry := "", time.Duration(0), time.Duration(0)
	if tenantLimit.enabled() {
		tenantKey, tenantTTL, tenantRetry = windowKey(l.config.KeyPrefix, scope, "t", tenantID, tenantLimit.Window, now)
		count, err := l.counter.increment(ctx, tenantKey, tenantTTL)
		if err != nil {
			return l.classify(ctx, err)
		}
		if count > tenantLimit.Limit {
			return &limiter.LimitError{Err: domain.ErrRateLimited, Scope: scope + ".tenant", After: tenantRetry}
		}
	}
	if !fleetLimit.enabled() {
		return nil
	}
	fleetKey, fleetTTL, fleetRetry := windowKey(l.config.KeyPrefix, scope, "f", 0, fleetLimit.Window, now)
	count, err := l.counter.increment(ctx, fleetKey, fleetTTL)
	if err == nil && count <= fleetLimit.Limit {
		return nil
	}
	if tenantKey != "" {
		if compensateErr := l.compensate(ctx, tenantKey, tenantTTL, tenantLimit.Window, now); compensateErr != nil && err == nil {
			err = compensateErr
		}
	}
	if err != nil {
		return l.classify(ctx, err)
	}
	return &limiter.LimitError{Err: domain.ErrRateLimited, Scope: scope + ".fleet", After: fleetRetry}
}

// compensate returns one tenant admission the fleet refused, but never into an already rolled window.
func (l *DistributedTenantRateLimiter) compensate(ctx context.Context, tenantKey string, ttl, window time.Duration, started time.Time) error {
	if epochOf(l.now(), window) != epochOf(started, window) {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, l.config.StoreTimeout)
	defer cancel()
	_, err := l.store.IncrementByWithTTL(callCtx, tenantKey, -1, ttl)
	return err
}

// classify keeps caller cancellation as-is and turns every other store failure into an outage.
func (l *DistributedTenantRateLimiter) classify(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return &storeOutage{err: err}
}

func epochOf(now time.Time, window time.Duration) int64 { return now.UnixNano() / int64(window) }

// windowKey names one epoch-aligned counter and returns its cleanup TTL and the delay to the next window.
func windowKey(prefix, scope, kind string, id int64, window time.Duration, now time.Time) (string, time.Duration, time.Duration) {
	epoch := epochOf(now, window)
	key := prefix + ":" + scope + ":" + kind + ":" + strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(epoch, 10)
	retry := time.Duration((epoch+1)*int64(window) - now.UnixNano())
	return key, 2 * window, retry
}

// sharedCounter serializes increments per key: while one store call is in flight, arriving callers
// join the next batch, which one of them sends as a single IncrementByWithTTL. Every member then
// receives a distinct count from the returned range, so a hot key costs one round trip per batch.
type sharedCounter struct {
	store   cache.CacheService
	timeout time.Duration

	mu     sync.Mutex
	states map[string]*counterKeyState
}

type counterKeyState struct {
	next *counterBatch
}

type counterBatch struct {
	n       int64
	current bool
	claimed bool
	ready   chan struct{}
	done    chan struct{}
	base    int64
	err     error
	taken   int64
}

func (c *sharedCounter) increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	c.mu.Lock()
	if c.states == nil {
		c.states = make(map[string]*counterKeyState)
	}
	state := c.states[key]
	if state == nil {
		c.states[key] = &counterKeyState{}
		c.mu.Unlock()
		count, err := c.call(ctx, key, 1, ttl)
		c.handoff(key)
		return count, err
	}
	batch := state.next
	if batch == nil {
		batch = &counterBatch{ready: make(chan struct{}), done: make(chan struct{})}
		state.next = batch
	}
	batch.n++
	c.mu.Unlock()

	select {
	case <-batch.ready:
	case <-ctx.Done():
		return 0, c.leave(key, batch, ctx.Err())
	}
	c.mu.Lock()
	lead := !batch.claimed
	if lead {
		batch.claimed = true
	}
	n := batch.n
	c.mu.Unlock()
	if lead {
		count, err := c.call(ctx, key, n, ttl)
		batch.base, batch.err = count-n, err
		close(batch.done)
		c.handoff(key)
	}
	select {
	case <-batch.done:
		if batch.err != nil {
			return 0, batch.err
		}
		c.mu.Lock()
		batch.taken++
		position := batch.taken
		c.mu.Unlock()
		return batch.base + position, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *sharedCounter) call(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.store.IncrementByWithTTL(callCtx, key, n, ttl)
}

// handoff releases the key or promotes the waiting batch to current.
func (c *sharedCounter) handoff(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handoffLocked(key)
}

func (c *sharedCounter) handoffLocked(key string) {
	state := c.states[key]
	if state == nil || state.next == nil || state.next.n == 0 {
		delete(c.states, key)
		return
	}
	batch := state.next
	state.next = nil
	batch.current = true
	close(batch.ready)
}

// leave withdraws a canceled member from an unsent batch; a member already charged keeps its slot unused.
func (c *sharedCounter) leave(key string, batch *counterBatch, cause error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if batch.claimed {
		return cause
	}
	batch.n--
	if batch.n > 0 {
		return cause
	}
	if batch.current {
		c.handoffLocked(key)
	} else if state := c.states[key]; state != nil && state.next == batch {
		state.next = nil
	}
	return cause
}

func (c *sharedCounter) pending(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state := c.states[key]; state != nil && state.next != nil {
		return state.next.n
	}
	return 0
}
