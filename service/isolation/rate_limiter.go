package isolation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nauticana/keel/cache"
	keellimiter "github.com/nauticana/keel/limiter"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Scout's three admission lanes. keel scopes a rejection as "<lane>.partner" or "<lane>.fleet".
const (
	laneTurn  = "turn"
	laneTool  = "tool"
	laneModel = "model"
)

// RateLimit configures one admission lane in calls per second.
type RateLimit = keellimiter.RateLimit

// WindowLimit is one shared fixed-window admission scope.
type WindowLimit = keellimiter.WindowLimit

// RateLimiterConfig configures tenant and process-wide admission lanes.
type RateLimiterConfig struct {
	Turn, Tool, Model                RateLimit
	FleetTurn, FleetTool, FleetModel RateLimit
	MaxTenants                       int
}

// DistributedRateLimiterConfig configures shared tenant and fleet admission over a keel cache.
type DistributedRateLimiterConfig struct {
	Turn, Tool, Model                WindowLimit
	FleetTurn, FleetTool, FleetModel WindowLimit
	KeyPrefix                        string
	StoreTimeout                     time.Duration
	FallbackFraction                 float64
	FallbackMaxTenants               int
	RecoveryProbe                    time.Duration
}

// laneLimiter names Scout's lanes over a keel limiter; the tenant is keel's admission partner.
type laneLimiter struct{ keelport.RateLimiter }

var _ contract.TenantRateLimiter = laneLimiter{}

func (l laneLimiter) AllowTurn(ctx context.Context, tenant domain.TenantContext) error {
	return l.allow(ctx, laneTurn, tenant)
}

func (l laneLimiter) AllowToolCall(ctx context.Context, call domain.ToolCall) error {
	return l.allow(ctx, laneTool, call.TenantContext)
}

func (l laneLimiter) AllowModelCall(ctx context.Context, request domain.ModelRequest) error {
	return l.allow(ctx, laneModel, request.TenantContext)
}

func (l laneLimiter) allow(ctx context.Context, lane string, tenant domain.TenantContext) error {
	if tenant.TenantID <= 0 {
		return fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	err := l.Allow(ctx, lane, keelmodel.AdmissionSubject{PartnerID: tenant.TenantID})
	if errors.Is(err, keellimiter.ErrClosed) {
		return fmt.Errorf("%w: rate limiter is closed", domain.ErrConflict)
	}
	if !errors.Is(err, keellimiter.ErrRateLimited) {
		return err
	}
	var limit *keellimiter.LimitError
	if errors.As(err, &limit) {
		return &keellimiter.LimitError{Err: domain.ErrRateLimited, Scope: limit.Scope, After: limit.After}
	}
	return fmt.Errorf("%w: %v", domain.ErrRateLimited, err)
}

// NewTenantRateLimiter builds a long-lived in-process tenant admission limiter.
func NewTenantRateLimiter(config RateLimiterConfig) (contract.TenantRateLimiter, error) {
	lanes := map[string]keellimiter.Lane{
		laneTurn:  {Partner: config.Turn, Fleet: config.FleetTurn},
		laneTool:  {Partner: config.Tool, Fleet: config.FleetTool},
		laneModel: {Partner: config.Model, Fleet: config.FleetModel},
	}
	if err := validLanes(lanes, config.MaxTenants); err != nil {
		return nil, err
	}
	return laneLimiter{&keellimiter.LocalRateLimiter{Lanes: lanes, MaxPartners: config.MaxTenants}}, nil
}

// DistributedTenantRateLimiter enforces the three lanes across replicas and reports its degraded state.
type DistributedTenantRateLimiter struct {
	laneLimiter
	shared *keellimiter.DistributedRateLimiter
}

// NewDistributedTenantRateLimiter binds the lanes to a shared store that admits every scope atomically.
func NewDistributedTenantRateLimiter(store cache.CacheService, metrics keelport.MetricsRecorder, config DistributedRateLimiterConfig) (*DistributedTenantRateLimiter, error) {
	admitter, ok := store.(cache.MultiScopeAdmitter)
	if !ok {
		return nil, fmt.Errorf("distributed rate limiter: store %T cannot admit multiple scopes atomically", store)
	}
	shared, err := keellimiter.NewDistributedRateLimiter(admitter, keellimiter.DistributedRateLimiterConfig{
		Lanes: map[string]keellimiter.WindowLane{
			laneTurn:  {Partner: config.Turn, Fleet: config.FleetTurn},
			laneTool:  {Partner: config.Tool, Fleet: config.FleetTool},
			laneModel: {Partner: config.Model, Fleet: config.FleetModel},
		},
		KeyPrefix:           config.KeyPrefix,
		StoreTimeout:        config.StoreTimeout,
		FallbackFraction:    config.FallbackFraction,
		FallbackMaxPartners: config.FallbackMaxTenants,
		RecoveryProbe:       config.RecoveryProbe,
	})
	if err != nil {
		return nil, err
	}
	shared.Metrics = metrics
	return &DistributedTenantRateLimiter{laneLimiter: laneLimiter{shared}, shared: shared}, nil
}

// Degraded reports whether admission currently runs on the local fallback.
func (l *DistributedTenantRateLimiter) Degraded() bool { return l.shared.Degraded() }

// Close stops admission; it is idempotent.
func (l *DistributedTenantRateLimiter) Close() error { return l.shared.Close() }

func validLanes(lanes map[string]keellimiter.Lane, maxTenants int) error {
	if maxTenants <= 0 {
		return fmt.Errorf("rate limiter: max tenants must be positive")
	}
	for _, lane := range lanes {
		for _, limit := range []RateLimit{lane.Partner, lane.Fleet} {
			if limit.PerSecond < 0 || math.IsNaN(limit.PerSecond) || math.IsInf(limit.PerSecond, 0) || limit.Burst < 0 || (limit.PerSecond == 0) != (limit.Burst == 0) {
				return fmt.Errorf("rate limiter: rate and burst must both be zero or positive")
			}
		}
	}
	return nil
}
