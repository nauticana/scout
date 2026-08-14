package modelgateway

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AdaptiveCapacityScheduler discovers provider capacity by AIMD: the limit
// grows by one after a healthy latency window and halves when the window's
// average exceeds SlowFactor times the observed floor.
type AdaptiveCapacityScheduler struct {
	Pool     string
	Min, Max int
	// SlowFactor marks a window slow at SlowFactor×the fastest observed call; default 1.5.
	SlowFactor float64
	Now        func() time.Time

	mu       sync.Mutex
	once     sync.Once
	limit    int
	inFlight int
	minRTT   time.Duration
	window   time.Duration
	samples  int
	changed  chan struct{}
}

var _ contract.CapacityScheduler = (*AdaptiveCapacityScheduler)(nil)

func (scheduler *AdaptiveCapacityScheduler) init() error {
	if scheduler.Min <= 0 || scheduler.Max < scheduler.Min || strings.TrimSpace(scheduler.Pool) == "" {
		return fmt.Errorf("adaptive capacity scheduler: pool and bounds 0 < min <= max are required")
	}
	scheduler.once.Do(func() {
		scheduler.limit = scheduler.Min
		scheduler.changed = make(chan struct{})
		if scheduler.SlowFactor <= 1 || math.IsNaN(scheduler.SlowFactor) || math.IsInf(scheduler.SlowFactor, 0) {
			scheduler.SlowFactor = 1.5
		}
		if scheduler.Now == nil {
			scheduler.Now = time.Now
		}
	})
	return nil
}

// Acquire blocks without polling until a slot frees or ctx is canceled.
func (scheduler *AdaptiveCapacityScheduler) Acquire(ctx context.Context, request domain.ModelRequest, _ domain.ModelSelection) (contract.CapacityLease, error) {
	if err := scheduler.init(); err != nil {
		return nil, err
	}
	if request.TenantContext.TenantID <= 0 {
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	for {
		scheduler.mu.Lock()
		if err := ctx.Err(); err != nil {
			scheduler.mu.Unlock()
			return nil, err
		}
		if scheduler.inFlight < scheduler.limit {
			scheduler.inFlight++
			scheduler.mu.Unlock()
			return &adaptiveLease{scheduler: scheduler, started: scheduler.Now()}, nil
		}
		wait := scheduler.changed
		scheduler.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Limit reports the current concurrency limit for autoscaling signals.
func (scheduler *AdaptiveCapacityScheduler) Limit() int {
	if err := scheduler.init(); err != nil {
		return 0
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.limit
}

func (scheduler *AdaptiveCapacityScheduler) complete(rtt time.Duration) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	defer scheduler.broadcast()
	scheduler.inFlight--
	if rtt <= 0 {
		rtt = time.Nanosecond
	}

	// minRTT is the healthy baseline; the window damps single-call noise.
	if scheduler.minRTT == 0 || rtt < scheduler.minRTT {
		scheduler.minRTT = rtt
	}
	scheduler.window += rtt
	scheduler.samples++
	if scheduler.samples < scheduler.limit {
		return
	}
	average := scheduler.window / time.Duration(scheduler.samples)
	if float64(average) > scheduler.SlowFactor*float64(scheduler.minRTT) {
		scheduler.limit = max(scheduler.Min, (scheduler.limit+1)/2)
	} else {
		scheduler.limit = min(scheduler.Max, scheduler.limit+1)
	}
	// Samples measured under the old limit must not drive the next decision.
	scheduler.window, scheduler.samples = 0, 0
}

func (scheduler *AdaptiveCapacityScheduler) broadcast() {
	close(scheduler.changed)
	scheduler.changed = make(chan struct{})
}
