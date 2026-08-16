package release

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BoundedShadowSampler mirrors a stable-hash share of requests to a shadow
// release and refuses once the shadow-to-live ratio in the current window
// exceeds MaxAmplification. Callers invoke it only after authentication and
// redaction; the mirrored copy must never publish output or run tools.
type BoundedShadowSampler struct {
	Percentage       int
	MaxAmplification float64
	Window           time.Duration
	Now              func() time.Time

	mu      sync.Mutex
	windows map[string]*shadowWindow
}

type shadowWindow struct {
	startedAt time.Time
	live      int64
	shadow    int64
}

var _ contract.ShadowTrafficSampler = (*BoundedShadowSampler)(nil)

func (sampler *BoundedShadowSampler) now() time.Time {
	if sampler.Now != nil {
		return sampler.Now()
	}
	return time.Now()
}

func (sampler *BoundedShadowSampler) validate() error {
	if sampler.Percentage < 0 || sampler.Percentage > 100 || sampler.MaxAmplification <= 0 || sampler.Window <= 0 {
		return fmt.Errorf("%w: shadow sampler needs a percentage in [0,100], a positive amplification bound, and a positive window", domain.ErrValidation)
	}
	return nil
}

func (sampler *BoundedShadowSampler) window(platformVersion string, now time.Time) *shadowWindow {
	if sampler.windows == nil {
		sampler.windows = map[string]*shadowWindow{}
	}
	current := sampler.windows[platformVersion]
	if current == nil || now.Sub(current.startedAt) >= sampler.Window {
		current = &shadowWindow{startedAt: now}
		sampler.windows[platformVersion] = current
	}
	return current
}

func (sampler *BoundedShadowSampler) Sample(ctx context.Context, platformVersion string, request domain.TurnRequest) (bool, error) {
	if err := sampler.validate(); err != nil {
		return false, err
	}
	if request.TenantContext.TenantID <= 0 || request.RequestID == "" {
		return false, fmt.Errorf("%w: shadow sampling needs an authenticated tenant and request id", domain.ErrValidation)
	}
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	current := sampler.window(platformVersion, sampler.now())
	current.live++
	if !CanarySelected(request.TenantContext.TenantID, platformVersion, request.RequestID, sampler.Percentage) {
		return false, nil
	}
	if float64(current.shadow+1)/float64(current.live) > sampler.MaxAmplification {
		return false, nil
	}
	current.shadow++
	return true, nil
}

func (sampler *BoundedShadowSampler) Amplification(ctx context.Context, platformVersion string) (float64, error) {
	if err := sampler.validate(); err != nil {
		return 0, err
	}
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	current := sampler.window(platformVersion, sampler.now())
	if current.live == 0 {
		return 0, nil
	}
	return float64(current.shadow) / float64(current.live), nil
}
