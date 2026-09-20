package scout

import (
	"context"
	"sync/atomic"
	"time"

	keelconfig "github.com/nauticana/keel/config"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

const (
	agent_max_tokens          = "agent_max_tokens"
	agent_temperature         = "agent_temperature"
	agent_run_retention_days  = "agent_run_retention_days"
	agent_turn_rate           = "agent_turn_rate"
	agent_turn_burst          = "agent_turn_burst"
	agent_tool_rate           = "agent_tool_rate"
	agent_tool_burst          = "agent_tool_burst"
	agent_model_rate          = "agent_model_rate"
	agent_model_burst         = "agent_model_burst"
	agent_fleet_turn_rate     = "agent_fleet_turn_rate"
	agent_fleet_turn_burst    = "agent_fleet_turn_burst"
	agent_fleet_tool_rate     = "agent_fleet_tool_rate"
	agent_fleet_tool_burst    = "agent_fleet_tool_burst"
	agent_fleet_model_rate    = "agent_fleet_model_rate"
	agent_fleet_model_burst   = "agent_fleet_model_burst"
	agent_max_tenants         = "agent_max_tenants"
	agent_model_capacity_pool = "agent_model_capacity_pool"
	agent_model_capacity      = "agent_model_capacity"
	agent_model_max_waiters   = "agent_model_max_waiters"
	agent_max_scope_depth     = "agent_max_scope_depth"
	agent_max_delegation_hops = "agent_max_delegation_hops"
	agent_approval_deadline   = "agent_approval_deadline"
	agent_credential_ttl      = "agent_credential_ttl"
	agent_audit_page_size     = "agent_audit_page_size"
	agent_loop_max_iterations = "agent_loop_max_iterations"
	agent_loop_max_tool_calls = "agent_loop_max_tool_calls"
	agent_loop_max_tokens     = "agent_loop_max_tokens"
	agent_loop_max_cost       = "agent_loop_max_cost"
	agent_loop_max_repeats    = "agent_loop_max_repeats"
	agent_loop_deadline       = "agent_loop_deadline"
)

var _ keelconfig.ApplicationConfig = (*ScoutConfig)(nil)

var activeConfig atomic.Pointer[ScoutConfig]

func init() { activeConfig.Store(&ScoutConfig{}) }

// Config returns Scout's active runtime configuration.
func Config() *ScoutConfig { return activeConfig.Load() }

// SetConfig publishes an applied configuration; callers must not mutate it afterwards.
func SetConfig(c *ScoutConfig) { activeConfig.Store(c) }

// LoadConfig loads, applies, and publishes the Keel and Scout config sections.
func LoadConfig(ctx context.Context, db port.DatabaseRepository, nodeID int) error {
	rows, err := keelconfig.LoadRows(ctx, db, nodeID)
	if err != nil {
		return err
	}
	kc := &keelconfig.KeelConfig{}
	sc := &ScoutConfig{}
	for _, ac := range []keelconfig.ApplicationConfig{kc, sc} {
		if err = ac.Apply(rows); err != nil {
			return err
		}
	}
	keelconfig.SetConfig(kc)
	SetConfig(sc)
	return nil
}

// ScoutConfig holds Scout's repository-owned runtime configuration.
type ScoutConfig struct {
	keelconfig.AbstractConfig

	AgentMaxTokens         int      // agent_max_tokens          8192                          Max output tokens per agent model completion
	AgentTemperature       *float64 // agent_temperature        (none)                        Sampling temperature 0.0-2.0 for models with the sampling capability; nil sends none
	AgentRunRetentionDays  int      // agent_run_retention_days   0                             Days to keep agent run activity; 0 keeps it forever
	AgentTurnRate          float64  // agent_turn_rate            2                             Per-tenant admitted turns per second
	AgentTurnBurst         int      // agent_turn_burst           10                            Per-tenant turn burst
	AgentToolRate          float64  // agent_tool_rate            10                            Per-tenant tool calls per second
	AgentToolBurst         int      // agent_tool_burst           20                            Per-tenant tool-call burst
	AgentModelRate         float64  // agent_model_rate           2                             Per-tenant model calls per second
	AgentModelBurst        int      // agent_model_burst          5                             Per-tenant model-call burst
	AgentFleetTurnRate     float64  // agent_fleet_turn_rate      100                           Process-wide admitted turns per second
	AgentFleetTurnBurst    int      // agent_fleet_turn_burst     200                           Process-wide turn burst
	AgentFleetToolRate     float64  // agent_fleet_tool_rate      500                           Process-wide tool calls per second
	AgentFleetToolBurst    int      // agent_fleet_tool_burst     1000                          Process-wide tool-call burst
	AgentFleetModelRate    float64  // agent_fleet_model_rate     100                           Process-wide model calls per second
	AgentFleetModelBurst   int      // agent_fleet_model_burst    200                           Process-wide model-call burst
	AgentMaxTenants        int      // agent_max_tenants          4096                          Maximum in-memory tenant limiter entries
	AgentModelCapacityPool string   // agent_model_capacity_pool  shared                        Shared model capacity pool name
	AgentModelCapacity     int      // agent_model_capacity       32                            Concurrent model capacity slots
	AgentModelMaxWaiters   int      // agent_model_max_waiters    4096                          Maximum queued model requests
	AgentMaxScopeDepth     int      // agent_max_scope_depth      8                             Maximum scope-chain depth a release may compile over
	AgentMaxDelegationHops int      // agent_max_delegation_hops  4                             Maximum delegation hops in an authority chain
	AgentApprovalDeadline  int      // agent_approval_deadline    3600                          Seconds a reviewer has before escalation; 0 leaves a request open
	AgentCredentialTTL     int      // agent_credential_ttl       300                           Default lifetime in seconds of a just-in-time tool credential
	AgentAuditPageSize     int      // agent_audit_page_size      100                           Decision records returned per audit query page
	AgentLoopMaxIterations int      // agent_loop_max_iterations 12 Model decisions one tool loop step may make
	AgentLoopMaxToolCalls  int      // agent_loop_max_tool_calls 24 Governed tool calls one tool loop step may make
	AgentLoopMaxTokens     int      // agent_loop_max_tokens 200000 Input plus output tokens one tool loop step may spend
	AgentLoopMaxCost       int      // agent_loop_max_cost 0 Minor-unit cost one tool loop step may spend; 0 leaves cost to the turn budget
	AgentLoopMaxRepeats    int      // agent_loop_max_repeats 3 Identical calls of one tool before a loop is declared
	AgentLoopDeadline      int      // agent_loop_deadline 300 Wall-clock seconds one tool loop step may run
}

// ToolLoopLimits is the executor-level ceiling the agent_loop_* flags configure.
func (c *ScoutConfig) ToolLoopLimits() domain.ToolLoopLimits {
	return domain.ToolLoopLimits{
		MaxIterations: c.AgentLoopMaxIterations, MaxToolCalls: c.AgentLoopMaxToolCalls,
		MaxTokens: int64(c.AgentLoopMaxTokens), MaxCostMinorUnits: int64(c.AgentLoopMaxCost),
		MaxRepeatedCalls: c.AgentLoopMaxRepeats, Deadline: time.Duration(c.AgentLoopDeadline) * time.Second,
	}
}

// Apply parses Scout's section of the shared application configuration.
func (c *ScoutConfig) Apply(rows keelconfig.ConfigRows) error {
	c.AgentMaxTokens = c.Int(rows, agent_max_tokens)
	c.AgentTemperature = nil
	if c.String(rows, agent_temperature) != "" {
		temperature := c.Float(rows, agent_temperature)
		c.AgentTemperature = &temperature
	}
	c.AgentRunRetentionDays = c.Int(rows, agent_run_retention_days)
	c.AgentTurnRate = c.Float(rows, agent_turn_rate)
	c.AgentTurnBurst = c.Int(rows, agent_turn_burst)
	c.AgentToolRate = c.Float(rows, agent_tool_rate)
	c.AgentToolBurst = c.Int(rows, agent_tool_burst)
	c.AgentModelRate = c.Float(rows, agent_model_rate)
	c.AgentModelBurst = c.Int(rows, agent_model_burst)
	c.AgentFleetTurnRate = c.Float(rows, agent_fleet_turn_rate)
	c.AgentFleetTurnBurst = c.Int(rows, agent_fleet_turn_burst)
	c.AgentFleetToolRate = c.Float(rows, agent_fleet_tool_rate)
	c.AgentFleetToolBurst = c.Int(rows, agent_fleet_tool_burst)
	c.AgentFleetModelRate = c.Float(rows, agent_fleet_model_rate)
	c.AgentFleetModelBurst = c.Int(rows, agent_fleet_model_burst)
	c.AgentMaxTenants = c.Int(rows, agent_max_tenants)
	c.AgentModelCapacityPool = c.String(rows, agent_model_capacity_pool)
	c.AgentModelCapacity = c.Int(rows, agent_model_capacity)
	c.AgentModelMaxWaiters = c.Int(rows, agent_model_max_waiters)
	c.AgentMaxScopeDepth = c.Int(rows, agent_max_scope_depth)
	c.AgentMaxDelegationHops = c.Int(rows, agent_max_delegation_hops)
	c.AgentApprovalDeadline = c.Int(rows, agent_approval_deadline)
	c.AgentCredentialTTL = c.Int(rows, agent_credential_ttl)
	c.AgentAuditPageSize = c.Int(rows, agent_audit_page_size)
	c.AgentLoopMaxIterations = c.Int(rows, agent_loop_max_iterations)
	c.AgentLoopMaxToolCalls = c.Int(rows, agent_loop_max_tool_calls)
	c.AgentLoopMaxTokens = c.Int(rows, agent_loop_max_tokens)
	c.AgentLoopMaxCost = c.Int(rows, agent_loop_max_cost)
	c.AgentLoopMaxRepeats = c.Int(rows, agent_loop_max_repeats)
	c.AgentLoopDeadline = c.Int(rows, agent_loop_deadline)
	return c.ParseErr()
}
