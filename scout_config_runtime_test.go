package scout

import (
	"strings"
	"testing"
	"time"

	keelconfig "github.com/nauticana/keel/config"
)

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
