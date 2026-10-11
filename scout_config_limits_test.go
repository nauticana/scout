package scout

import (
	"strings"
	"testing"
	"time"

	keelconfig "github.com/nauticana/keel/config"
)

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
