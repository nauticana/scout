package scout

import (
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"

	"github.com/nauticana/scout/domain"
)

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
