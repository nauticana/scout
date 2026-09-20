package scout

import (
	"context"
	"fmt"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/isolation"
	"github.com/nauticana/scout/service/modelgateway"
)

// ControlSet is the process-wide admission pair every governed path shares: one tenant
// rate limiter and one model capacity scheduler. Sharing them is the point; a second set
// in the same process doubles every limit.
type ControlSet struct {
	RateLimiter contract.TenantRateLimiter
	Capacity    contract.CapacityScheduler
}

// NewControlSet builds the pair from the agent_*_rate, agent_*_burst, agent_max_tenants and
// agent_model_capacity* flags.
func NewControlSet(cfg *ScoutConfig) (ControlSet, error) {
	if cfg == nil {
		return ControlSet{}, fmt.Errorf("%w: control set needs a configuration", domain.ErrValidation)
	}
	limit := func(perSecond float64, burst int) isolation.RateLimit {
		return isolation.RateLimit{PerSecond: perSecond, Burst: burst}
	}
	rateLimiter, err := isolation.NewTenantRateLimiter(isolation.RateLimiterConfig{
		Turn:       limit(cfg.AgentTurnRate, cfg.AgentTurnBurst),
		Tool:       limit(cfg.AgentToolRate, cfg.AgentToolBurst),
		Model:      limit(cfg.AgentModelRate, cfg.AgentModelBurst),
		FleetTurn:  limit(cfg.AgentFleetTurnRate, cfg.AgentFleetTurnBurst),
		FleetTool:  limit(cfg.AgentFleetToolRate, cfg.AgentFleetToolBurst),
		FleetModel: limit(cfg.AgentFleetModelRate, cfg.AgentFleetModelBurst),
		MaxTenants: cfg.AgentMaxTenants,
	})
	if err != nil {
		return ControlSet{}, fmt.Errorf("agent rate limiter: %w", err)
	}
	capacity, err := modelgateway.NewFairCapacityScheduler(modelgateway.FairCapacityConfig{
		Pool: cfg.AgentModelCapacityPool, Slots: cfg.AgentModelCapacity, MaxWaiters: cfg.AgentModelMaxWaiters,
	})
	if err != nil {
		return ControlSet{}, fmt.Errorf("agent model capacity: %w", err)
	}
	return ControlSet{RateLimiter: rateLimiter, Capacity: capacity}, nil
}

// ActiveControls follows the active configuration: the pair is rebuilt when SetConfig
// publishes a new one, and a configuration it cannot be built from fails every admission.
func ActiveControls() ControlSet {
	return ControlSet{RateLimiter: activeRateLimiter{}, Capacity: activeCapacityScheduler{}}
}

var activeControls struct {
	sync.Mutex
	config   *ScoutConfig
	controls ControlSet
	err      error
}

func currentControls() (ControlSet, error) {
	cfg := Config()
	activeControls.Lock()
	defer activeControls.Unlock()
	if activeControls.config != cfg {
		activeControls.controls, activeControls.err = NewControlSet(cfg)
		activeControls.config = cfg
	}
	return activeControls.controls, activeControls.err
}

type activeRateLimiter struct{}

func (activeRateLimiter) AllowTurn(ctx context.Context, tenant domain.TenantContext) error {
	controls, err := currentControls()
	if err != nil {
		return err
	}
	return controls.RateLimiter.AllowTurn(ctx, tenant)
}

func (activeRateLimiter) AllowToolCall(ctx context.Context, call domain.ToolCall) error {
	controls, err := currentControls()
	if err != nil {
		return err
	}
	return controls.RateLimiter.AllowToolCall(ctx, call)
}

func (activeRateLimiter) AllowModelCall(ctx context.Context, request domain.ModelRequest) error {
	controls, err := currentControls()
	if err != nil {
		return err
	}
	return controls.RateLimiter.AllowModelCall(ctx, request)
}

type activeCapacityScheduler struct{}

func (activeCapacityScheduler) Acquire(ctx context.Context, request domain.ModelRequest, selection domain.ModelSelection) (contract.CapacityLease, error) {
	controls, err := currentControls()
	if err != nil {
		return nil, err
	}
	return controls.Capacity.Acquire(ctx, request, selection)
}

var (
	_ contract.TenantRateLimiter = activeRateLimiter{}
	_ contract.CapacityScheduler = activeCapacityScheduler{}
)
