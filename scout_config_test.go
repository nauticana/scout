package scout

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	keelconfig "github.com/nauticana/keel/config"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"
	keelschema "github.com/nauticana/keel/schema"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/dataplane"
)

type scoutConfigQueryFake struct {
	keelport.QueryService
	args []any
	rows [][]any
}

func (f *scoutConfigQueryFake) Query(_ context.Context, _ string, args ...any) (*keelmodel.QueryResult, error) {
	f.args = args
	return &keelmodel.QueryResult{Rows: f.rows}, nil
}

type scoutConfigDatabaseFake struct {
	keelport.DatabaseRepository
	query keelport.QueryService
}

func (f scoutConfigDatabaseFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return f.query
}

func TestScoutConfigApply(t *testing.T) {
	rows := scoutConfigRows()
	rows[agent_max_tokens] = keelconfig.ConfigRow{Value: "16384", Default: "8192"}
	rows[agent_model_capacity_pool] = keelconfig.ConfigRow{Value: " dedicated ", Default: "shared"}

	unset := &ScoutConfig{}
	if err := unset.Apply(rows); err != nil || unset.AgentTemperature != nil {
		t.Fatalf("an unconfigured temperature must stay nil, got %v (%v)", unset.AgentTemperature, err)
	}
	rows[agent_temperature] = keelconfig.ConfigRow{Value: "0.7"}

	cfg := &ScoutConfig{}
	if err := cfg.Apply(rows); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cfg.AgentMaxTokens != 16384 {
		t.Fatalf("AgentMaxTokens = %d, want 16384", cfg.AgentMaxTokens)
	}
	if cfg.AgentTemperature == nil || *cfg.AgentTemperature != 0.7 {
		t.Fatalf("AgentTemperature = %v, want 0.7", cfg.AgentTemperature)
	}
	if cfg.AgentModelCapacityPool != "dedicated" {
		t.Fatalf("AgentModelCapacityPool = %q, want dedicated", cfg.AgentModelCapacityPool)
	}
	if cfg.AgentModelMaxWaiters != 4096 {
		t.Fatalf("AgentModelMaxWaiters = %d, want 4096", cfg.AgentModelMaxWaiters)
	}
	if cfg.AgentPilotMaxMembers != 200 || cfg.AgentPilotPositionTimeout != 2 {
		t.Fatalf("AgentPilotMaxMembers = %d, AgentPilotPositionTimeout = %d, want 200 and 2", cfg.AgentPilotMaxMembers, cfg.AgentPilotPositionTimeout)
	}
	if cfg.AgentStudioMaxTemplatesPerType != 20 || cfg.AgentStudioMaxTemplateVersions != 50 {
		t.Fatalf("template limits = %d, %d, want 20 and 50", cfg.AgentStudioMaxTemplatesPerType, cfg.AgentStudioMaxTemplateVersions)
	}
	if cfg.AgentEvaluationMaxOpenRuns != 4 || cfg.AgentEvaluationLease != 900 || cfg.AgentEvaluationCurrency != "" {
		t.Fatalf("evaluation settings = %d, %d, %q", cfg.AgentEvaluationMaxOpenRuns, cfg.AgentEvaluationLease, cfg.AgentEvaluationCurrency)
	}
}

func TestScoutConfigGovernanceRequiredFields(t *testing.T) {
	for value, want := range map[string][]domain.GovernanceField{
		"":     nil,
		"none": {},
		" purpose, accountable ,escalationConditions": {domain.GovernancePurpose, domain.GovernanceAccountable, domain.GovernanceEscalationConditions},
	} {
		rows := scoutConfigRows()
		rows[agent_governance_required_fields] = keelconfig.ConfigRow{Value: value}
		cfg := &ScoutConfig{}
		if err := cfg.Apply(rows); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if !slices.Equal(cfg.AgentGovernanceRequiredFields, want) || (want == nil) != (cfg.AgentGovernanceRequiredFields == nil) {
			t.Fatalf("%q: fields = %#v, want %#v", value, cfg.AgentGovernanceRequiredFields, want)
		}
	}
	for _, value := range []string{"purpose,owner", "purpose,purpose", "purpose,"} {
		rows := scoutConfigRows()
		rows[agent_governance_required_fields] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_governance_required_fields) {
			t.Fatalf("%q: error = %v, want a refusal naming the flag", value, err)
		}
	}
}

func TestScoutConfigApplyReportsAllInvalidRows(t *testing.T) {
	rows := scoutConfigRows()
	delete(rows, agent_turn_burst)
	rows[agent_model_rate] = keelconfig.ConfigRow{Default: "many"}

	err := (&ScoutConfig{}).Apply(rows)
	if err == nil {
		t.Fatal("Apply returned nil, want catalog errors")
	}
	for _, want := range []string{agent_turn_burst, agent_model_rate} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply error %q does not contain %q", err, want)
		}
	}
}

func TestScoutConfigAccessor(t *testing.T) {
	previous := Config()
	t.Cleanup(func() { SetConfig(previous) })

	want := &ScoutConfig{AgentMaxTokens: 42}
	SetConfig(want)
	if got := Config(); got != want {
		t.Fatalf("Config() = %p, want %p", got, want)
	}
}

func TestLoadConfigPublishesKeelAndScoutSections(t *testing.T) {
	previousKeel, previousScout := keelconfig.Config(), Config()
	t.Cleanup(func() {
		keelconfig.SetConfig(previousKeel)
		SetConfig(previousScout)
	})

	oldKeel := &keelconfig.KeelConfig{HttpApiPort: 1}
	oldScout := &ScoutConfig{AgentMaxTokens: 1}
	keelconfig.SetConfig(oldKeel)
	SetConfig(oldScout)
	query := &scoutConfigQueryFake{rows: allConfigQueryRows(t)}

	if err := LoadConfig(context.Background(), scoutConfigDatabaseFake{query: query}, 37); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(query.args) != 1 || query.args[0] != 37 {
		t.Fatalf("query args = %#v, want [37]", query.args)
	}
	if got := keelconfig.Config(); got == oldKeel || got.HttpApiPort != 8080 {
		t.Fatalf("keel Config() = %#v, want newly published defaults", got)
	}
	if got := Config(); got == oldScout || got.AgentMaxTokens != 8192 {
		t.Fatalf("scout Config() = %#v, want newly published defaults", got)
	}
}

func TestLoadConfigFailurePublishesNeitherSection(t *testing.T) {
	previousKeel, previousScout := keelconfig.Config(), Config()
	t.Cleanup(func() {
		keelconfig.SetConfig(previousKeel)
		SetConfig(previousScout)
	})

	oldKeel := &keelconfig.KeelConfig{HttpApiPort: 1}
	oldScout := &ScoutConfig{AgentMaxTokens: 1}
	keelconfig.SetConfig(oldKeel)
	SetConfig(oldScout)
	rows := allConfigQueryRows(t)
	for i, row := range rows {
		if row[0] == agent_turn_burst {
			rows = append(rows[:i], rows[i+1:]...)
			break
		}
	}

	err := LoadConfig(context.Background(), scoutConfigDatabaseFake{query: &scoutConfigQueryFake{rows: rows}}, 0)
	if err == nil || !strings.Contains(err.Error(), agent_turn_burst) {
		t.Fatalf("LoadConfig error = %v, want missing %s", err, agent_turn_burst)
	}
	if got := keelconfig.Config(); got != oldKeel {
		t.Fatalf("keel Config() changed after failed load: got %p, want %p", got, oldKeel)
	}
	if got := Config(); got != oldScout {
		t.Fatalf("scout Config() changed after failed load: got %p, want %p", got, oldScout)
	}
}

func scoutConfigRows() keelconfig.ConfigRows {
	defaults := map[string]string{
		agent_max_tokens:                 "8192",
		agent_temperature:                "",
		agent_run_retention_days:         "0",
		agent_turn_rate:                  "2",
		agent_turn_burst:                 "10",
		agent_tool_rate:                  "10",
		agent_tool_burst:                 "20",
		agent_model_rate:                 "2",
		agent_model_burst:                "5",
		agent_fleet_turn_rate:            "100",
		agent_fleet_turn_burst:           "200",
		agent_fleet_tool_rate:            "500",
		agent_fleet_tool_burst:           "1000",
		agent_fleet_model_rate:           "100",
		agent_fleet_model_burst:          "200",
		agent_max_tenants:                "4096",
		agent_model_capacity_pool:        "shared",
		agent_model_capacity:             "32",
		agent_model_max_waiters:          "4096",
		agent_max_scope_depth:            "8",
		agent_max_delegation_hops:        "4",
		agent_approval_deadline:          "3600",
		agent_credential_ttl:             "300",
		agent_audit_page_size:            "100",
		agent_loop_max_iterations:        "12",
		agent_loop_max_tool_calls:        "24",
		agent_loop_max_tokens:            "200000",
		agent_loop_max_cost:              "0",
		agent_loop_max_repeats:           "3",
		agent_loop_deadline:              "300",
		agent_state_bucket:               "",
		agent_state_max_bytes:            "4194304",
		agent_queue_partitions:           "64",
		agent_queue_shards:               "4",
		agent_queue_max_attempts:         "5",
		agent_queue_lease:                "900",
		agent_queue_batch:                "8",
		agent_session_cache_size:         "4096",
		agent_session_cache_ttl:          "300",
		agent_graph_cache_size:           "1024",
		agent_graph_cache_ttl:            "3600",
		agent_step_claim_lease:           "360",
		agent_turn_max_steps:             "16",
		agent_tool_timeout:               "30",
		agent_tool_max_attempts:          "3",
		agent_guardrail_max_input_bytes:  "262144",
		agent_guardrail_max_output_bytes: "1048576",
		agent_model_region:               "",
		agent_pilot_max_members:          "200",
		agent_pilot_position_timeout:     "2",
		agent_studio_max_agents_per_type: "20",
		agent_governance_required_fields: "",
		agent_evaluation_max_open_runs:   "4",
		agent_evaluation_concurrency:     "4",
		agent_evaluation_max_run_cost:    "10000",
		agent_evaluation_currency:        "",
		agent_evaluation_lease:           "900",
		agent_evaluation_batch:           "4",
		agent_evaluation_max_attempts:    "3",
		agent_evaluation_case_max_bytes:  "32768",
		agent_evaluation_max_cases:       "500",
		agent_activation_batch:           "20",
		agent_activation_poll:            "30",
		agent_activation_max_attempts:    "5",

		agent_evaluation_judge_provider:        "",
		agent_evaluation_judge_model:           "",
		agent_evaluation_judge_model_version:   "",
		agent_evaluation_judge_min_score:       "1",
		agent_evaluation_judge_max_score:       "5",
		agent_evaluation_judge_pass_score:      "4",
		agent_evaluation_judge_max_tokens:      "512",
		agent_evaluation_judge_max_input_bytes: "131072",
		agent_studio_max_templates_per_type:    "20",
		agent_studio_max_template_versions:     "50",

		agent_cost_window:                         "3600",
		agent_reply_retention:                     "600",
		agent_reply_poll_ms:                       "500",
		agent_reply_record_read:                   "5",
		agent_cancel_poll_ms:                      "1000",
		agent_cancel_max_read_failures:            "3",
		agent_session_revision_memory:             "4096",
		agent_result_card_max_bytes:               "65536",
		agent_run_page_max:                        "500",
		agent_loop_max_call_revisions:             "8",
		agent_pause_batch:                         "200",
		agent_studio_report_window_days:           "92",
		agent_studio_usage_max_lines:              "1000",
		agent_work_item_max_depth:                 "32",
		agent_approval_max_candidates:             "20",
		agent_approval_max_asks_per_call:          "8",
		agent_approval_max_revised_argument_bytes: "65536",
		agent_approval_max_routes_per_binding:     "32",
		agent_approval_max_routes:                 "64",
		agent_approval_max_route_tools:            "16",
		agent_approval_min_route_duration:         "60",
		agent_approval_max_route_duration:         "2592000",
		agent_approval_max_route_window:           "7776000",
		agent_confirmation_max_document_bytes:     "1048576",
		agent_confirmation_batch:                  "100",
		agent_confirmation_max_batch:              "1000",
		agent_confirmation_lease:                  "60",
		agent_activation_timeout:                  "120",
		agent_activation_max_lead_days:            "366",
		agent_studio_max_portable_bindings:        "1024",
		agent_studio_max_references:               "1024",
		agent_studio_max_portable_extension_bytes: "262144",
		agent_studio_max_template_audience:        "100",
		agent_studio_usable_templates:             "200",
		agent_studio_max_governance_items:         "32",
		agent_studio_max_comments_shown:           "500",
		agent_studio_max_restriction_entries:      "256",
		agent_restriction_cache_ttl_ms:            "5000",
		agent_restriction_cache_entries:           "4096",
		agent_audit_max_page_size:                 "1000",
		agent_safety_event_max_page:               "500",
		agent_metric_label_max_length:             "64",
		agent_knowledge_max_source_bytes:          "33554432",
		agent_knowledge_rrf_offset:                "60",
		agent_evaluation_page_size:                "50",
		agent_evaluation_max_page_size:            "200",
		agent_evaluation_judge_max_verdict_bytes:  "8192",
		agent_default_language:                    "en-US",
		agent_default_priority_class:              string(domain.PriorityStandard),
		agent_default_test_task:                   "",

		agent_review_feedback_threshold:     "2",
		agent_review_max_pending:            "1000",
		agent_review_page_size:              "50",
		agent_review_max_page_size:          "200",
		agent_review_max_turn_bytes:         "1048576",
		agent_evaluation_gate_max_results:   "2000",
		agent_evaluation_baseline_max_bytes: "32768",
		agent_evaluation_max_baselines:      "20",
		agent_evaluation_compare_page_bytes: "1048576",

		agent_health_period:                   "60",
		agent_health_batch:                    "100",
		agent_health_window:                   "60",
		agent_health_stale_periods:            "3",
		agent_alert_page_size:                 "50",
		agent_alert_max_page_size:             "200",
		agent_evaluation_case_max_turns:       "10",
		agent_evaluation_case_max_tools:       "20",
		agent_evaluation_case_tool_max_bytes:  "16384",
		agent_evaluation_trajectory_max_bytes: "1048576",
		agent_run_trace_max_events:            "500",
		agent_run_trace_max_journal_entries:   "1000",
		agent_run_trace_max_journal_bytes:     "4194304",
		agent_live_turn_page_size:             "50",
		agent_live_turn_max_page_size:         "200",
		agent_outcome_attest_grace:            "60",
		agent_takeover_lease:                  "900",
		agent_operator_entry_max_bytes:        "16384",
		agent_operator_max_entries:            "64",
		agent_hold_lapse_batch:                "100",
		agent_hold_lapse_period:               "30",
	}
	rows := make(keelconfig.ConfigRows, len(defaults))
	for id, value := range defaults {
		rows[id] = keelconfig.ConfigRow{Default: value}
	}
	return rows
}

func allConfigQueryRows(t *testing.T) [][]any {
	t.Helper()
	rows, err := keelschema.ConfigDefaults()
	if err != nil {
		t.Fatalf("keel config defaults: %v", err)
	}
	for id, row := range scoutConfigRows() {
		rows[id] = row
	}
	queryRows := make([][]any, 0, len(rows))
	for id, row := range rows {
		queryRows = append(queryRows, []any{id, row.Value, row.Default, "default"})
	}
	return queryRows
}

func TestScoutConfigDefaultTexts(t *testing.T) {
	rows := scoutConfigRows()
	cfg := &ScoutConfig{}
	if err := cfg.Apply(rows); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentDefaultLanguage != "en-US" || cfg.AgentDefaultPriorityClass != domain.PriorityStandard || cfg.AgentDefaultTestTask != "" {
		t.Fatalf("defaults = %q, %q, %q", cfg.AgentDefaultLanguage, cfg.AgentDefaultPriorityClass, cfg.AgentDefaultTestTask)
	}
	for _, flag := range [][2]string{{agent_default_language, strings.Repeat("x", 17)}, {agent_default_priority_class, "SS"},
		{agent_default_priority_class, "Z"}, {agent_default_priority_class, "standard"}} {
		name := flag[0]
		rows := scoutConfigRows()
		rows[name] = keelconfig.ConfigRow{Value: flag[1]}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: error = %v, want a refusal naming the flag", name, err)
		}
	}
}

func TestRuntimeTuningFlagsReachTheSettingsHelpers(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	settings := cfg.DataPlaneSettings()
	if settings.CostWindow != time.Hour || settings.ReplyRetention != 10*time.Minute || settings.ReplyPollInterval != 500*time.Millisecond ||
		settings.ReplyRecordReadInterval != 5*time.Second || settings.CancelPollInterval != time.Second || settings.CancelMaxReadFailures != 3 ||
		settings.SessionRevisionMemory != 4096 || settings.ResultCardMaxBytes != 64<<10 || settings.RunPageMax != 500 {
		t.Fatalf("data plane settings = %+v", settings)
	}
	if limits := cfg.ToolLoopLimits(); limits.MaxCallRevisions != 8 {
		t.Fatalf("tool loop limits = %+v", limits)
	}
	if cfg.AgentPauseBatch != 200 || cfg.AgentStudioReportWindowDays != 92 || cfg.AgentStudioUsageMaxLines != 1000 || cfg.AgentWorkItemMaxDepth != 32 {
		t.Fatalf("product-wired flags = %d, %d, %d, %d", cfg.AgentPauseBatch, cfg.AgentStudioReportWindowDays, cfg.AgentStudioUsageMaxLines, cfg.AgentWorkItemMaxDepth)
	}

	rows := scoutConfigRows()
	rows[agent_reply_poll_ms] = keelconfig.ConfigRow{Value: "250", Default: "500"}
	rows[agent_cancel_poll_ms] = keelconfig.ConfigRow{Value: "0", Default: "1000"}
	rows[agent_loop_max_call_revisions] = keelconfig.ConfigRow{Value: "2", Default: "8"}
	cfg = &ScoutConfig{}
	if err := cfg.Apply(rows); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DataPlaneSettings(); got.ReplyPollInterval != 250*time.Millisecond || got.CancelPollInterval != 0 {
		t.Fatalf("reply poll = %s, cancel poll = %s; want 250ms and zero (the consumer's default)", got.ReplyPollInterval, got.CancelPollInterval)
	}
	if got := cfg.ToolLoopLimits(); got.MaxCallRevisions != 2 {
		t.Fatalf("call revisions = %d, want 2", got.MaxCallRevisions)
	}
}

func TestRuntimeTuningFlagsRefuseNegativeValues(t *testing.T) {
	for _, flag := range []string{
		agent_cost_window, agent_reply_retention, agent_reply_poll_ms, agent_reply_record_read, agent_cancel_poll_ms,
		agent_cancel_max_read_failures, agent_session_revision_memory, agent_result_card_max_bytes, agent_run_page_max,
		agent_loop_max_call_revisions, agent_pause_batch, agent_studio_report_window_days, agent_studio_usage_max_lines,
		agent_work_item_max_depth,
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: "-1", Default: rows[flag].Default}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s = -1: error = %v, want a refusal naming the flag", flag, err)
		}
	}
}

func TestLimitFlagsParseAndReachTheSettingsHelpers(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentActivationMaxLeadDays != 366 || cfg.AgentStudioMaxPortableBindings != 1024 || cfg.AgentStudioMaxReferences != 1024 ||
		cfg.AgentStudioMaxPortableExtensionBytes != 262144 || cfg.AgentStudioMaxTemplateAudience != 100 || cfg.AgentStudioUsableTemplates != 200 ||
		cfg.AgentStudioMaxGovernanceItems != 32 || cfg.AgentStudioMaxCommentsShown != 500 || cfg.AgentStudioMaxRestrictionEntries != 256 ||
		cfg.AgentMetricLabelMaxLength != 64 || cfg.AgentKnowledgeMaxSourceBytes != 32<<20 || cfg.AgentKnowledgeRRFOffset != 60 ||
		cfg.AgentEvaluationPageSize != 50 || cfg.AgentEvaluationMaxPageSize != 200 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if activation := cfg.ReleaseActivationSettings(); activation.Timeout != 2*time.Minute {
		t.Fatalf("activation timeout = %s, want 2m", activation.Timeout)
	}
	plane := cfg.DataPlaneSettings()
	if plane.AuditPageSize != 100 || plane.AuditMaxPageSize != 1000 || plane.SafetyEventMaxPage != 500 ||
		plane.RestrictionCacheTTL != 5*time.Second || plane.RestrictionCacheEntries != 4096 {
		t.Fatalf("data plane settings = %+v", plane)
	}
	if judge := cfg.RubricJudgeSettings(); judge.MaxVerdictBytes != 8192 {
		t.Fatalf("verdict bytes = %d, want 8192", judge.MaxVerdictBytes)
	}

	rows := scoutConfigRows()
	rows[agent_restriction_cache_ttl_ms] = keelconfig.ConfigRow{Value: "250"}
	rows[agent_activation_timeout] = keelconfig.ConfigRow{Value: "45"}
	rows[agent_max_scope_depth] = keelconfig.ConfigRow{Value: "64"}
	if err := cfg.Apply(rows); err != nil {
		t.Fatal(err)
	}
	if cfg.DataPlaneSettings().RestrictionCacheTTL != 250*time.Millisecond || cfg.ReleaseActivationSettings().Timeout != 45*time.Second ||
		cfg.AgentMaxScopeDepth != 64 {
		t.Fatalf("configured values = %+v", cfg)
	}
}

func TestLimitFlagsRefuseOutOfRangeValues(t *testing.T) {
	for flag, change := range map[string]map[string]string{
		agent_max_scope_depth:                    {agent_max_scope_depth: "65"},
		agent_studio_max_references:              {agent_studio_max_references: "-1"},
		agent_restriction_cache_ttl_ms:           {agent_restriction_cache_ttl_ms: "-5"},
		agent_audit_page_size:                    {agent_audit_page_size: "200", agent_audit_max_page_size: "100"},
		agent_evaluation_page_size:               {agent_evaluation_max_page_size: "40"},
		agent_evaluation_judge_max_verdict_bytes: {agent_evaluation_judge_max_verdict_bytes: "-1"},
	} {
		rows := scoutConfigRows()
		for id, value := range change {
			rows[id] = keelconfig.ConfigRow{Value: value}
		}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s: Apply error = %v, want a refusal naming the flag", flag, err)
		}
	}
	rows := scoutConfigRows()
	rows[agent_max_scope_depth] = keelconfig.ConfigRow{Value: "0"}
	if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_max_scope_depth) {
		t.Fatalf("a zero scope depth: Apply error = %v", err)
	}
}

func TestRubricJudgeSettingsFromFlags(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if settings := cfg.RubricJudgeSettings(); settings.Configured() || settings.MinScore != 1 || settings.MaxScore != 5 || settings.PassScore != 4 ||
		settings.MaxOutputTokens != 512 || settings.MaxInputBytes != 131072 {
		t.Fatalf("default settings = %+v, want an unconfigured 1..5 judge passing at 4", settings)
	}

	rows := scoutConfigRows()
	rows[agent_evaluation_judge_provider] = keelconfig.ConfigRow{Value: "anthropic"}
	rows[agent_evaluation_judge_model] = keelconfig.ConfigRow{Value: "judge-large"}
	rows[agent_evaluation_judge_model_version] = keelconfig.ConfigRow{Value: "2026-01"}
	if err := cfg.Apply(rows); err != nil {
		t.Fatal(err)
	}
	settings := cfg.RubricJudgeSettings()
	if !settings.Configured() || settings.Model != (domain.ModelSelection{Provider: "anthropic", Model: "judge-large", ModelVersion: "2026-01"}) {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestRubricJudgeFlagsAreValidatedWhenAJudgeIsConfigured(t *testing.T) {
	for name, change := range map[string]map[string]string{
		"unpinned version":      {agent_evaluation_judge_provider: "p", agent_evaluation_judge_model: "m"},
		"empty range":           {agent_evaluation_judge_max_score: "1"},
		"pass outside range":    {agent_evaluation_judge_pass_score: "6"},
		"negative lowest grade": {agent_evaluation_judge_min_score: "-1"},
		"no output tokens":      {agent_evaluation_judge_max_tokens: "0"},
		"no input bytes":        {agent_evaluation_judge_max_input_bytes: "0"},
	} {
		rows := scoutConfigRows()
		rows[agent_evaluation_judge_provider] = keelconfig.ConfigRow{Value: "p"}
		rows[agent_evaluation_judge_model] = keelconfig.ConfigRow{Value: "m"}
		rows[agent_evaluation_judge_model_version] = keelconfig.ConfigRow{Value: "1"}
		if name == "unpinned version" {
			rows[agent_evaluation_judge_model_version] = keelconfig.ConfigRow{}
		}
		for id, value := range change {
			rows[id] = keelconfig.ConfigRow{Value: value}
		}
		err := (&ScoutConfig{}).Apply(rows)
		if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "agent_evaluation_judge_") {
			t.Fatalf("%s: Apply error = %v, want a judge validation error", name, err)
		}
	}

	rows := scoutConfigRows()
	rows[agent_evaluation_judge_max_score] = keelconfig.ConfigRow{Value: "1"}
	if err := (&ScoutConfig{}).Apply(rows); err != nil {
		t.Fatalf("judge bounds are not validated while no judge model is configured: %v", err)
	}
}

func TestApprovalAndConfirmationFlagsDefault(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentApprovalMaxCandidates != 20 || cfg.AgentApprovalMaxAsksPerCall != 8 || cfg.AgentApprovalMaxRevisedArgumentBytes != 65536 {
		t.Fatalf("approval flags = %d, %d, %d", cfg.AgentApprovalMaxCandidates, cfg.AgentApprovalMaxAsksPerCall, cfg.AgentApprovalMaxRevisedArgumentBytes)
	}
	if cfg.AgentConfirmationMaxDocumentBytes != 1<<20 || cfg.AgentConfirmationBatch != 100 ||
		cfg.AgentConfirmationMaxBatch != 1000 || cfg.AgentConfirmationLease != 60 {
		t.Fatalf("confirmation flags = %d, %d, %d, %d", cfg.AgentConfirmationMaxDocumentBytes, cfg.AgentConfirmationBatch,
			cfg.AgentConfirmationMaxBatch, cfg.AgentConfirmationLease)
	}
	limits := cfg.ApprovalRoutingLimits()
	ceilings, err := domain.ApprovalRoutingLimits{}.Resolve()
	if err != nil || limits != ceilings {
		t.Fatalf("routing limits = %+v, want the structural bounds %+v (%v)", limits, ceilings, err)
	}
}

func TestApprovalRoutingLimitsFromFlags(t *testing.T) {
	rows := scoutConfigRows()
	for name, value := range map[string]string{
		agent_approval_max_routes_per_binding: "4", agent_approval_max_routes: "10", agent_approval_max_route_tools: "2",
		agent_approval_min_route_duration: "300", agent_approval_max_route_duration: "86400", agent_approval_max_route_window: "172800",
	} {
		rows[name] = keelconfig.ConfigRow{Value: value}
	}
	cfg := &ScoutConfig{}
	if err := cfg.Apply(rows); err != nil {
		t.Fatal(err)
	}
	want := domain.ApprovalRoutingLimits{MaxRoutesPerBinding: 4, MaxRoutes: 10, MaxToolPatterns: 2,
		MinDurationSeconds: 300, MaxDurationSeconds: 86400, MaxWindowSeconds: 172800}
	if got := cfg.ApprovalRoutingLimits(); got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
}

func TestApprovalAndConfirmationFlagsRefuseOutOfRange(t *testing.T) {
	for name, value := range map[string]string{
		agent_approval_max_candidates:             "-1",
		agent_approval_max_asks_per_call:          "-1",
		agent_approval_max_revised_argument_bytes: "-1",
		agent_approval_max_routes_per_binding:     "33",
		agent_approval_max_routes:                 "65",
		agent_approval_max_route_tools:            "17",
		agent_approval_min_route_duration:         "59",
		agent_approval_max_route_duration:         "2592001",
		agent_approval_max_route_window:           "7776001",
		agent_confirmation_max_document_bytes:     "-1",
		agent_confirmation_batch:                  "-1",
		agent_confirmation_max_batch:              "-1",
		agent_confirmation_lease:                  "2147483648",
	} {
		rows := scoutConfigRows()
		rows[name] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%s: error = %v, want a refusal naming the flag", name, value, err)
		}
	}
	for name, values := range map[string]map[string]string{
		agent_approval_min_route_duration: {agent_approval_min_route_duration: "3600", agent_approval_max_route_duration: "600"},
		agent_confirmation_batch:          {agent_confirmation_batch: "200", agent_confirmation_max_batch: "150"},
	} {
		rows := scoutConfigRows()
		for flag, value := range values {
			rows[flag] = keelconfig.ConfigRow{Value: value}
		}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%v: error = %v, want a refusal naming %s", values, err, name)
		}
	}
}

func TestReleaseActivationSettingsFromFlags(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	settings := cfg.ReleaseActivationSettings()
	if settings.Batch != 20 || settings.PollPeriod != 30*time.Second || settings.MaxAttempts != 5 {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestRunTraceFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	want := domain.RunTraceLimits{MaxEvents: dataplane.DefaultRunTraceEvents, MaxJournalEntries: dataplane.DefaultRunTraceJournalEntries,
		MaxJournalBytes: dataplane.DefaultRunTraceJournalBytes}
	if got := cfg.RunTraceLimits(); got != want {
		t.Fatalf("RunTraceLimits() = %+v, want the seeded defaults %+v", got, want)
	}
	zero := scoutConfigRows()
	for _, flag := range []string{agent_run_trace_max_events, agent_run_trace_max_journal_entries, agent_run_trace_max_journal_bytes} {
		zero[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for flag, value := range map[string]string{
		agent_run_trace_max_events: "5001", agent_run_trace_max_journal_entries: "-1",
		agent_run_trace_max_journal_bytes: "1023",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
}

func TestBaselineFlagsTakeTheirDefaultsAndRefuseNegatives(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentEvaluationBaselineMaxBytes != 32768 || cfg.AgentEvaluationMaxBaselines != 20 || cfg.AgentEvaluationComparePageBytes != 1048576 {
		t.Fatalf("baseline flags = %d, %d", cfg.AgentEvaluationBaselineMaxBytes, cfg.AgentEvaluationMaxBaselines)
	}
	for _, flag := range []string{agent_evaluation_baseline_max_bytes, agent_evaluation_max_baselines, agent_evaluation_compare_page_bytes} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: "-1"}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s: Apply error = %v, want a refusal naming the flag", flag, err)
		}
	}
}

func TestHealthFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	want := domain.AgentHealthSettings{Period: time.Minute, Batch: 100, WindowMinutes: 60, StalePeriods: 3, AlertPageSize: 50, AlertMaxPageSize: 200}
	if got := cfg.AgentHealthSettings(); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	for flag, value := range map[string]string{
		agent_health_period: "-1", agent_health_batch: "10001", agent_health_window: "10081",
		agent_health_stale_periods: "1001", agent_alert_page_size: "-2", agent_alert_max_page_size: "1001",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
	rows := scoutConfigRows()
	rows[agent_alert_page_size] = keelconfig.ConfigRow{Value: "300"}
	if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_alert_page_size) {
		t.Fatalf("a default page above the largest page must be refused: %v", err)
	}
	rows = scoutConfigRows()
	for _, flag := range []string{agent_health_period, agent_health_batch, agent_health_window, agent_health_stale_periods, agent_alert_page_size, agent_alert_max_page_size} {
		rows[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	zero := &ScoutConfig{}
	if err := zero.Apply(rows); err != nil || zero.AgentHealthSettings() != (domain.AgentHealthSettings{}) {
		t.Fatalf("zero flags pass through for the service to default: %+v, %v", zero.AgentHealthSettings(), err)
	}
}

func TestLiveTurnFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.LiveTurnLimits(), (domain.LiveTurnLimits{PageSize: 50, MaxPageSize: 200}); got != want {
		t.Fatalf("LiveTurnLimits() = %+v, want the seeded defaults %+v", got, want)
	}
	zero := scoutConfigRows()
	for _, flag := range []string{agent_live_turn_page_size, agent_live_turn_max_page_size} {
		zero[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for flag, value := range map[string]string{
		agent_live_turn_page_size: "-1", agent_live_turn_max_page_size: "1001",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
	// A default page larger than the largest page is refused, naming the default.
	rows := scoutConfigRows()
	rows[agent_live_turn_page_size] = keelconfig.ConfigRow{Value: "300"}
	if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_live_turn_page_size) {
		t.Fatalf("page above the maximum: %v", err)
	}
}

// agent_outcome_attest_grace reaches the data plane in seconds; zero takes the 60-second default
// downstream, and a value outside 0..86400 is refused naming the flag.
func TestOutcomeAttestGraceTakesItsDefaultAndRefusesOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DataPlaneSettings().OutcomeAttestGrace; got != time.Minute {
		t.Fatalf("OutcomeAttestGrace = %s, want the seeded 60s", got)
	}
	zero := scoutConfigRows()
	zero[agent_outcome_attest_grace] = keelconfig.ConfigRow{Value: "0"}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for _, value := range []string{"-1", "86401"} {
		rows := scoutConfigRows()
		rows[agent_outcome_attest_grace] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_outcome_attest_grace) {
			t.Fatalf("%s: Apply error = %v, want a refusal naming the flag", value, err)
		}
	}
}

func TestReviewFlagsParseTheirDefaults(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentReviewFeedbackThreshold != 2 || cfg.AgentReviewMaxPending != 1000 || cfg.AgentReviewPageSize != 50 ||
		cfg.AgentReviewMaxPageSize != 200 || cfg.AgentReviewMaxTurnBytes != 1<<20 {
		t.Fatalf("review defaults = %d %d %d %d %d", cfg.AgentReviewFeedbackThreshold, cfg.AgentReviewMaxPending,
			cfg.AgentReviewPageSize, cfg.AgentReviewMaxPageSize, cfg.AgentReviewMaxTurnBytes)
	}
	rows := scoutConfigRows()
	rows[agent_review_feedback_threshold] = keelconfig.ConfigRow{Value: "0"}
	if err := cfg.Apply(rows); err != nil || cfg.AgentReviewFeedbackThreshold != 0 {
		t.Fatalf("threshold 0 = %d, %v", cfg.AgentReviewFeedbackThreshold, err)
	}
}

func TestReviewFlagsRefuseOutOfRangeValues(t *testing.T) {
	for flag, change := range map[string]map[string]string{
		agent_review_feedback_threshold: {agent_review_feedback_threshold: "6"},
		agent_review_max_pending:        {agent_review_max_pending: "-1"},
		agent_review_max_turn_bytes:     {agent_review_max_turn_bytes: "-1"},
		agent_review_page_size:          {agent_review_max_page_size: "40"},
	} {
		rows := scoutConfigRows()
		for id, value := range change {
			rows[id] = keelconfig.ConfigRow{Value: value}
		}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s: error = %v, want a refusal naming the flag", flag, err)
		}
	}
}

func TestCaseScriptFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentEvaluationCaseMaxTurns != 10 || cfg.AgentEvaluationCaseMaxTools != 20 || cfg.AgentEvaluationCaseToolMaxBytes != 16384 ||
		cfg.AgentEvaluationTrajectoryMaxBytes != 1048576 {
		t.Fatalf("case script flags = %d, %d, %d, %d", cfg.AgentEvaluationCaseMaxTurns, cfg.AgentEvaluationCaseMaxTools,
			cfg.AgentEvaluationCaseToolMaxBytes, cfg.AgentEvaluationTrajectoryMaxBytes)
	}
	zero := scoutConfigRows()
	for _, flag := range []string{agent_evaluation_case_max_turns, agent_evaluation_case_max_tools, agent_evaluation_case_tool_max_bytes, agent_evaluation_trajectory_max_bytes} {
		zero[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for flag, value := range map[string]string{
		agent_evaluation_case_max_turns: "101", agent_evaluation_case_max_tools: "-1",
		agent_evaluation_case_tool_max_bytes: "1", agent_evaluation_trajectory_max_bytes: "1023",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
}
