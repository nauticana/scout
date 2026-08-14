package modelgateway

import (
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/limiter"
)

// FairCapacityConfig configures weighted, tenant-fair model capacity.
type FairCapacityConfig struct {
	Pool       string
	Slots      int
	MaxWaiters int
	Weight     func(domain.ModelRequest) int
}

// NewFairCapacityScheduler builds a long-lived capacity scheduler.
func NewFairCapacityScheduler(config FairCapacityConfig) (contract.CapacityScheduler, error) {
	config.Pool = strings.TrimSpace(config.Pool)
	if config.Pool == "" || config.Slots <= 0 || config.MaxWaiters <= 0 {
		return nil, fmt.Errorf("fair capacity: pool, slots, and max waiters must be positive")
	}
	return &limiter.SlotCapacityScheduler{
		Slots:  &limiter.FairSlotLimiter{Capacity: config.Slots, MaxWaiters: config.MaxWaiters},
		Pool:   config.Pool,
		Weight: config.Weight,
	}, nil
}
