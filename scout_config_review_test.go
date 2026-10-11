package scout

import (
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"
)

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
