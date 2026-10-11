package scout

import (
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"
)

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
