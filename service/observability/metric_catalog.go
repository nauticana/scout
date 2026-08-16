package observability

// MetricKind is how a sink aggregates one metric.
type MetricKind string

const (
	MetricCounter   MetricKind = "counter"
	MetricHistogram MetricKind = "histogram"
	MetricGauge     MetricKind = "gauge"
)

// Metric names emitted by this package; sinks may pre-register them from Catalog.
const (
	MetricTurnOutcomes          = "scout_turn_outcomes_total"
	MetricTurnDuration          = "scout_turn_duration_seconds"
	MetricStageDuration         = "scout_stage_duration_seconds"
	MetricTimeToFirstToken      = "scout_time_to_first_token_seconds"
	MetricTimePerOutputToken    = "scout_time_per_output_token_seconds"
	MetricQueueWait             = "scout_queue_wait_seconds"
	MetricAdmissionRejections   = "scout_admission_rejections_total"
	MetricReservationTokenDelta = "scout_reservation_token_delta"
	MetricReservationCostDelta  = "scout_reservation_cost_delta_minor_units"
	MetricUsageInputTokens      = "scout_usage_input_tokens_total"
	MetricUsageOutputTokens     = "scout_usage_output_tokens_total"
	MetricUsageToolCalls        = "scout_usage_tool_calls_total"
	MetricUsageCost             = "scout_usage_cost_minor_units_total"
	MetricStepOutcomes          = "scout_step_outcomes_total"
	MetricDependencyCalls       = "scout_dependency_calls_total"
	MetricDependencyErrors      = "scout_dependency_errors_total"
	MetricLedgerErrors          = "scout_ledger_errors_total"
	MetricLabelRejections       = "scout_metric_label_rejections_total"
	MetricTenantRankEstimate    = "scout_tenant_rank_estimate"
)

// Metric describes one catalog entry.
type Metric struct {
	Name string
	Kind MetricKind
	Help string
}

// Catalog is the fixed set of metric names this package emits.
var Catalog = []Metric{
	{MetricTurnOutcomes, MetricCounter, "Completed turns by outcome and error class."},
	{MetricTurnDuration, MetricHistogram, "Whole-turn wall time in seconds."},
	{MetricStageDuration, MetricHistogram, "Stage wall time in seconds by stage and component."},
	{MetricTimeToFirstToken, MetricHistogram, "Seconds from stage start to the first approved streamed frame."},
	{MetricTimePerOutputToken, MetricHistogram, "Seconds per output token after the first frame."},
	{MetricQueueWait, MetricHistogram, "Seconds a turn waited before admission or dispatch."},
	{MetricAdmissionRejections, MetricCounter, "Observations rejected at admission by error class."},
	{MetricReservationTokenDelta, MetricHistogram, "Actual minus reserved tokens per observation."},
	{MetricReservationCostDelta, MetricHistogram, "Actual minus reserved cost in minor units per observation."},
	{MetricUsageInputTokens, MetricCounter, "Input tokens consumed."},
	{MetricUsageOutputTokens, MetricCounter, "Output tokens produced."},
	{MetricUsageToolCalls, MetricCounter, "Tool calls made."},
	{MetricUsageCost, MetricCounter, "Cost in minor units by currency."},
	{MetricStepOutcomes, MetricCounter, "Graph steps by kind, outcome, and error class."},
	{MetricDependencyCalls, MetricCounter, "Governed dependency calls by component."},
	{MetricDependencyErrors, MetricCounter, "Governed dependency failures by component and error class."},
	{MetricLedgerErrors, MetricCounter, "Exact tenant ledger write failures."},
	{MetricLabelRejections, MetricCounter, "Samples dropped because a label violated the policy."},
	{MetricTenantRankEstimate, MetricGauge, "Estimated weight of the tenant occupying each stable rank slot."},
}

// LookupMetric returns the catalog entry for name.
func LookupMetric(name string) (Metric, bool) {
	for _, metric := range Catalog {
		if metric.Name == name {
			return metric, true
		}
	}
	return Metric{}, false
}
