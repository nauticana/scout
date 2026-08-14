package isolation

import (
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/internal/limiter"
)

// RateLimit configures one admission lane in calls per second.
type RateLimit struct {
	PerSecond float64
	Burst     int
}

// RateLimiterConfig configures tenant and process-wide admission lanes.
type RateLimiterConfig struct {
	Turn, Tool, Model                RateLimit
	FleetTurn, FleetTool, FleetModel RateLimit
	MaxTenants                       int
}

// NewTenantRateLimiter builds a long-lived tenant admission limiter.
func NewTenantRateLimiter(config RateLimiterConfig) (contract.TenantRateLimiter, error) {
	limits := []RateLimit{config.Turn, config.Tool, config.Model, config.FleetTurn, config.FleetTool, config.FleetModel}
	for _, limit := range limits {
		if !validRateLimit(limit) {
			return nil, fmt.Errorf("rate limiter: rate and burst must both be zero or positive")
		}
	}
	if config.MaxTenants <= 0 {
		return nil, fmt.Errorf("rate limiter: max tenants must be positive")
	}
	convert := func(limit RateLimit) limiter.RateLimit {
		return limiter.RateLimit{PerSecond: limit.PerSecond, Burst: limit.Burst}
	}
	return &limiter.TenantRateLimiter{
		Turn: convert(config.Turn), Tool: convert(config.Tool), Model: convert(config.Model),
		FleetTurn: convert(config.FleetTurn), FleetTool: convert(config.FleetTool), FleetModel: convert(config.FleetModel),
		MaxTenants: config.MaxTenants,
	}, nil
}

func validRateLimit(limit RateLimit) bool {
	return limit.PerSecond == 0 && limit.Burst == 0 || limit.PerSecond > 0 && limit.Burst > 0
}
