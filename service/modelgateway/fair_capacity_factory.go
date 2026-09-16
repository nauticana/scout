package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	keellimiter "github.com/nauticana/keel/limiter"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// FairCapacityConfig configures weighted, tenant-fair model capacity.
type FairCapacityConfig struct {
	Pool       string
	Slots      int
	MaxWaiters int
	Weight     func(domain.ModelRequest) int
}

// NewFairCapacityScheduler builds a long-lived capacity scheduler over keel's fair slots.
func NewFairCapacityScheduler(config FairCapacityConfig) (contract.CapacityScheduler, error) {
	config.Pool = strings.TrimSpace(config.Pool)
	if config.Pool == "" || config.Slots <= 0 || config.MaxWaiters <= 0 {
		return nil, fmt.Errorf("fair capacity: pool, slots, and max waiters must be positive")
	}
	return &slotScheduler{
		slots:  &keellimiter.FairSlotLimiter{Capacity: config.Slots, MaxWaiters: config.MaxWaiters},
		pool:   config.Pool,
		weight: config.Weight,
	}, nil
}

// slotScheduler adapts keel's fair slots to one inference pool; nil weight charges one slot.
type slotScheduler struct {
	slots  *keellimiter.FairSlotLimiter
	pool   string
	weight func(domain.ModelRequest) int
}

var _ contract.CapacityScheduler = (*slotScheduler)(nil)

func (s *slotScheduler) Acquire(ctx context.Context, request domain.ModelRequest, _ domain.ModelSelection) (contract.CapacityLease, error) {
	weight := 1
	if s.weight != nil {
		weight = s.weight(request)
	}
	lease, err := s.slots.AcquireWeighted(ctx, keelmodel.AdmissionSubject{PartnerID: request.TenantContext.TenantID}, weight)
	if err != nil {
		switch {
		case errors.Is(err, keellimiter.ErrRateLimited):
			return nil, fmt.Errorf("%w: %v", domain.ErrRateLimited, err)
		case errors.Is(err, keellimiter.ErrInvalidSubject):
			return nil, fmt.Errorf("%w: %v", domain.ErrValidation, err)
		}
		return nil, err
	}
	return &slotLease{pool: s.pool, slot: lease}, nil
}

type slotLease struct {
	pool string
	slot keelport.ConcurrencyLease
}

var _ contract.CapacityLease = (*slotLease)(nil)

func (l *slotLease) Pool() string { return l.pool }

func (l *slotLease) Release(context.Context, domain.Usage) error { return l.slot.Release() }
