package scout

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	keelconfig "github.com/nauticana/keel/config"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/confirmation"
	"github.com/nauticana/scout/service/dataplane"
	"github.com/nauticana/scout/service/evaluation"
	"github.com/nauticana/scout/service/observability"
	"github.com/nauticana/scout/service/scope"
	"github.com/nauticana/scout/service/workingset"
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

	agent_evaluation_judge_provider        = "agent_evaluation_judge_provider"
	agent_evaluation_judge_model           = "agent_evaluation_judge_model"
	agent_evaluation_judge_model_version   = "agent_evaluation_judge_model_version"
	agent_evaluation_judge_min_score       = "agent_evaluation_judge_min_score"
	agent_evaluation_judge_max_score       = "agent_evaluation_judge_max_score"
	agent_evaluation_judge_pass_score      = "agent_evaluation_judge_pass_score"
	agent_evaluation_judge_max_tokens      = "agent_evaluation_judge_max_tokens"
	agent_evaluation_judge_max_input_bytes = "agent_evaluation_judge_max_input_bytes"
)

const (
	agent_studio_max_templates_per_type = "agent_studio_max_templates_per_type"
	agent_studio_max_template_versions  = "agent_studio_max_template_versions"

	agent_approval_max_candidates             = "agent_approval_max_candidates"
	agent_approval_max_asks_per_call          = "agent_approval_max_asks_per_call"
	agent_approval_max_revised_argument_bytes = "agent_approval_max_revised_argument_bytes"
	agent_approval_max_routes_per_binding     = "agent_approval_max_routes_per_binding"
	agent_approval_max_routes                 = "agent_approval_max_routes"
	agent_approval_max_route_tools            = "agent_approval_max_route_tools"
	agent_approval_min_route_duration         = "agent_approval_min_route_duration"
	agent_approval_max_route_duration         = "agent_approval_max_route_duration"
	agent_approval_max_route_window           = "agent_approval_max_route_window"
	agent_confirmation_max_document_bytes     = "agent_confirmation_max_document_bytes"
	agent_confirmation_batch                  = "agent_confirmation_batch"
	agent_confirmation_max_batch              = "agent_confirmation_max_batch"
	agent_confirmation_lease                  = "agent_confirmation_lease"
)

const (
	agent_cost_window                         = "agent_cost_window"
	agent_reply_retention                     = "agent_reply_retention"
	agent_reply_poll_ms                       = "agent_reply_poll_ms"
	agent_reply_record_read                   = "agent_reply_record_read"
	agent_cancel_poll_ms                      = "agent_cancel_poll_ms"
	agent_cancel_max_read_failures            = "agent_cancel_max_read_failures"
	agent_session_revision_memory             = "agent_session_revision_memory"
	agent_result_card_max_bytes               = "agent_result_card_max_bytes"
	agent_run_page_max                        = "agent_run_page_max"
	agent_loop_max_call_revisions             = "agent_loop_max_call_revisions"
	agent_pause_batch                         = "agent_pause_batch"
	agent_studio_report_window_days           = "agent_studio_report_window_days"
	agent_studio_usage_max_lines              = "agent_studio_usage_max_lines"
	agent_work_item_max_depth                 = "agent_work_item_max_depth"
	agent_activation_timeout                  = "agent_activation_timeout"
	agent_activation_max_lead_days            = "agent_activation_max_lead_days"
	agent_studio_max_portable_bindings        = "agent_studio_max_portable_bindings"
	agent_studio_max_references               = "agent_studio_max_references"
	agent_studio_max_portable_extension_bytes = "agent_studio_max_portable_extension_bytes"
	agent_studio_max_template_audience        = "agent_studio_max_template_audience"
	agent_studio_usable_templates             = "agent_studio_usable_templates"
	agent_studio_max_governance_items         = "agent_studio_max_governance_items"
	agent_studio_max_comments_shown           = "agent_studio_max_comments_shown"
	agent_studio_max_restriction_entries      = "agent_studio_max_restriction_entries"
	agent_restriction_cache_ttl_ms            = "agent_restriction_cache_ttl_ms"
	agent_restriction_cache_entries           = "agent_restriction_cache_entries"
	agent_audit_max_page_size                 = "agent_audit_max_page_size"
	agent_safety_event_max_page               = "agent_safety_event_max_page"
	agent_metric_label_max_length             = "agent_metric_label_max_length"
	agent_knowledge_max_source_bytes          = "agent_knowledge_max_source_bytes"
	agent_knowledge_rrf_offset                = "agent_knowledge_rrf_offset"
	agent_memory_section_max_chars            = "agent_memory_section_max_chars"
	agent_evaluation_page_size                = "agent_evaluation_page_size"
	agent_evaluation_max_page_size            = "agent_evaluation_max_page_size"
	agent_evaluation_judge_max_verdict_bytes  = "agent_evaluation_judge_max_verdict_bytes"
	agent_default_language                    = "agent_default_language"
	agent_default_priority_class              = "agent_default_priority_class"
	agent_default_test_task                   = "agent_default_test_task"

	agent_review_feedback_threshold     = "agent_review_feedback_threshold"
	agent_review_max_pending            = "agent_review_max_pending"
	agent_review_page_size              = "agent_review_page_size"
	agent_review_max_page_size          = "agent_review_max_page_size"
	agent_review_max_turn_bytes         = "agent_review_max_turn_bytes"
	agent_evaluation_gate_max_results   = "agent_evaluation_gate_max_results"
	agent_evaluation_baseline_max_bytes = "agent_evaluation_baseline_max_bytes"
	agent_evaluation_max_baselines      = "agent_evaluation_max_baselines"
	agent_evaluation_compare_page_bytes = "agent_evaluation_compare_page_bytes"

	agent_health_period                   = "agent_health_period"
	agent_health_batch                    = "agent_health_batch"
	agent_health_window                   = "agent_health_window"
	agent_health_stale_periods            = "agent_health_stale_periods"
	agent_alert_page_size                 = "agent_alert_page_size"
	agent_alert_max_page_size             = "agent_alert_max_page_size"
	agent_evaluation_case_max_turns       = "agent_evaluation_case_max_turns"
	agent_evaluation_case_max_tools       = "agent_evaluation_case_max_tools"
	agent_evaluation_case_tool_max_bytes  = "agent_evaluation_case_tool_max_bytes"
	agent_evaluation_trajectory_max_bytes = "agent_evaluation_trajectory_max_bytes"
	agent_run_trace_max_events            = "agent_run_trace_max_events"
	agent_run_trace_max_journal_entries   = "agent_run_trace_max_journal_entries"
	agent_run_trace_max_journal_bytes     = "agent_run_trace_max_journal_bytes"
	agent_live_turn_page_size             = "agent_live_turn_page_size"
	agent_live_turn_max_page_size         = "agent_live_turn_max_page_size"
	agent_turn_history_page_size          = "agent_turn_history_page_size"
	agent_turn_history_max_page_size      = "agent_turn_history_max_page_size"
	agent_outcome_attest_grace            = "agent_outcome_attest_grace"
	agent_takeover_lease                  = "agent_takeover_lease"
	agent_operator_entry_max_bytes        = "agent_operator_entry_max_bytes"
	agent_operator_max_entries            = "agent_operator_max_entries"
	agent_hold_lapse_batch                = "agent_hold_lapse_batch"
	agent_hold_lapse_period               = "agent_hold_lapse_period"
	agent_working_set_max_candidates      = "agent_working_set_max_candidates"
	agent_working_set_max_tokens          = "agent_working_set_max_tokens"
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
	AgentMaxScopeDepth           int      // agent_max_scope_depth      8                             Maximum scope-chain depth a release may compile over (1..64)
	AgentMaxDelegationHops       int      // agent_max_delegation_hops  4                             Maximum delegation hops in an authority chain
	AgentApprovalDeadline        int      // agent_approval_deadline    3600                          Seconds a reviewer has before escalation; 0 leaves a request open
	AgentCredentialTTL           int      // agent_credential_ttl       300                           Default lifetime in seconds of a just-in-time tool credential
	AgentAuditPageSize           int      // agent_audit_page_size      100                           Decision records an audit page returns when the query names no limit; 0 uses Scout's default
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

	AgentStudioMaxTemplatesPerType int // agent_studio_max_templates_per_type 20 Templates of one agent type a tenant may own; 0 refuses every new template
	AgentStudioMaxTemplateVersions int // agent_studio_max_template_versions 50 Versions one template may keep; 0 refuses every template save

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

	AgentEvaluationJudgeProvider      string // agent_evaluation_judge_provider (none) Provider of the rubric judge model; empty with the model and version composes no judge
	AgentEvaluationJudgeModel         string // agent_evaluation_judge_model (none) Rubric judge model id
	AgentEvaluationJudgeModelVersion  string // agent_evaluation_judge_model_version (none) Rubric judge model version the judge is pinned to
	AgentEvaluationJudgeMinScore      int    // agent_evaluation_judge_min_score 1 Lowest rubric grade, at least 0
	AgentEvaluationJudgeMaxScore      int    // agent_evaluation_judge_max_score 5 Highest rubric grade, at most 100 above the lowest
	AgentEvaluationJudgePassScore     int    // agent_evaluation_judge_pass_score 4 Lowest rubric grade that passes
	AgentEvaluationJudgeMaxTokens     int    // agent_evaluation_judge_max_tokens 512 Output tokens one rubric verdict may use
	AgentEvaluationJudgeMaxInputBytes int    // agent_evaluation_judge_max_input_bytes 131072 Largest rubric judge prompt in bytes; a larger case fails its run

	AgentCostWindow                      int                  // agent_cost_window 3600 Seconds of trailing spend the data plane's cost breaker sums; 0 uses the default
	AgentReplyRetention                  int                  // agent_reply_retention 600 Seconds each reply frame stays in the delivery cache; 0 uses the default
	AgentReplyPollMs                     int                  // agent_reply_poll_ms 500 Milliseconds a reply subscriber waits before re-reading after a lost wake-up; 0 uses the default
	AgentReplyRecordRead                 int                  // agent_reply_record_read 5 Seconds between a reply subscriber's durable turn record reads; 0 uses the default
	AgentCancelPollMs                    int                  // agent_cancel_poll_ms 1000 Milliseconds between a running turn's cancellation reads; 0 uses the default
	AgentCancelMaxReadFailures           int                  // agent_cancel_max_read_failures 3 Consecutive failed cancellation reads that stop a running turn; 0 uses the default
	AgentSessionRevisionMemory           int                  // agent_session_revision_memory 4096 Conversations whose last written revision a process remembers; 0 uses the default
	AgentResultCardMaxBytes              int                  // agent_result_card_max_bytes 65536 Largest response in bytes copied into turn history; 0 uses the default
	AgentRunPageMax                      int                  // agent_run_page_max 500 Largest page of settled agent runs one query returns; 0 uses the default
	AgentLoopMaxCallRevisions            int                  // agent_loop_max_call_revisions 8 Decider revisions of one proposed tool call in a turn; 0 uses the default
	AgentPauseBatch                      int                  // agent_pause_batch 200 Live turns one Studio pause listing asks to stop; 0 uses the default
	AgentStudioReportWindowDays          int                  // agent_studio_report_window_days 92 Days one Studio usage or feedback report may span; 0 uses the default
	AgentStudioUsageMaxLines             int                  // agent_studio_usage_max_lines 1000 Lines one Studio usage report may return; 0 uses the default
	AgentWorkItemMaxDepth                int                  // agent_work_item_max_depth 32 Deepest delegated work item chain; 0 uses the default
	AgentApprovalMaxCandidates           int                  // agent_approval_max_candidates 20 Eligible holders one approver resolution considers; 0 takes 20
	AgentApprovalMaxAsksPerCall          int                  // agent_approval_max_asks_per_call 8 Approval requests one tool call may owe; 0 takes 8
	AgentApprovalMaxRevisedArgumentBytes int                  // agent_approval_max_revised_argument_bytes 65536 Largest revised arguments of an edited verdict in bytes; 0 takes 65536
	AgentApprovalMaxRoutesPerBinding     int                  // agent_approval_max_routes_per_binding 32 Routes one saved approval_routing binding may hold (1..32); 0 takes 32
	AgentApprovalMaxRoutes               int                  // agent_approval_max_routes 64 Effective approval routes one published agent may hold (1..64); 0 takes 64
	AgentApprovalMaxRouteTools           int                  // agent_approval_max_route_tools 16 Tool patterns one saved approval route may match (1..16); 0 takes 16
	AgentApprovalMinRouteDuration        int                  // agent_approval_min_route_duration 60 Shortest saved route deadline or extension in seconds (60..2592000); 0 takes 60
	AgentApprovalMaxRouteDuration        int                  // agent_approval_max_route_duration 2592000 Longest saved route deadline or extension in seconds (60..2592000); 0 takes 2592000
	AgentApprovalMaxRouteWindow          int                  // agent_approval_max_route_window 7776000 Longest saved route deadline plus extensions in seconds (60..7776000); 0 takes 7776000
	AgentConfirmationMaxDocumentBytes    int                  // agent_confirmation_max_document_bytes 1048576 Largest MCP confirmation payload, preview, requirements, or result in bytes; 0 takes 1048576
	AgentConfirmationBatch               int                  // agent_confirmation_batch 100 MCP confirmations one list or sweep reads when its caller names none; at most agent_confirmation_max_batch; 0 takes 100
	AgentConfirmationMaxBatch            int                  // agent_confirmation_max_batch 1000 MCP confirmations one list or sweep may read; 0 takes 1000
	AgentConfirmationLease               int                  // agent_confirmation_lease 60 Seconds one MCP confirmation execution claim lasts, renewed every third (1..2147483647); 0 takes 60
	AgentActivationTimeout               int                  // agent_activation_timeout 120 Seconds one scheduled activation's checks and swap may take; 0 uses Scout's default
	AgentActivationMaxLeadDays           int                  // agent_activation_max_lead_days 366 Days ahead a publish may schedule its activation; 0 uses Scout's default
	AgentStudioMaxPortableBindings       int                  // agent_studio_max_portable_bindings 1024 Bindings one portable document or template carries; 0 uses Scout's default
	AgentStudioMaxReferences             int                  // agent_studio_max_references 1024 Reference mappings one import or template create names; 0 uses Scout's default
	AgentStudioMaxPortableExtensionBytes int                  // agent_studio_max_portable_extension_bytes 262144 Bytes of one portable draft extension; 0 uses Scout's default
	AgentStudioMaxTemplateAudience       int                  // agent_studio_max_template_audience 100 Tenants one template publication names; 0 uses Scout's default
	AgentStudioUsableTemplates           int                  // agent_studio_usable_templates 200 Templates of each owner class one template list returns; 0 uses Scout's default
	AgentStudioMaxGovernanceItems        int                  // agent_studio_max_governance_items 32 Items one Studio-authored governance list holds; 0 uses Scout's default
	AgentStudioMaxCommentsShown          int                  // agent_studio_max_comments_shown 500 Draft comments one list returns; 0 uses Scout's default
	AgentStudioMaxRestrictionEntries     int                  // agent_studio_max_restriction_entries 256 Denials, and separately guardrail rules, of a tenant restriction layer written through Studio; 0 uses Scout's default
	AgentRestrictionCacheTTLMs           int                  // agent_restriction_cache_ttl_ms 5000 Milliseconds a read restriction layer is reused; 0 uses Scout's default
	AgentRestrictionCacheEntries         int                  // agent_restriction_cache_entries 4096 Tenant restriction layers held in the in-memory cache; 0 uses Scout's default
	AgentAuditMaxPageSize                int                  // agent_audit_max_page_size 1000 Largest decision record page an audit query may ask for; 0 uses Scout's default
	AgentSafetyEventMaxPage              int                  // agent_safety_event_max_page 500 Largest page of safety events one query may ask for; 0 uses Scout's default
	AgentMetricLabelMaxLength            int                  // agent_metric_label_max_length 64 Bytes of one fleet metric label value; 0 uses Scout's default
	AgentKnowledgeMaxSourceBytes         int                  // agent_knowledge_max_source_bytes 33554432 Bytes of one knowledge source object; 0 uses Scout's default
	AgentKnowledgeRRFOffset              int                  // agent_knowledge_rrf_offset 60 Reciprocal rank fusion constant k of hybrid retrieval; 0 uses Scout's default
	AgentMemorySectionMaxChars           int                  // agent_memory_section_max_chars 8000 Characters of one tenant memory section; 0 uses Scout's default
	AgentEvaluationPageSize              int                  // agent_evaluation_page_size 50 Evaluation results one Studio page returns when the caller names no limit; 0 uses Scout's default
	AgentEvaluationMaxPageSize           int                  // agent_evaluation_max_page_size 200 Largest evaluation result page a Studio caller may ask for; 0 uses Scout's default
	AgentEvaluationJudgeMaxVerdictBytes  int                  // agent_evaluation_judge_max_verdict_bytes 8192 Bytes of one raw rubric verdict; 0 uses Scout's default
	AgentDefaultLanguage                 string               // agent_default_language en-US Language a runtime or draft test falls back to when the agent has no prompt in the requested one
	AgentDefaultPriorityClass            domain.PriorityClass // agent_default_priority_class S priority_class code a model access grant takes when none is named
	AgentDefaultTestTask                 string               // agent_default_test_task (Scout's sample task) Task a Studio draft test runs when the caller sends none

	AgentReviewFeedbackThreshold    int // agent_review_feedback_threshold 2 Highest turn rating that queues a rated turn for review; 0 queues none
	AgentReviewMaxPending           int // agent_review_max_pending 1000 Pending sampled and rated turns one agent's review queue holds; 0 uses Scout's default
	AgentReviewPageSize             int // agent_review_page_size 50 Review items one Studio page returns when the caller names no limit; 0 uses Scout's default
	AgentReviewMaxPageSize          int // agent_review_max_page_size 200 Largest review item page a Studio caller may ask for; 0 uses Scout's default
	AgentReviewMaxTurnBytes         int // agent_review_max_turn_bytes 1048576 Bytes of a reviewed turn's input or response Studio reads or promotes; 0 uses Scout's default
	AgentEvaluationGateMaxResults   int // agent_evaluation_gate_max_results 2000 Results of one run a gate decision may read; 0 uses Scout's default
	AgentEvaluationBaselineMaxBytes int // agent_evaluation_baseline_max_bytes 32768 Bytes of one recorded human baseline output; 0 uses Scout's default
	AgentEvaluationMaxBaselines     int // agent_evaluation_max_baselines 20 Human baselines one golden set version may hold; 0 uses Scout's default
	AgentEvaluationComparePageBytes int // agent_evaluation_compare_page_bytes 1048576 Bytes of stored answers one comparison page returns; 0 uses Scout's default

	AgentHealthPeriod                 int // agent_health_period 60 Seconds between health evaluator passes (1..86400); 0 takes 60
	AgentHealthBatch                  int // agent_health_batch 100 Active agents one page of a health pass reads (1..10000); 0 takes 100
	AgentHealthWindow                 int // agent_health_window 60 Minutes a health measure covers when the policy names no window (1..10080); 0 takes 60
	AgentHealthStalePeriods           int // agent_health_stale_periods 3 Evaluator periods an agent may go unevaluated before its telemetry is stale (1..1000); 0 takes 3
	AgentAlertPageSize                int // agent_alert_page_size 50 Alerts one Studio page returns when the caller names no limit (0..agent_alert_max_page_size); 0 takes 50
	AgentAlertMaxPageSize             int // agent_alert_max_page_size 200 Largest alert page a Studio caller may ask for (1..1000); 0 takes 200
	AgentEvaluationCaseMaxTurns       int // agent_evaluation_case_max_turns 10 Turns one golden case scripts, its input included (1..100); 0 uses Scout's default
	AgentEvaluationCaseMaxTools       int // agent_evaluation_case_max_tools 20 Sandboxed tool responses one golden case scripts (1..200); 0 uses Scout's default
	AgentEvaluationCaseToolMaxBytes   int // agent_evaluation_case_tool_max_bytes 16384 Bytes of one scripted tool output (2..1048576); 0 uses Scout's default
	AgentEvaluationTrajectoryMaxBytes int // agent_evaluation_trajectory_max_bytes 1048576 Bytes of one stored case trajectory (1024..16777216); 0 uses Scout's default
	AgentRunTraceMaxEvents            int // agent_run_trace_max_events 500 Events one Studio run trace returns before it is truncated (1..5000); 0 takes 500
	AgentRunTraceMaxJournalEntries    int // agent_run_trace_max_journal_entries 1000 Loop journal entries one run trace reads (1..10000); 0 takes 1000
	AgentRunTraceMaxJournalBytes      int // agent_run_trace_max_journal_bytes 4194304 Cumulative loop journal payload bytes one run trace classifies (1024..67108864); 0 takes 4194304
	AgentLiveTurnPageSize             int // agent_live_turn_page_size 50 Live turns one Studio page returns when the caller names no limit (0..agent_live_turn_max_page_size); 0 takes 50
	AgentLiveTurnMaxPageSize          int // agent_live_turn_max_page_size 200 Largest live turn page a Studio caller may ask for (1..1000); 0 takes 200
	AgentTurnHistoryPageSize          int // agent_turn_history_page_size 50 Turns one conversation history page returns when the caller names no limit (0..agent_turn_history_max_page_size); 0 takes 50
	AgentTurnHistoryMaxPageSize       int // agent_turn_history_max_page_size 200 Largest conversation history page a caller may ask for (1..1000); 0 takes 200
	AgentOutcomeAttestGrace           int // agent_outcome_attest_grace 60 Seconds added to a non-read-only tool call's dispatch window for its outcome deadline (1..86400); 0 takes 60
	AgentTakeoverLease                int // agent_takeover_lease 900 Seconds an operator's lease on a turn they took over lasts from their latest act (1..86400); 0 takes 900
	AgentOperatorEntryMaxBytes        int // agent_operator_entry_max_bytes 16384 Encoded bytes of one operator entry (1..1048576); 0 takes 16384
	AgentOperatorMaxEntries           int // agent_operator_max_entries 64 Operator entries one turn may hold (1..4096); 0 takes 64
	AgentHoldLapseBatch               int // agent_hold_lapse_batch 100 Lapsed operator leases one sweeper statement holds again (1..10000); 0 takes 100
	AgentHoldLapsePeriod              int // agent_hold_lapse_period 30 Seconds between passes of the operator lease sweeper (1..3600); 0 takes 30
	AgentWorkingSetMaxCandidates      int // agent_working_set_max_candidates 64 Candidates one turn's working set source may offer (1..1024); 0 takes 64
	AgentWorkingSetMaxTokens          int // agent_working_set_max_tokens 8000 Token budget of one turn's working set (1..1048576); 0 takes 8000
}

// ApprovalRoutingLimits is what the agent_approval_*route* flags configure for StudioService;
// agent_approval_max_routes also bounds StudioApprovalRouting.MaxRoutes.
func (c *ScoutConfig) ApprovalRoutingLimits() domain.ApprovalRoutingLimits {
	return domain.ApprovalRoutingLimits{
		MaxRoutesPerBinding: c.AgentApprovalMaxRoutesPerBinding, MaxRoutes: c.AgentApprovalMaxRoutes,
		MaxToolPatterns:    c.AgentApprovalMaxRouteTools,
		MinDurationSeconds: int64(c.AgentApprovalMinRouteDuration), MaxDurationSeconds: int64(c.AgentApprovalMaxRouteDuration),
		MaxWindowSeconds: int64(c.AgentApprovalMaxRouteWindow),
	}
}

// approvalAndConfirmationRanges refuses an agent_approval_* or agent_confirmation_* value out of
// its range, naming the flag; zero always takes the consumer's default.
func (c *ScoutConfig) approvalAndConfirmationRanges() error {
	unbounded := math.MaxInt
	var errs []error
	check := func(name string, value, low, high int) {
		if value != 0 && (value < low || value > high) {
			errs = append(errs, fmt.Errorf("%s: %d is outside %d..%d", name, value, low, high))
		}
	}
	check(agent_approval_max_candidates, c.AgentApprovalMaxCandidates, 1, unbounded)
	check(agent_approval_max_asks_per_call, c.AgentApprovalMaxAsksPerCall, 1, unbounded)
	check(agent_approval_max_revised_argument_bytes, c.AgentApprovalMaxRevisedArgumentBytes, 1, unbounded)
	check(agent_approval_max_routes_per_binding, c.AgentApprovalMaxRoutesPerBinding, 1, domain.MaxApprovalRoutesPerBinding)
	check(agent_approval_max_routes, c.AgentApprovalMaxRoutes, 1, domain.MaxApprovalRoutes)
	check(agent_approval_max_route_tools, c.AgentApprovalMaxRouteTools, 1, domain.MaxApprovalRouteToolPatterns)
	check(agent_approval_min_route_duration, c.AgentApprovalMinRouteDuration, domain.MinApprovalRouteDurationSeconds, domain.MaxApprovalRouteDurationSeconds)
	check(agent_approval_max_route_duration, c.AgentApprovalMaxRouteDuration, domain.MinApprovalRouteDurationSeconds, domain.MaxApprovalRouteDurationSeconds)
	check(agent_approval_max_route_window, c.AgentApprovalMaxRouteWindow, domain.MinApprovalRouteDurationSeconds, domain.MaxApprovalRouteWindowSeconds)
	check(agent_confirmation_max_document_bytes, c.AgentConfirmationMaxDocumentBytes, 1, unbounded)
	check(agent_confirmation_batch, c.AgentConfirmationBatch, 1, unbounded)
	check(agent_confirmation_max_batch, c.AgentConfirmationMaxBatch, 1, unbounded)
	check(agent_confirmation_lease, c.AgentConfirmationLease, 1, 1<<31-1)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if _, err := c.ApprovalRoutingLimits().Resolve(); err != nil {
		errs = append(errs, fmt.Errorf("%s must not exceed %s or %s: %w",
			agent_approval_min_route_duration, agent_approval_max_route_duration, agent_approval_max_route_window, err))
	}
	batch, maxBatch := cmp.Or(c.AgentConfirmationBatch, confirmation.DefaultBatch), cmp.Or(c.AgentConfirmationMaxBatch, confirmation.DefaultMaxBatch)
	if batch > maxBatch {
		errs = append(errs, fmt.Errorf("%s: %d exceeds %s %d", agent_confirmation_batch, batch, agent_confirmation_max_batch, maxBatch))
	}
	return errors.Join(errs...)
}

// ToolLoopLimits is the executor-level ceiling the agent_loop_* flags configure.
func (c *ScoutConfig) ToolLoopLimits() domain.ToolLoopLimits {
	return domain.ToolLoopLimits{
		MaxIterations: c.AgentLoopMaxIterations, MaxToolCalls: c.AgentLoopMaxToolCalls,
		MaxTokens: int64(c.AgentLoopMaxTokens), MaxCostMinorUnits: int64(c.AgentLoopMaxCost),
		MaxRepeatedCalls: c.AgentLoopMaxRepeats, Deadline: time.Duration(c.AgentLoopDeadline) * time.Second,
		MaxCallRevisions: c.AgentLoopMaxCallRevisions,
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
		CostWindow:  seconds(c.AgentCostWindow), ReplyRetention: seconds(c.AgentReplyRetention),
		ReplyPollInterval: time.Duration(c.AgentReplyPollMs) * time.Millisecond, ReplyRecordReadInterval: seconds(c.AgentReplyRecordRead),
		CancelPollInterval: time.Duration(c.AgentCancelPollMs) * time.Millisecond, CancelMaxReadFailures: c.AgentCancelMaxReadFailures,
		SessionRevisionMemory: c.AgentSessionRevisionMemory, ResultCardMaxBytes: c.AgentResultCardMaxBytes, RunPageMax: c.AgentRunPageMax,
		AuditPageSize: c.AgentAuditPageSize, AuditMaxPageSize: c.AgentAuditMaxPageSize, SafetyEventMaxPage: c.AgentSafetyEventMaxPage,
		RestrictionCacheTTL:     time.Duration(c.AgentRestrictionCacheTTLMs) * time.Millisecond,
		RestrictionCacheEntries: c.AgentRestrictionCacheEntries,
		OutcomeAttestGrace:      seconds(c.AgentOutcomeAttestGrace),
	}
}

// ReleaseActivationSettings is what the agent_activation_* flags configure.
func (c *ScoutConfig) ReleaseActivationSettings() domain.ReleaseActivationSettings {
	return domain.ReleaseActivationSettings{
		Batch: c.AgentActivationBatch, PollPeriod: time.Duration(c.AgentActivationPoll) * time.Second,
		MaxAttempts: c.AgentActivationMaxAttempts, Timeout: time.Duration(c.AgentActivationTimeout) * time.Second,
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
	c.AgentStudioMaxTemplatesPerType = c.Int(rows, agent_studio_max_templates_per_type)
	c.AgentStudioMaxTemplateVersions = c.Int(rows, agent_studio_max_template_versions)
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
	c.AgentEvaluationJudgeProvider = c.String(rows, agent_evaluation_judge_provider)
	c.AgentEvaluationJudgeModel = c.String(rows, agent_evaluation_judge_model)
	c.AgentEvaluationJudgeModelVersion = c.String(rows, agent_evaluation_judge_model_version)
	c.AgentEvaluationJudgeMinScore = c.Int(rows, agent_evaluation_judge_min_score)
	c.AgentEvaluationJudgeMaxScore = c.Int(rows, agent_evaluation_judge_max_score)
	c.AgentEvaluationJudgePassScore = c.Int(rows, agent_evaluation_judge_pass_score)
	c.AgentEvaluationJudgeMaxTokens = c.Int(rows, agent_evaluation_judge_max_tokens)
	c.AgentEvaluationJudgeMaxInputBytes = c.Int(rows, agent_evaluation_judge_max_input_bytes)
	c.AgentCostWindow = c.Int(rows, agent_cost_window)
	c.AgentReplyRetention = c.Int(rows, agent_reply_retention)
	c.AgentReplyPollMs = c.Int(rows, agent_reply_poll_ms)
	c.AgentReplyRecordRead = c.Int(rows, agent_reply_record_read)
	c.AgentCancelPollMs = c.Int(rows, agent_cancel_poll_ms)
	c.AgentCancelMaxReadFailures = c.Int(rows, agent_cancel_max_read_failures)
	c.AgentSessionRevisionMemory = c.Int(rows, agent_session_revision_memory)
	c.AgentResultCardMaxBytes = c.Int(rows, agent_result_card_max_bytes)
	c.AgentRunPageMax = c.Int(rows, agent_run_page_max)
	c.AgentLoopMaxCallRevisions = c.Int(rows, agent_loop_max_call_revisions)
	c.AgentPauseBatch = c.Int(rows, agent_pause_batch)
	c.AgentStudioReportWindowDays = c.Int(rows, agent_studio_report_window_days)
	c.AgentStudioUsageMaxLines = c.Int(rows, agent_studio_usage_max_lines)
	c.AgentWorkItemMaxDepth = c.Int(rows, agent_work_item_max_depth)
	c.AgentApprovalMaxCandidates = c.Int(rows, agent_approval_max_candidates)
	c.AgentApprovalMaxAsksPerCall = c.Int(rows, agent_approval_max_asks_per_call)
	c.AgentApprovalMaxRevisedArgumentBytes = c.Int(rows, agent_approval_max_revised_argument_bytes)
	c.AgentApprovalMaxRoutesPerBinding = c.Int(rows, agent_approval_max_routes_per_binding)
	c.AgentApprovalMaxRoutes = c.Int(rows, agent_approval_max_routes)
	c.AgentApprovalMaxRouteTools = c.Int(rows, agent_approval_max_route_tools)
	c.AgentApprovalMinRouteDuration = c.Int(rows, agent_approval_min_route_duration)
	c.AgentApprovalMaxRouteDuration = c.Int(rows, agent_approval_max_route_duration)
	c.AgentApprovalMaxRouteWindow = c.Int(rows, agent_approval_max_route_window)
	c.AgentConfirmationMaxDocumentBytes = c.Int(rows, agent_confirmation_max_document_bytes)
	c.AgentConfirmationBatch = c.Int(rows, agent_confirmation_batch)
	c.AgentConfirmationMaxBatch = c.Int(rows, agent_confirmation_max_batch)
	c.AgentConfirmationLease = c.Int(rows, agent_confirmation_lease)
	runtimeErr := c.runtimeRangeErr()
	c.AgentActivationTimeout = c.Int(rows, agent_activation_timeout)
	c.AgentActivationMaxLeadDays = c.Int(rows, agent_activation_max_lead_days)
	c.AgentStudioMaxPortableBindings = c.Int(rows, agent_studio_max_portable_bindings)
	c.AgentStudioMaxReferences = c.Int(rows, agent_studio_max_references)
	c.AgentStudioMaxPortableExtensionBytes = c.Int(rows, agent_studio_max_portable_extension_bytes)
	c.AgentStudioMaxTemplateAudience = c.Int(rows, agent_studio_max_template_audience)
	c.AgentStudioUsableTemplates = c.Int(rows, agent_studio_usable_templates)
	c.AgentStudioMaxGovernanceItems = c.Int(rows, agent_studio_max_governance_items)
	c.AgentStudioMaxCommentsShown = c.Int(rows, agent_studio_max_comments_shown)
	c.AgentStudioMaxRestrictionEntries = c.Int(rows, agent_studio_max_restriction_entries)
	c.AgentRestrictionCacheTTLMs = c.Int(rows, agent_restriction_cache_ttl_ms)
	c.AgentRestrictionCacheEntries = c.Int(rows, agent_restriction_cache_entries)
	c.AgentAuditMaxPageSize = c.Int(rows, agent_audit_max_page_size)
	c.AgentSafetyEventMaxPage = c.Int(rows, agent_safety_event_max_page)
	c.AgentMetricLabelMaxLength = c.Int(rows, agent_metric_label_max_length)
	c.AgentKnowledgeMaxSourceBytes = c.Int(rows, agent_knowledge_max_source_bytes)
	c.AgentKnowledgeRRFOffset = c.Int(rows, agent_knowledge_rrf_offset)
	c.AgentMemorySectionMaxChars = c.Int(rows, agent_memory_section_max_chars)
	c.AgentEvaluationPageSize = c.Int(rows, agent_evaluation_page_size)
	c.AgentEvaluationMaxPageSize = c.Int(rows, agent_evaluation_max_page_size)
	c.AgentEvaluationJudgeMaxVerdictBytes = c.Int(rows, agent_evaluation_judge_max_verdict_bytes)
	c.AgentDefaultLanguage = c.String(rows, agent_default_language)
	c.AgentDefaultPriorityClass = domain.PriorityClass(c.String(rows, agent_default_priority_class))
	c.AgentDefaultTestTask = c.String(rows, agent_default_test_task)
	c.AgentReviewFeedbackThreshold = c.Int(rows, agent_review_feedback_threshold)
	c.AgentReviewMaxPending = c.Int(rows, agent_review_max_pending)
	c.AgentReviewPageSize = c.Int(rows, agent_review_page_size)
	c.AgentReviewMaxPageSize = c.Int(rows, agent_review_max_page_size)
	c.AgentReviewMaxTurnBytes = c.Int(rows, agent_review_max_turn_bytes)
	c.AgentEvaluationGateMaxResults = c.Int(rows, agent_evaluation_gate_max_results)
	c.AgentEvaluationBaselineMaxBytes = c.Int(rows, agent_evaluation_baseline_max_bytes)
	c.AgentEvaluationMaxBaselines = c.Int(rows, agent_evaluation_max_baselines)
	c.AgentEvaluationComparePageBytes = c.Int(rows, agent_evaluation_compare_page_bytes)
	c.AgentHealthPeriod = c.Int(rows, agent_health_period)
	c.AgentHealthBatch = c.Int(rows, agent_health_batch)
	c.AgentHealthWindow = c.Int(rows, agent_health_window)
	c.AgentHealthStalePeriods = c.Int(rows, agent_health_stale_periods)
	c.AgentAlertPageSize = c.Int(rows, agent_alert_page_size)
	c.AgentAlertMaxPageSize = c.Int(rows, agent_alert_max_page_size)
	c.AgentEvaluationCaseMaxTurns = c.Int(rows, agent_evaluation_case_max_turns)
	c.AgentEvaluationCaseMaxTools = c.Int(rows, agent_evaluation_case_max_tools)
	c.AgentEvaluationCaseToolMaxBytes = c.Int(rows, agent_evaluation_case_tool_max_bytes)
	c.AgentEvaluationTrajectoryMaxBytes = c.Int(rows, agent_evaluation_trajectory_max_bytes)
	c.AgentRunTraceMaxEvents = c.Int(rows, agent_run_trace_max_events)
	c.AgentRunTraceMaxJournalEntries = c.Int(rows, agent_run_trace_max_journal_entries)
	c.AgentRunTraceMaxJournalBytes = c.Int(rows, agent_run_trace_max_journal_bytes)
	c.AgentLiveTurnPageSize = c.Int(rows, agent_live_turn_page_size)
	c.AgentLiveTurnMaxPageSize = c.Int(rows, agent_live_turn_max_page_size)
	c.AgentTurnHistoryPageSize = c.Int(rows, agent_turn_history_page_size)
	c.AgentTurnHistoryMaxPageSize = c.Int(rows, agent_turn_history_max_page_size)
	c.AgentOutcomeAttestGrace = c.Int(rows, agent_outcome_attest_grace)
	c.AgentTakeoverLease = c.Int(rows, agent_takeover_lease)
	c.AgentOperatorEntryMaxBytes = c.Int(rows, agent_operator_entry_max_bytes)
	c.AgentOperatorMaxEntries = c.Int(rows, agent_operator_max_entries)
	c.AgentHoldLapseBatch = c.Int(rows, agent_hold_lapse_batch)
	c.AgentHoldLapsePeriod = c.Int(rows, agent_hold_lapse_period)
	c.AgentWorkingSetMaxCandidates = c.Int(rows, agent_working_set_max_candidates)
	c.AgentWorkingSetMaxTokens = c.Int(rows, agent_working_set_max_tokens)
	limitErr := c.limitFlagErrors()
	required, err := governanceFields(c.String(rows, agent_governance_required_fields))
	c.AgentGovernanceRequiredFields = required
	var judgeErr error
	if judge := c.RubricJudgeSettings(); judge.Configured() {
		if judgeErr = judge.Validate(); judgeErr != nil {
			judgeErr = fmt.Errorf("agent_evaluation_judge_*: %w", judgeErr)
		}
	}
	return errors.Join(c.ParseErr(), err, judgeErr, runtimeErr, c.approvalAndConfirmationRanges(), limitErr, c.defaultTextErrors(), c.reviewFlagErrors(), c.healthFlagErrors(), c.caseScriptFlagErrors(), c.runTraceFlagErrors(), c.liveTurnFlagErrors(), c.operatorFlagErrors(), c.workingSetFlagErrors())
}

// WorkingSetLimits is what the agent_working_set_* flags configure for workingset.Compiler.
func (c *ScoutConfig) WorkingSetLimits() domain.WorkingSetLimits {
	return domain.WorkingSetLimits{MaxCandidates: c.AgentWorkingSetMaxCandidates, MaxTokens: int64(c.AgentWorkingSetMaxTokens)}
}

// workingSetFlagErrors refuses a working set bound outside its range; zero takes the default.
func (c *ScoutConfig) workingSetFlagErrors() error {
	var errs []error
	for _, flag := range []struct {
		name       string
		value, max int
	}{
		{agent_working_set_max_candidates, c.AgentWorkingSetMaxCandidates, workingset.MaxCandidates},
		{agent_working_set_max_tokens, c.AgentWorkingSetMaxTokens, workingset.MaxTokens},
	} {
		if flag.value < 0 || flag.value > flag.max {
			errs = append(errs, fmt.Errorf("%s: %d is outside 0..%d; 0 takes the default", flag.name, flag.value, flag.max))
		}
	}
	return errors.Join(errs...)
}

// OperatorSettings is what the agent_takeover_lease, agent_operator_*, and agent_hold_lapse_*
// flags configure for dataplane.TableTurnControl and its lapse worker.
func (c *ScoutConfig) OperatorSettings() domain.OperatorSettings {
	return domain.OperatorSettings{
		Lease: time.Duration(c.AgentTakeoverLease) * time.Second, EntryMaxBytes: c.AgentOperatorEntryMaxBytes,
		MaxEntries: c.AgentOperatorMaxEntries, LapseBatch: c.AgentHoldLapseBatch,
		LapsePeriod: time.Duration(c.AgentHoldLapsePeriod) * time.Second,
	}
}

// operatorFlagErrors refuses an operation flag outside its range; zero takes each default.
func (c *ScoutConfig) operatorFlagErrors() error {
	var errs []error
	for _, flag := range []struct {
		name       string
		value, max int
	}{
		{agent_takeover_lease, c.AgentTakeoverLease, int(dataplane.MaxTakeoverLease / time.Second)},
		{agent_operator_entry_max_bytes, c.AgentOperatorEntryMaxBytes, dataplane.MaxOperatorEntryBytes},
		{agent_operator_max_entries, c.AgentOperatorMaxEntries, dataplane.MaxOperatorEntries},
		{agent_hold_lapse_batch, c.AgentHoldLapseBatch, dataplane.MaxHoldLapseBatch},
		{agent_hold_lapse_period, c.AgentHoldLapsePeriod, int(dataplane.MaxHoldLapsePeriod / time.Second)},
	} {
		if flag.value < 0 || flag.value > flag.max {
			errs = append(errs, fmt.Errorf("%s: %d is outside 0..%d; 0 takes the default", flag.name, flag.value, flag.max))
		}
	}
	return errors.Join(errs...)
}

// maxOutcomeAttestGrace bounds agent_outcome_attest_grace: one day, in seconds.
const maxOutcomeAttestGrace = 86400

// LiveTurnLimits is what the agent_live_turn_* flags configure for dataplane.StudioTurnControl.
func (c *ScoutConfig) LiveTurnLimits() domain.LiveTurnLimits {
	return domain.LiveTurnLimits{PageSize: c.AgentLiveTurnPageSize, MaxPageSize: c.AgentLiveTurnMaxPageSize}
}

// liveTurnFlagErrors refuses a live turn or turn history page flag, or agent_outcome_attest_grace,
// outside its range, and a default page larger than its largest page; zero takes each default.
func (c *ScoutConfig) liveTurnFlagErrors() error {
	var errs []error
	for _, pages := range []struct {
		sizeName, maxName                  string
		size, max, defaultSize, defaultMax int
	}{
		{agent_live_turn_page_size, agent_live_turn_max_page_size, c.AgentLiveTurnPageSize, c.AgentLiveTurnMaxPageSize,
			dataplane.DefaultLiveTurnPageSize, dataplane.DefaultLiveTurnMaxPageSize},
		{agent_turn_history_page_size, agent_turn_history_max_page_size, c.AgentTurnHistoryPageSize, c.AgentTurnHistoryMaxPageSize,
			dataplane.DefaultTurnHistoryPageSize, dataplane.DefaultTurnHistoryMaxPageSize},
	} {
		for _, flag := range []struct {
			name  string
			value int
		}{{pages.sizeName, pages.size}, {pages.maxName, pages.max}} {
			if flag.value < 0 || flag.value > 1000 {
				errs = append(errs, fmt.Errorf("%s: %d is outside 0..1000; 0 takes the default", flag.name, flag.value))
			}
		}
		if size, maxSize := cmp.Or(pages.size, pages.defaultSize), cmp.Or(pages.max, pages.defaultMax); size > maxSize {
			errs = append(errs, fmt.Errorf("%s: %d exceeds the largest page %d", pages.sizeName, size, maxSize))
		}
	}
	if c.AgentOutcomeAttestGrace < 0 || c.AgentOutcomeAttestGrace > maxOutcomeAttestGrace {
		errs = append(errs, fmt.Errorf("%s: %d is outside 0..%d; 0 takes the default", agent_outcome_attest_grace, c.AgentOutcomeAttestGrace, maxOutcomeAttestGrace))
	}
	return errors.Join(errs...)
}

// runtimeRangeErr refuses a negative data plane or runtime tuning flag; zero takes Scout's default.
func (c *ScoutConfig) runtimeRangeErr() error {
	var failures []error
	for _, flag := range []struct {
		name  string
		value int
	}{
		{agent_cost_window, c.AgentCostWindow}, {agent_reply_retention, c.AgentReplyRetention},
		{agent_reply_poll_ms, c.AgentReplyPollMs}, {agent_reply_record_read, c.AgentReplyRecordRead},
		{agent_cancel_poll_ms, c.AgentCancelPollMs}, {agent_cancel_max_read_failures, c.AgentCancelMaxReadFailures},
		{agent_session_revision_memory, c.AgentSessionRevisionMemory}, {agent_result_card_max_bytes, c.AgentResultCardMaxBytes},
		{agent_run_page_max, c.AgentRunPageMax}, {agent_loop_max_call_revisions, c.AgentLoopMaxCallRevisions},
		{agent_pause_batch, c.AgentPauseBatch}, {agent_studio_report_window_days, c.AgentStudioReportWindowDays},
		{agent_studio_usage_max_lines, c.AgentStudioUsageMaxLines}, {agent_work_item_max_depth, c.AgentWorkItemMaxDepth},
	} {
		if flag.value < 0 {
			failures = append(failures, fmt.Errorf("%s: %d is negative; 0 takes the default", flag.name, flag.value))
		}
	}
	return errors.Join(failures...)
}

// RubricJudgeSettings is what the agent_evaluation_judge_* flags configure; a product
// composes the rubric judge only when Configured reports a judge model.
func (c *ScoutConfig) RubricJudgeSettings() domain.RubricJudgeSettings {
	return domain.RubricJudgeSettings{
		Model: domain.ModelSelection{
			Provider: c.AgentEvaluationJudgeProvider, Model: c.AgentEvaluationJudgeModel, ModelVersion: c.AgentEvaluationJudgeModelVersion,
		},
		MinScore: c.AgentEvaluationJudgeMinScore, MaxScore: c.AgentEvaluationJudgeMaxScore, PassScore: c.AgentEvaluationJudgePassScore,
		MaxOutputTokens: int64(c.AgentEvaluationJudgeMaxTokens), MaxInputBytes: c.AgentEvaluationJudgeMaxInputBytes,
		MaxVerdictBytes: c.AgentEvaluationJudgeMaxVerdictBytes,
	}
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
	for name := range strings.SplitSeq(text, ",") {
		field := domain.GovernanceField(strings.TrimSpace(name))
		if !slices.Contains(domain.CharterGovernanceFields(), field) || slices.Contains(fields, field) {
			return nil, fmt.Errorf("%s: %q is not a distinct Charter governance field", agent_governance_required_fields, field)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// limitFlagErrors refuses a negative limit flag, a scope depth outside 1..scope.MaxScopeChainDepth,
// and a default page larger than the largest page it answers within.
func (c *ScoutConfig) limitFlagErrors() error {
	var errs []error
	for _, flag := range []struct {
		name  string
		value int
	}{
		{agent_audit_page_size, c.AgentAuditPageSize},
		{agent_activation_timeout, c.AgentActivationTimeout},
		{agent_activation_max_lead_days, c.AgentActivationMaxLeadDays},
		{agent_studio_max_portable_bindings, c.AgentStudioMaxPortableBindings},
		{agent_studio_max_references, c.AgentStudioMaxReferences},
		{agent_studio_max_portable_extension_bytes, c.AgentStudioMaxPortableExtensionBytes},
		{agent_studio_max_template_audience, c.AgentStudioMaxTemplateAudience},
		{agent_studio_usable_templates, c.AgentStudioUsableTemplates},
		{agent_studio_max_governance_items, c.AgentStudioMaxGovernanceItems},
		{agent_studio_max_comments_shown, c.AgentStudioMaxCommentsShown},
		{agent_studio_max_restriction_entries, c.AgentStudioMaxRestrictionEntries},
		{agent_restriction_cache_ttl_ms, c.AgentRestrictionCacheTTLMs},
		{agent_restriction_cache_entries, c.AgentRestrictionCacheEntries},
		{agent_audit_max_page_size, c.AgentAuditMaxPageSize},
		{agent_safety_event_max_page, c.AgentSafetyEventMaxPage},
		{agent_metric_label_max_length, c.AgentMetricLabelMaxLength},
		{agent_knowledge_max_source_bytes, c.AgentKnowledgeMaxSourceBytes},
		{agent_knowledge_rrf_offset, c.AgentKnowledgeRRFOffset},
		{agent_memory_section_max_chars, c.AgentMemorySectionMaxChars},
		{agent_evaluation_page_size, c.AgentEvaluationPageSize},
		{agent_evaluation_max_page_size, c.AgentEvaluationMaxPageSize},
		{agent_evaluation_judge_max_verdict_bytes, c.AgentEvaluationJudgeMaxVerdictBytes},
		{agent_evaluation_gate_max_results, c.AgentEvaluationGateMaxResults},
		{agent_evaluation_baseline_max_bytes, c.AgentEvaluationBaselineMaxBytes},
		{agent_evaluation_max_baselines, c.AgentEvaluationMaxBaselines},
		{agent_evaluation_compare_page_bytes, c.AgentEvaluationComparePageBytes},
	} {
		if flag.value < 0 {
			errs = append(errs, fmt.Errorf("%s: %d cannot be negative", flag.name, flag.value))
		}
	}
	if c.AgentMaxScopeDepth < 1 || c.AgentMaxScopeDepth > scope.MaxScopeChainDepth {
		errs = append(errs, fmt.Errorf("%s: %d is outside 1..%d", agent_max_scope_depth, c.AgentMaxScopeDepth, scope.MaxScopeChainDepth))
	}
	page := func(name string, size, maxSize, defaultSize, defaultMax int) {
		if size == 0 {
			size = defaultSize
		}
		if maxSize == 0 {
			maxSize = defaultMax
		}
		if size > maxSize {
			errs = append(errs, fmt.Errorf("%s: %d exceeds the largest page %d", name, size, maxSize))
		}
	}
	page(agent_audit_page_size, c.AgentAuditPageSize, c.AgentAuditMaxPageSize, observability.DefaultDecisionPageSize, observability.DefaultMaxDecisionPageSize)
	page(agent_evaluation_page_size, c.AgentEvaluationPageSize, c.AgentEvaluationMaxPageSize, evaluation.DefaultStudioPageSize, evaluation.DefaultStudioMaxPageSize)
	return errors.Join(errs...)
}

// defaultTextErrors refuses a default longer than the column it is stored in; empty takes
// Scout's default.
func (c *ScoutConfig) defaultTextErrors() error {
	var errs []error
	for _, flag := range []struct {
		name, value string
		max         int
	}{{agent_default_language, c.AgentDefaultLanguage, 16}, {agent_default_priority_class, string(c.AgentDefaultPriorityClass), 1}} {
		if len(strings.TrimSpace(flag.value)) > flag.max {
			errs = append(errs, fmt.Errorf("%s: %q is longer than %d bytes", flag.name, flag.value, flag.max))
		}
	}
	if class := c.AgentDefaultPriorityClass; class != "" && !class.Valid() {
		errs = append(errs, fmt.Errorf("%s: %q is not a priority_class code", agent_default_priority_class, class))
	}
	return errors.Join(errs...)
}

// reviewFlagErrors refuses a feedback threshold outside 0..5, a negative review limit, and a
// default review page larger than the largest page.
func (c *ScoutConfig) reviewFlagErrors() error {
	var errs []error
	if c.AgentReviewFeedbackThreshold < 0 || c.AgentReviewFeedbackThreshold > domain.ReviewMaxScore {
		errs = append(errs, fmt.Errorf("%s: %d is outside 0..%d", agent_review_feedback_threshold, c.AgentReviewFeedbackThreshold, domain.ReviewMaxScore))
	}
	for _, flag := range []struct {
		name  string
		value int
	}{
		{agent_review_max_pending, c.AgentReviewMaxPending}, {agent_review_page_size, c.AgentReviewPageSize},
		{agent_review_max_page_size, c.AgentReviewMaxPageSize}, {agent_review_max_turn_bytes, c.AgentReviewMaxTurnBytes},
	} {
		if flag.value < 0 {
			errs = append(errs, fmt.Errorf("%s: %d cannot be negative", flag.name, flag.value))
		}
	}
	size, maxSize := cmp.Or(c.AgentReviewPageSize, evaluation.DefaultReviewPageSize), cmp.Or(c.AgentReviewMaxPageSize, evaluation.DefaultReviewMaxPageSize)
	if size > maxSize {
		errs = append(errs, fmt.Errorf("%s: %d exceeds the largest page %d", agent_review_page_size, size, maxSize))
	}
	return errors.Join(errs...)
}

// AgentHealthSettings is what the agent_health_* and agent_alert_* flags configure for
// dataplane.AgentHealth and its worker.
func (c *ScoutConfig) AgentHealthSettings() domain.AgentHealthSettings {
	return domain.AgentHealthSettings{
		Period: time.Duration(c.AgentHealthPeriod) * time.Second, Batch: c.AgentHealthBatch, WindowMinutes: int64(c.AgentHealthWindow),
		StalePeriods: c.AgentHealthStalePeriods, AlertPageSize: c.AgentAlertPageSize, AlertMaxPageSize: c.AgentAlertMaxPageSize,
	}
}

// healthFlagErrors refuses a health or alert flag outside its range and a default alert page
// larger than the largest page; zero takes each default.
func (c *ScoutConfig) healthFlagErrors() error {
	var errs []error
	for _, flag := range []struct {
		name       string
		value, max int
	}{
		{agent_health_period, c.AgentHealthPeriod, 86400}, {agent_health_batch, c.AgentHealthBatch, 10000},
		{agent_health_window, c.AgentHealthWindow, domain.MaxHealthWindowMinutes}, {agent_health_stale_periods, c.AgentHealthStalePeriods, 1000},
		{agent_alert_page_size, c.AgentAlertPageSize, 1000}, {agent_alert_max_page_size, c.AgentAlertMaxPageSize, 1000},
	} {
		if flag.value < 0 || flag.value > flag.max {
			errs = append(errs, fmt.Errorf("%s: %d is outside 0..%d; 0 takes the default", flag.name, flag.value, flag.max))
		}
	}
	size, maxSize := cmp.Or(c.AgentAlertPageSize, dataplane.DefaultAlertPageSize), cmp.Or(c.AgentAlertMaxPageSize, dataplane.DefaultAlertMaxPageSize)
	if size > maxSize {
		errs = append(errs, fmt.Errorf("%s: %d exceeds the largest page %d", agent_alert_page_size, size, maxSize))
	}
	return errors.Join(errs...)
}

// caseScriptFlagErrors refuses a golden case script or trajectory bound outside its range; zero takes Scout's default.
func (c *ScoutConfig) caseScriptFlagErrors() error {
	var errs []error
	for _, flag := range []struct {
		name          string
		value, lo, hi int
	}{
		{agent_evaluation_case_max_turns, c.AgentEvaluationCaseMaxTurns, 1, 100},
		{agent_evaluation_case_max_tools, c.AgentEvaluationCaseMaxTools, 1, 200},
		{agent_evaluation_case_tool_max_bytes, c.AgentEvaluationCaseToolMaxBytes, 2, 1 << 20},
		{agent_evaluation_trajectory_max_bytes, c.AgentEvaluationTrajectoryMaxBytes, 1 << 10, 16 << 20},
	} {
		if flag.value != 0 && (flag.value < flag.lo || flag.value > flag.hi) {
			errs = append(errs, fmt.Errorf("%s: %d is outside %d..%d; 0 takes the default", flag.name, flag.value, flag.lo, flag.hi))
		}
	}
	return errors.Join(errs...)
}

// RunTraceLimits is what the agent_run_trace_* flags configure for dataplane.TableRunTrace.
func (c *ScoutConfig) RunTraceLimits() domain.RunTraceLimits {
	return domain.RunTraceLimits{
		MaxEvents: c.AgentRunTraceMaxEvents, MaxJournalEntries: c.AgentRunTraceMaxJournalEntries,
		MaxJournalBytes: int64(c.AgentRunTraceMaxJournalBytes),
	}
}

// runTraceFlagErrors refuses a run trace bound outside its range; zero takes the default.
func (c *ScoutConfig) runTraceFlagErrors() error {
	var errs []error
	for _, flag := range []struct {
		name          string
		value, lo, hi int
	}{
		{agent_run_trace_max_events, c.AgentRunTraceMaxEvents, 1, dataplane.MaxRunTraceEvents},
		{agent_run_trace_max_journal_entries, c.AgentRunTraceMaxJournalEntries, 1, dataplane.MaxRunTraceJournalEntries},
		{agent_run_trace_max_journal_bytes, c.AgentRunTraceMaxJournalBytes, 1 << 10, dataplane.MaxRunTraceJournalBytes},
	} {
		if flag.value != 0 && (flag.value < flag.lo || flag.value > flag.hi) {
			errs = append(errs, fmt.Errorf("%s: %d is outside %d..%d; 0 takes the default", flag.name, flag.value, flag.lo, flag.hi))
		}
	}
	return errors.Join(errs...)
}
