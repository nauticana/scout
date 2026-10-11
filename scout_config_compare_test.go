package scout

import (
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"
)

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
