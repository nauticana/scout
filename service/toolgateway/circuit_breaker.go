package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/limiter"
	"github.com/nauticana/scout/internal/lru"
)

// ErrInvalidToolOutput marks a tool response that failed its registered output contract.
var ErrInvalidToolOutput = errors.New("invalid tool output")

// CircuitBreakerConfig defines one breaker; every limit is validated by NewCircuitBreaker.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of counted failures inside Window that opens a circuit.
	FailureThreshold int
	// Window is the failure counting window; counts reset when it elapses without opening.
	Window time.Duration
	// MinSamples is the minimum attempts inside Window before the threshold can open; zero disables.
	MinSamples int
	// OpenDuration is how long an open circuit rejects before admitting one recovery probe.
	OpenDuration time.Duration
	// MaxEntries bounds tenant×tool state; the least recently used entry is evicted beyond it.
	MaxEntries int
	// SharedDestinationHealth also tracks one circuit per destination host across tenants (Admit/Settle only).
	SharedDestinationHealth bool
	// MaxDestinations bounds destination state when shared health is enabled.
	MaxDestinations int
	Now             func() time.Time
}

// DefaultCircuitBreakerConfig is a conservative starting point for real tool dependencies.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{FailureThreshold: 5, Window: 30 * time.Second, OpenDuration: 15 * time.Second, MaxEntries: 4096, MaxDestinations: 1024}
}

type circuitState uint8

const (
	stateClosed circuitState = iota
	stateOpen
	stateHalfOpen
)

func (state circuitState) String() string {
	return [...]string{"closed", "open", "half-open"}[state]
}

type circuit struct {
	state       circuitState
	generation  int64
	failures    int
	samples     int
	windowStart time.Time
	openedAt    time.Time
	probing     bool
}

// CircuitBreaker is a closed → open → half-open breaker per tenant×tool with one recovery probe,
// generation fencing, bounded cardinality, and optional shared destination health.
type CircuitBreaker struct {
	config       CircuitBreakerConfig
	mu           sync.Mutex
	entries      *lru.Cache[string, *circuit]
	destinations *lru.Cache[string, *circuit]
}

var (
	_ contract.ToolCircuitBreaker       = (*CircuitBreaker)(nil)
	_ contract.FencedToolCircuitBreaker = (*CircuitBreaker)(nil)
)

// NewCircuitBreaker validates the configuration and returns a breaker with bounded state.
func NewCircuitBreaker(config CircuitBreakerConfig) (*CircuitBreaker, error) {
	if config.FailureThreshold <= 0 || config.Window <= 0 || config.OpenDuration <= 0 || config.MaxEntries <= 0 {
		return nil, fmt.Errorf("circuit breaker: failure threshold, window, open duration, and max entries must be positive")
	}
	if config.MinSamples < 0 || config.MinSamples > 0 && config.MinSamples < config.FailureThreshold {
		return nil, fmt.Errorf("circuit breaker: min samples must be zero or at least the failure threshold")
	}
	if config.SharedDestinationHealth && config.MaxDestinations <= 0 {
		return nil, fmt.Errorf("circuit breaker: max destinations must be positive with shared destination health")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	breaker := &CircuitBreaker{config: config, entries: lru.New[string, *circuit](config.MaxEntries, config.Now)}
	if config.SharedDestinationHealth {
		breaker.destinations = lru.New[string, *circuit](config.MaxDestinations, config.Now)
	}
	return breaker, nil
}

// Allow admits a call without a generation token; outcomes recorded through RecordSuccess/RecordFailure
// are attributed to the current generation.
func (breaker *CircuitBreaker) Allow(ctx context.Context, tenantID int64, toolID string) error {
	if err := validateKey(ctx, tenantID, toolID); err != nil {
		return err
	}
	now := breaker.config.Now()
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	_, err := breaker.admitLocked(breaker.entries, entryKey(tenantID, toolID), "tool.circuit", now)
	return err
}

// RecordSuccess records an unfenced success for the current generation.
func (breaker *CircuitBreaker) RecordSuccess(ctx context.Context, tenantID int64, toolID string) error {
	return breaker.recordUnfenced(ctx, tenantID, toolID, true)
}

// RecordFailure records an unfenced counted failure for the current generation.
func (breaker *CircuitBreaker) RecordFailure(ctx context.Context, tenantID int64, toolID string) error {
	return breaker.recordUnfenced(ctx, tenantID, toolID, false)
}

func (breaker *CircuitBreaker) recordUnfenced(ctx context.Context, tenantID int64, toolID string, success bool) error {
	if err := validateKey(ctx, tenantID, toolID); err != nil {
		return err
	}
	now := breaker.config.Now()
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	if entry, ok := breaker.entries.Get(entryKey(tenantID, toolID)); ok {
		breaker.settleLocked(entry, entry.generation, success, now)
	}
	return nil
}

// Admit checks the tenant×tool circuit and, when enabled, the shared destination circuit.
func (breaker *CircuitBreaker) Admit(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (int64, error) {
	tenantID, toolID := call.TenantContext.TenantID, call.ToolID
	if err := validateKey(ctx, tenantID, toolID); err != nil {
		return 0, err
	}
	now := breaker.config.Now()
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	var destinationGeneration int64
	if breaker.destinations != nil {
		host, err := destinationHost(definition.Endpoint)
		if err != nil {
			return 0, err
		}
		destinationGeneration, err = breaker.admitLocked(breaker.destinations, host, "tool.circuit.destination", now)
		if err != nil {
			return 0, err
		}
	}
	generation, err := breaker.admitLocked(breaker.entries, entryKey(tenantID, toolID), "tool.circuit", now)
	if err != nil {
		if breaker.destinations != nil {
			breaker.releaseProbeLocked(breaker.destinations, definition.Endpoint)
		}
		return 0, err
	}
	return packGenerations(generation, destinationGeneration), nil
}

// Settle records a fenced outcome; outcomes carrying an older generation are ignored.
func (breaker *CircuitBreaker) Settle(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, generation int64, success bool) error {
	tenantID, toolID := call.TenantContext.TenantID, call.ToolID
	if err := validateKey(ctx, tenantID, toolID); err != nil {
		return err
	}
	now := breaker.config.Now()
	entryGeneration, destinationGeneration := unpackGenerations(generation)
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	if entry, ok := breaker.entries.Get(entryKey(tenantID, toolID)); ok {
		breaker.settleLocked(entry, entryGeneration, success, now)
	}
	if breaker.destinations != nil {
		host, err := destinationHost(definition.Endpoint)
		if err != nil {
			return err
		}
		if entry, ok := breaker.destinations.Get(host); ok {
			breaker.settleLocked(entry, destinationGeneration, success, now)
		}
	}
	return nil
}

// State reports the tenant×tool circuit state for diagnostics and tests.
func (breaker *CircuitBreaker) State(tenantID int64, toolID string) (state string, generation int64) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	entry, ok := breaker.entries.Get(entryKey(tenantID, toolID))
	if !ok {
		return stateClosed.String(), 0
	}
	return entry.state.String(), entry.generation
}

func (breaker *CircuitBreaker) admitLocked(cache *lru.Cache[string, *circuit], key, scope string, now time.Time) (int64, error) {
	entry, ok := cache.Get(key)
	if !ok {
		entry = &circuit{windowStart: now}
		cache.Set(key, entry, 0)
	}
	switch entry.state {
	case stateClosed:
		breaker.expireWindowLocked(entry, now)
		return entry.generation, nil
	case stateOpen:
		remaining := entry.openedAt.Add(breaker.config.OpenDuration).Sub(now)
		if remaining > 0 {
			return 0, &limiter.LimitError{Err: domain.ErrCircuitOpen, Scope: scope, After: remaining}
		}
		entry.state, entry.probing = stateHalfOpen, true
		entry.generation++
		return entry.generation, nil
	default:
		if entry.probing {
			return 0, &limiter.LimitError{Err: domain.ErrCircuitOpen, Scope: scope + ".probe", After: breaker.config.OpenDuration}
		}
		entry.probing = true
		return entry.generation, nil
	}
}

func (breaker *CircuitBreaker) settleLocked(entry *circuit, generation int64, success bool, now time.Time) {
	if generation != entry.generation {
		return
	}
	switch entry.state {
	case stateClosed:
		breaker.expireWindowLocked(entry, now)
		entry.samples++
		if success {
			return
		}
		entry.failures++
		if entry.failures >= breaker.config.FailureThreshold && entry.samples >= breaker.config.MinSamples {
			breaker.openLocked(entry, now)
		}
	case stateHalfOpen:
		if success {
			entry.state, entry.probing = stateClosed, false
			entry.generation++
			entry.failures, entry.samples, entry.windowStart = 0, 0, now
			return
		}
		breaker.openLocked(entry, now)
	}
}

func (breaker *CircuitBreaker) openLocked(entry *circuit, now time.Time) {
	entry.state, entry.probing, entry.openedAt = stateOpen, false, now
	entry.generation++
	entry.failures, entry.samples = 0, 0
}

func (breaker *CircuitBreaker) expireWindowLocked(entry *circuit, now time.Time) {
	if now.Sub(entry.windowStart) >= breaker.config.Window {
		entry.failures, entry.samples, entry.windowStart = 0, 0, now
	}
}

// releaseProbeLocked undoes a destination probe admission when the tenant circuit rejected the same call.
func (breaker *CircuitBreaker) releaseProbeLocked(cache *lru.Cache[string, *circuit], endpoint string) {
	host, err := destinationHost(endpoint)
	if err != nil {
		return
	}
	if entry, ok := cache.Get(host); ok && entry.state == stateHalfOpen {
		entry.probing = false
	}
}

func validateKey(ctx context.Context, tenantID int64, toolID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(toolID) == "" {
		return fmt.Errorf("%w: tenant and tool are required", domain.ErrValidation)
	}
	return nil
}

func entryKey(tenantID int64, toolID string) string {
	return strconv.FormatInt(tenantID, 10) + "/" + toolID
}

func destinationHost(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%w: tool endpoint must be an absolute URL", domain.ErrValidation)
	}
	return strings.ToLower(parsed.Host), nil
}

// Generations change only on state transitions, so 32 bits each is ample.
func packGenerations(entry, destination int64) int64 {
	return entry<<32 | destination&0xffffffff
}

func unpackGenerations(packed int64) (entry, destination int64) {
	return packed >> 32, packed & 0xffffffff
}

// DefaultFailureClassifier counts transport failures, deadlines, retryable results, and (by default)
// invalid output; caller cancellation and typed tenant, authorization, and rate-limit errors never count.
type DefaultFailureClassifier struct {
	// CountValidationFailures treats ErrInvalidToolOutput as dependency failure; NewGovernedGateway defaults it to true.
	CountValidationFailures bool
}

var _ contract.ToolFailureClassifier = DefaultFailureClassifier{}

// CountsAsDependencyFailure applies the default classification.
func (classifier DefaultFailureClassifier) CountsAsDependencyFailure(_ context.Context, _ domain.ToolCall, _ domain.ToolResult, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrInvalidToolOutput):
		return classifier.CountValidationFailures
	case errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrRateLimited), errors.Is(err, domain.ErrNotFound):
		return false
	}
	return true
}
