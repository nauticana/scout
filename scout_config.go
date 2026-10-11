package scout

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	keelconfig "github.com/nauticana/keel/config"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

const (
	agent_max_tokens                 = "agent_max_tokens"
	agent_temperature                = "agent_temperature"
	agent_run_retention_days         = "agent_run_retention_days"
	agent_turn_rate                  = "agent_turn_rate"
	agent_turn_burst                 = "agent_turn_burst"
	agent_tool_rate                  = "agent_tool_rate"
	agent_tool_burst                 = "agent_tool_burst"
	agent_model_rate                 = "agent_model_rate"
	agent_model_burst                = "agent_model_burst"
	agent_fleet_turn_rate            = "agent_fleet_turn_rate"
	agent_fleet_turn_burst           = "agent_fleet_turn_burst"
	agent_fleet_tool_rate            = "agent_fleet_tool_rate"
	agent_fleet_tool_burst           = "agent_fleet_tool_burst"
	agent_fleet_model_rate           = "agent_fleet_model_rate"
	agent_fleet_model_burst          = "agent_fleet_model_burst"
	agent_max_tenants                = "agent_max_tenants"
	agent_model_capacity_pool        = "agent_model_capacity_pool"
	agent_model_capacity             = "agent_model_capacity"
	agent_model_max_waiters          = "agent_model_max_waiters"
	agent_max_scope_depth            = "agent_max_scope_depth"
	agent_max_delegation_hops        = "agent_max_delegation_hops"
	agent_approval_deadline          = "agent_approval_deadline"
	agent_credential_ttl             = "agent_credential_ttl"
	agent_audit_page_size            = "agent_audit_page_size"
	agent_loop_max_iterations        = "agent_loop_max_iterations"
	agent_loop_max_tool_calls        = "agent_loop_max_tool_calls"
	agent_loop_max_tokens            = "agent_loop_max_tokens"
	agent_loop_max_cost              = "agent_loop_max_cost"
	agent_loop_max_repeats           = "agent_loop_max_repeats"
	agent_loop_deadline              = "agent_loop_deadline"
	agent_state_bucket               = "agent_state_bucket"
	agent_state_max_bytes            = "agent_state_max_bytes"
	agent_queue_partitions           = "agent_queue_partitions"
	agent_queue_shards               = "agent_queue_shards"
	agent_queue_max_attempts         = "agent_queue_max_attempts"
	agent_queue_lease                = "agent_queue_lease"
	agent_queue_batch                = "agent_queue_batch"
	agent_session_cache_size         = "agent_session_cache_size"
	agent_session_cache_ttl          = "agent_session_cache_ttl"
	agent_graph_cache_size           = "agent_graph_cache_size"
	agent_graph_cache_ttl            = "agent_graph_cache_ttl"
	agent_step_claim_lease           = "agent_step_claim_lease"
	agent_turn_max_steps             = "agent_turn_max_steps"
	agent_tool_timeout               = "agent_tool_timeout"
	agent_tool_max_attempts          = "agent_tool_max_attempts"
	agent_guardrail_max_input_bytes  = "agent_guardrail_max_input_bytes"
	agent_guardrail_max_output_bytes = "agent_guardrail_max_output_bytes"
	agent_model_region               = "agent_model_region"
	agent_pilot_max_members          = "agent_pilot_max_members"
	agent_pilot_position_timeout     = "agent_pilot_position_timeout"
	agent_studio_max_agents_per_type = "agent_studio_max_agents_per_type"
	agent_governance_required_fields = "agent_governance_required_fields"
	agent_evaluation_max_open_runs   = "agent_evaluation_max_open_runs"
	agent_evaluation_concurrency     = "agent_evaluation_concurrency"
	agent_evaluation_max_run_cost    = "agent_evaluation_max_run_cost"
	agent_evaluation_currency        = "agent_evaluation_currency"
	agent_evaluation_lease           = "agent_evaluation_lease"
	agent_evaluation_batch           = "agent_evaluation_batch"
	agent_evaluation_max_attempts    = "agent_evaluation_max_attempts"
	agent_evaluation_case_max_bytes  = "agent_evaluation_case_max_bytes"
	agent_evaluation_max_cases       = "agent_evaluation_max_cases"
	agent_activation_batch           = "agent_activation_batch"
	agent_activation_poll            = "agent_activation_poll"
	agent_activation_max_attempts    = "agent_activation_max_attempts"
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

	AgentMaxTokens               int      // agent_max_tokens          8192                          Max output tokens per agent model completion
	AgentTemperature             *float64 // agent_temperature        (none)                        Sampling temperature 0.0-2.0 for models with the sampling capability; nil sends none
	AgentRunRetentionDays        int      // agent_run_retention_days   0                             Days to keep agent run activity; 0 keeps it forever
	AgentTurnRate                float64  // agent_turn_rate            2                             Per-tenant admitted turns per second
	AgentTurnBurst               int      // agent_turn_burst           10                            Per-tenant turn burst
	AgentToolRate                float64  // agent_tool_rate            10                            Per-tenant tool calls per second
	AgentToolBurst               int      // agent_tool_burst           20                            Per-tenant tool-call burst
	AgentModelRate               float64  // agent_model_rate           2                             Per-tenant model calls per second
	AgentModelBurst              int      // agent_model_burst          5                             Per-tenant model-call burst
	AgentFleetTurnRate           float64  // agent_fleet_turn_rate      100                           Process-wide admitted turns per second
	AgentFleetTurnBurst          int      // agent_fleet_turn_burst     200                           Process-wide turn burst
	AgentFleetToolRate           float64  // agent_fleet_tool_rate      500                           Process-wide tool calls per second
	AgentFleetToolBurst          int      // agent_fleet_tool_burst     1000                          Process-wide tool-call burst
	AgentFleetModelRate          float64  // agent_fleet_model_rate     100                           Process-wide model calls per second
	AgentFleetModelBurst         int      // agent_fleet_model_burst    200                           Process-wide model-call burst
	AgentMaxTenants              int      // agent_max_tenants          4096                          Maximum in-memory tenant limiter entries
	AgentModelCapacityPool       string   // agent_model_capacity_pool  shared                        Shared model capacity pool name
	AgentModelCapacity           int      // agent_model_capacity       32                            Concurrent model capacity slots
	AgentModelMaxWaiters         int      // agent_model_max_waiters    4096                          Maximum queued model requests
	AgentMaxScopeDepth           int      // agent_max_scope_depth      8                             Maximum scope-chain depth a release may compile over
	AgentMaxDelegationHops       int      // agent_max_delegation_hops  4                             Maximum delegation hops in an authority chain
	AgentApprovalDeadline        int      // agent_approval_deadline    3600                          Seconds a reviewer has before escalation; 0 leaves a request open
	AgentCredentialTTL           int      // agent_credential_ttl       300                           Default lifetime in seconds of a just-in-time tool credential
	AgentAuditPageSize           int      // agent_audit_page_size      100                           Decision records returned per audit query page
	AgentLoopMaxIterations       int      // agent_loop_max_iterations 12 Model decisions one tool loop step may make
	AgentLoopMaxToolCalls        int      // agent_loop_max_tool_calls 24 Governed tool calls one tool loop step may make
	AgentLoopMaxTokens           int      // agent_loop_max_tokens 200000 Input plus output tokens one tool loop step may spend
	AgentLoopMaxCost             int      // agent_loop_max_cost 0 Minor-unit cost one tool loop step may spend; 0 leaves cost to the turn budget
	AgentLoopMaxRepeats          int      // agent_loop_max_repeats 3 Identical calls of one tool before a loop is declared
	AgentLoopDeadline            int      // agent_loop_deadline 300 Wall-clock seconds one tool loop step may run
	AgentStateBucket             string   // agent_state_bucket (none) Private bucket for turn input and conversation state; empty refuses to compose the data plane
	AgentStateMaxBytes           int      // agent_state_max_bytes 4194304 Largest single state or input payload in bytes
	AgentQueuePartitions         int      // agent_queue_partitions 64 Fixed turn-queue partition pool; changing it reshuffles tenants
	AgentQueueShards             int      // agent_queue_shards 4 Partitions one tenant spreads over; at most agent_queue_partitions
	AgentQueueMaxAttempts        int      // agent_queue_max_attempts 5 Deliveries of one turn before it is dead-lettered
	AgentQueueLease              int      // agent_queue_lease 900 Seconds one claimed turn stays leased to its worker; keep it above agent_loop_deadline
	AgentQueueBatch              int      // agent_queue_batch 8 Turns one worker tick claims
	AgentSessionCacheSize        int      // agent_session_cache_size 4096 Conversations held in the in-memory session cache
	AgentSessionCacheTTL         int      // agent_session_cache_ttl 300 Seconds an in-memory session snapshot lives
	AgentGraphCacheSize          int      // agent_graph_cache_size 1024 Execution graphs held in the in-memory graph cache
	AgentGraphCacheTTL           int      // agent_graph_cache_ttl 3600 Seconds an in-memory execution graph lives
	AgentStepClaimLease          int      // agent_step_claim_lease 360 Seconds a step claim blocks other workers; keep it above agent_loop_deadline
	AgentTurnMaxSteps            int      // agent_turn_max_steps 16 Graph steps one turn may execute
	AgentToolTimeout             int      // agent_tool_timeout 30 Seconds one governed tool call may run when the tool registers none
	AgentToolMaxAttempts         int      // agent_tool_max_attempts 3 Deliveries of one tool call when the tool registers none
	AgentGuardrailMaxInputBytes  int      // agent_guardrail_max_input_bytes 262144 Baseline byte ceiling on turn input, tool arguments, and retrieved content
	AgentGuardrailMaxOutputBytes int      // agent_guardrail_max_output_bytes 1048576 Baseline byte ceiling on model and tool output
	AgentModelRegion             string   // agent_model_region (none) Residency region stamped on a candidate model with no route row; empty leaves it unknown
	AgentPilotMaxMembers         int      // agent_pilot_max_members 200 Open pilot memberships one agent may hold, which bounds what starting a conversation reads; 0 refuses every pilot add
	AgentPilotPositionTimeout    int      // agent_pilot_position_timeout 2 Seconds one product position lookup may take while a new conversation chooses its release; 0 uses Scout's default
	AgentStudioMaxAgentsPerType  int      // agent_studio_max_agents_per_type 20 Agents of one type a tenant may keep, archived ones included; 0 refuses every Studio create

	// agent_governance_required_fields (none) Charter governance fields a release must carry, comma-separated;
	// empty (nil) requires every field Charter's AgentDefinition requires, none (an empty list) requires none.
	AgentGovernanceRequiredFields []domain.GovernanceField
	AgentEvaluationMaxOpenRuns    int    // agent_evaluation_max_open_runs 4 Queued and running evaluations one agent may hold; 0 refuses every evaluation
	AgentEvaluationConcurrency    int    // agent_evaluation_concurrency 4 Examples of one evaluation run in flight; below 2 runs them one at a time
	AgentEvaluationMaxRunCost     int    // agent_evaluation_max_run_cost 10000 Minor units of agent_evaluation_currency one evaluation run may spend before it stops early
	AgentEvaluationCurrency       string // agent_evaluation_currency (none) ISO currency every evaluation cost is priced in; empty refuses to compose the evaluation worker
	AgentEvaluationLease          int    // agent_evaluation_lease 900 Seconds each recorded result extends an evaluation run's lease; keep it above agent_loop_deadline
	AgentEvaluationBatch          int    // agent_evaluation_batch 4 Evaluation runs one worker tick claims
	AgentEvaluationMaxAttempts    int    // agent_evaluation_max_attempts 3 Claims of one evaluation run before an expired lease fails it
	AgentEvaluationCaseMaxBytes   int    // agent_evaluation_case_max_bytes 32768 Largest saved test case input or expected behavior in bytes
	AgentEvaluationMaxCases       int    // agent_evaluation_max_cases 500 Saved test cases one golden set may hold
	AgentActivationBatch          int    // agent_activation_batch 20 Due scheduled release activations one worker poll handles
	AgentActivationPoll           int    // agent_activation_poll 30 Seconds between polls of the release activation worker
	AgentActivationMaxAttempts    int    // agent_activation_max_attempts 5 Attempts of a scheduled activation that fails transiently before it is refused
}

// ToolLoopLimits is the executor-level ceiling the agent_loop_* flags configure.
func (c *ScoutConfig) ToolLoopLimits() domain.ToolLoopLimits {
	return domain.ToolLoopLimits{
		MaxIterations: c.AgentLoopMaxIterations, MaxToolCalls: c.AgentLoopMaxToolCalls,
		MaxTokens: int64(c.AgentLoopMaxTokens), MaxCostMinorUnits: int64(c.AgentLoopMaxCost),
		MaxRepeatedCalls: c.AgentLoopMaxRepeats, Deadline: time.Duration(c.AgentLoopDeadline) * time.Second,
	}
}

// DataPlaneSettings is what the agent_state_*, agent_queue_*, cache, step, tool, and guardrail flags configure.
func (c *ScoutConfig) DataPlaneSettings() domain.DataPlaneSettings {
	seconds := func(value int) time.Duration { return time.Duration(value) * time.Second }
	return domain.DataPlaneSettings{
		StateBucket: c.AgentStateBucket, StateMaxBytes: int64(c.AgentStateMaxBytes),
		QueuePartitions: c.AgentQueuePartitions, QueueShards: c.AgentQueueShards, QueueMaxAttempts: c.AgentQueueMaxAttempts,
		QueueLease: seconds(c.AgentQueueLease), QueueBatch: c.AgentQueueBatch,
		SessionCacheSize: c.AgentSessionCacheSize, SessionCacheTTL: seconds(c.AgentSessionCacheTTL),
		GraphCacheSize: c.AgentGraphCacheSize, GraphCacheTTL: seconds(c.AgentGraphCacheTTL),
		StepClaimLease: seconds(c.AgentStepClaimLease), TurnMaxSteps: c.AgentTurnMaxSteps,
		ToolTimeout: seconds(c.AgentToolTimeout), ToolMaxAttempts: c.AgentToolMaxAttempts,
		GuardrailMaxInputBytes: c.AgentGuardrailMaxInputBytes, GuardrailMaxOutputBytes: c.AgentGuardrailMaxOutputBytes,
		ModelRegion: c.AgentModelRegion,
	}
}

// ReleaseActivationSettings is what the agent_activation_* flags configure.
func (c *ScoutConfig) ReleaseActivationSettings() domain.ReleaseActivationSettings {
	return domain.ReleaseActivationSettings{
		Batch: c.AgentActivationBatch, PollPeriod: time.Duration(c.AgentActivationPoll) * time.Second,
		MaxAttempts: c.AgentActivationMaxAttempts,
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
	c.AgentStateBucket = c.String(rows, agent_state_bucket)
	c.AgentStateMaxBytes = c.Int(rows, agent_state_max_bytes)
	c.AgentQueuePartitions = c.Int(rows, agent_queue_partitions)
	c.AgentQueueShards = c.Int(rows, agent_queue_shards)
	c.AgentQueueMaxAttempts = c.Int(rows, agent_queue_max_attempts)
	c.AgentQueueLease = c.Int(rows, agent_queue_lease)
	c.AgentQueueBatch = c.Int(rows, agent_queue_batch)
	c.AgentSessionCacheSize = c.Int(rows, agent_session_cache_size)
	c.AgentSessionCacheTTL = c.Int(rows, agent_session_cache_ttl)
	c.AgentGraphCacheSize = c.Int(rows, agent_graph_cache_size)
	c.AgentGraphCacheTTL = c.Int(rows, agent_graph_cache_ttl)
	c.AgentStepClaimLease = c.Int(rows, agent_step_claim_lease)
	c.AgentTurnMaxSteps = c.Int(rows, agent_turn_max_steps)
	c.AgentToolTimeout = c.Int(rows, agent_tool_timeout)
	c.AgentToolMaxAttempts = c.Int(rows, agent_tool_max_attempts)
	c.AgentGuardrailMaxInputBytes = c.Int(rows, agent_guardrail_max_input_bytes)
	c.AgentGuardrailMaxOutputBytes = c.Int(rows, agent_guardrail_max_output_bytes)
	c.AgentModelRegion = c.String(rows, agent_model_region)
	c.AgentPilotMaxMembers = c.Int(rows, agent_pilot_max_members)
	c.AgentPilotPositionTimeout = c.Int(rows, agent_pilot_position_timeout)
	c.AgentStudioMaxAgentsPerType = c.Int(rows, agent_studio_max_agents_per_type)
	c.AgentActivationBatch = c.Int(rows, agent_activation_batch)
	c.AgentActivationPoll = c.Int(rows, agent_activation_poll)
	c.AgentActivationMaxAttempts = c.Int(rows, agent_activation_max_attempts)
	c.AgentEvaluationMaxOpenRuns = c.Int(rows, agent_evaluation_max_open_runs)
	c.AgentEvaluationConcurrency = c.Int(rows, agent_evaluation_concurrency)
	c.AgentEvaluationMaxRunCost = c.Int(rows, agent_evaluation_max_run_cost)
	c.AgentEvaluationCurrency = c.String(rows, agent_evaluation_currency)
	c.AgentEvaluationLease = c.Int(rows, agent_evaluation_lease)
	c.AgentEvaluationBatch = c.Int(rows, agent_evaluation_batch)
	c.AgentEvaluationMaxAttempts = c.Int(rows, agent_evaluation_max_attempts)
	c.AgentEvaluationCaseMaxBytes = c.Int(rows, agent_evaluation_case_max_bytes)
	c.AgentEvaluationMaxCases = c.Int(rows, agent_evaluation_max_cases)
	required, err := governanceFields(c.String(rows, agent_governance_required_fields))
	c.AgentGovernanceRequiredFields = required
	return errors.Join(c.ParseErr(), err)
}

// governanceFields parses agent_governance_required_fields: empty is nil, every Charter field;
// "none" is an empty list.
func governanceFields(text string) ([]domain.GovernanceField, error) {
	switch text {
	case "":
		return nil, nil
	case "none":
		return []domain.GovernanceField{}, nil
	}
	fields := []domain.GovernanceField{}
	for _, name := range strings.Split(text, ",") {
		field := domain.GovernanceField(strings.TrimSpace(name))
		if !slices.Contains(domain.CharterGovernanceFields(), field) || slices.Contains(fields, field) {
			return nil, fmt.Errorf("%s: %q is not a distinct Charter governance field", agent_governance_required_fields, field)
		}
		fields = append(fields, field)
	}
	return fields, nil
}
