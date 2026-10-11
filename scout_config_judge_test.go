package scout

import (
	"errors"
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"

	"github.com/nauticana/scout/domain"
)

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
