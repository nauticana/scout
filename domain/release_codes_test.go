package domain

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// releaseCatalogList ties a catalog table to its Go codes and every column that references it.
type releaseCatalogList struct {
	table       string
	codes       []string
	seed        string
	schema      string
	referencing [][2]string
}

func TestReleaseCatalogCodesAgreeWithSeedAndSchema(t *testing.T) {
	lists := []releaseCatalogList{
		{
			table: "approval_status", seed: "catalog/catalog.yml", schema: "catalog/approval_status.yml",
			codes: codes(ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusRejected, ApprovalStatusEdited,
				ApprovalStatusExpired, ApprovalStatusEscalated, ApprovalStatusWithdrawn),
			referencing: [][2]string{{"approval/turn_approval_request.yml", "status_code"}, {"approval/turn_approval_decision.yml", "status_code"}},
		},
		{
			table: "mcp_confirmation_status", seed: "catalog/catalog.yml", schema: "catalog/mcp_confirmation_status.yml",
			codes: codes(MCPConfirmationPending, MCPConfirmationApproved, MCPConfirmationExecuting, MCPConfirmationExecuted,
				MCPConfirmationFailed, MCPConfirmationUnknown, MCPConfirmationDeclined, MCPConfirmationWithdrawn, MCPConfirmationExpired),
			referencing: [][2]string{{"mcp_confirmation/mcp_confirmation.yml", "status_code"}},
		},
		{
			table: "release_activation_state", seed: "catalog/catalog.yml", schema: "catalog/release_activation_state.yml",
			codes:       codes(ActivationPending, ActivationActivated, ActivationRefused, ActivationCancelled),
			referencing: [][2]string{{"agent/agent_release_activation.yml", "state_code"}},
		},
		{
			table: "rollout_status", seed: "catalog/catalog.yml", schema: "catalog/rollout_status.yml",
			codes:       codes(RolloutStatusPending, RolloutStatusActive, RolloutStatusPaused, RolloutStatusCompleted, RolloutStatusRolledBack),
			referencing: [][2]string{{"release/platform_rollout.yml", "status_code"}},
		},
		{
			table: "rollout_stage", seed: "release/release.yml", schema: "release/rollout_stage.yml",
			codes: codes(StageBuild, StageOfflineReplay, StageShadow, StageInternalCanary, StageTenantCanary,
				StageRegionalRamp, StageGlobalDefault, StageRetired, StageRolledBack, StageQuarantined),
			referencing: [][2]string{
				{"release/platform_rollout_state.yml", "stage_code"}, {"release/platform_rollout_bypass.yml", "stage_code"},
				{"release/platform_rollout_transition.yml", "from_stage_code"}, {"release/platform_rollout_transition.yml", "to_stage_code"},
			},
		},
	}
	for _, list := range lists {
		t.Run(list.table, func(t *testing.T) {
			width := len(list.codes[0])
			for _, code := range list.codes {
				if len(code) != width {
					t.Errorf("code %q is not %d characters", code, width)
				}
			}
			if seeded, want := releaseSeededRows(t, list.seed, list.table), sortedCopy(list.codes); !slices.Equal(seeded, want) {
				t.Errorf("seeded codes = %v, want %v", seeded, want)
			}
			wantType := "CHAR(" + strconv.Itoa(width) + ")"
			if got := yamlFieldAfter(t, list.schema, "- name: code", "type:"); got != wantType {
				t.Errorf("%s.code type = %q, want %q", list.table, got, wantType)
			}
			for _, column := range list.referencing {
				if got := yamlFieldAfter(t, column[0], "- name: "+column[1], "type:"); got != wantType {
					t.Errorf("%s %s type = %q, want %q", column[0], column[1], got, wantType)
				}
			}
		})
	}
}

func TestReleaseConstantCodesAgreeWithSeedAndSchema(t *testing.T) {
	lists := []codeList{
		{
			constantID: "rollout_health_verdict",
			codes:      codes(RolloutHealthy, RolloutUnhealthy, RolloutInconclusive),
			valid:      func(code string) bool { return RolloutVerdict(code).Valid() },
			seed:       "evaluation/evaluation.yml", schema: "evaluation/gate_decision.yml",
			column: "verdict_code", columnType: "CHAR(1)", check: "gate_decision_verdict_ck",
		},
		{
			constantID: "version_pin_scope",
			codes:      codes(PinScopeCompliance, PinScopeTenant),
			valid:      func(code string) bool { return PinScope(code).Valid() },
			seed:       "release/release.yml", schema: "release/agent_version_pin.yml",
			column: "scope_code", columnType: "CHAR(1)", check: "agent_version_pin_scope_ck",
		},
		{
			constantID: "pilot_subject_kind",
			codes:      codes(PilotPrincipal, PilotPosition),
			valid: func(code string) bool {
				return slices.Contains([]PilotSubjectKind{PilotPrincipal, PilotPosition}, PilotSubjectKind(code))
			},
			seed: "release/release.yml", schema: "release/agent_pilot_member.yml",
			column: "subject_kind", columnType: "CHAR(1)", check: "agent_pilot_member_subject_ck",
		},
	}
	for _, list := range lists {
		t.Run(list.constantID, func(t *testing.T) {
			for _, code := range list.codes {
				if len(code) != 1 || !list.valid(code) {
					t.Errorf("code %q is not a valid 1-character code", code)
				}
			}
			for _, word := range []string{"", strings.ToLower(list.codes[0]), list.codes[0] + " "} {
				if list.valid(word) {
					t.Errorf("%q must not be valid", word)
				}
			}
			want := sortedCopy(list.codes)
			if seeded := seededCodes(t, list.seed, list.constantID); !slices.Equal(seeded, want) {
				t.Errorf("seed codes = %v, want %v", seeded, want)
			}
			if checked := checkedCodes(t, list.schema, list.check); !slices.Equal(checked, want) {
				t.Errorf("%s codes = %v, want %v", list.check, checked, want)
			}
			if got := yamlFieldAfter(t, list.schema, "- name: "+list.column, "type:"); got != list.columnType {
				t.Errorf("%s type = %q, want %q", list.column, got, list.columnType)
			}
		})
	}
}

// releaseSeededRows returns the first field of every row the seed lists for one catalog table.
func releaseSeededRows(t *testing.T, seed, table string) []string {
	t.Helper()
	var out []string
	inTable := false
	for _, line := range readSchemaFile(t, "seed/"+seed) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- table:") {
			inTable = trimmed == "- table: "+table
			continue
		}
		if row, ok := strings.CutPrefix(trimmed, "- ["); inTable && ok {
			out = append(out, strings.TrimSpace(strings.SplitN(row, ",", 2)[0]))
		}
	}
	slices.Sort(out)
	return out
}
