package domain

import (
	"slices"
	"strings"
	"testing"
)

// runtimeCatalogList is one catalog table whose seeded codes a Go type defines, with every
// column that references it.
type runtimeCatalogList struct {
	table   string
	codes   []string
	valid   func(string) bool
	columns [][2]string
}

func TestRuntimeCatalogCodesAgreeWithSeedAndSchema(t *testing.T) {
	lists := []runtimeCatalogList{
		{
			table: "turn_status",
			codes: codes(TurnQueued, TurnRunning, TurnStreaming, TurnSuspended, TurnCompleted, TurnFailed, TurnCancelled),
			valid: func(code string) bool { return TurnStatus(code).Valid() },
			columns: [][2]string{{"runtime/conversation_turn.yml", "status_code"}, {"runtime/agent_work_item.yml", "status_code"},
				{"runtime/agent_run.yml", "status_code"}},
		},
		{
			table: "idempotency_status", codes: codes(StepClaimed, StepCommitted, StepAbandoned),
			valid:   func(code string) bool { return StepIdempotencyStatus(code).Valid() },
			columns: [][2]string{{"runtime/step_idempotency.yml", "status_code"}},
		},
		{
			table: "reservation_status", codes: codes(ReservationHeld, ReservationSettled, ReservationReleased, ReservationExpired),
			valid:   func(code string) bool { return ReservationStatus(code).Valid() },
			columns: [][2]string{{"runtime/budget_reservation.yml", "status_code"}},
		},
		{
			table: "priority_class", codes: codes(PriorityInteractive, PriorityStandard, PriorityBatch),
			valid:   func(code string) bool { return PriorityClass(code).Valid() },
			columns: [][2]string{{"tenancy/tenant_runtime_policy.yml", "priority_class_code"}, {"model/tenant_model_access.yml", "priority_class_code"}},
		},
		{
			table: "audit_decision_outcome", codes: codes(DecisionAllow, DecisionDeny, DecisionPending),
			valid:   func(code string) bool { return DecisionOutcome(code).Valid() },
			columns: [][2]string{{"configuration/audit_event.yml", "outcome_code"}},
		},
		{
			table:   "agent_state",
			codes:   codes(AgentStateDraft, AgentStateActive, AgentStateSuspended, AgentStateDraining, AgentStateRetired, AgentStateArchived),
			valid:   func(code string) bool { return AgentState(code).Valid() },
			columns: [][2]string{{"agent/agent_profile.yml", "state_code"}},
		},
	}
	for _, list := range lists {
		t.Run(list.table, func(t *testing.T) {
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
			if seeded := runtimeCatalogSeededCodes(t, list.table); !slices.Equal(seeded, sortedCopy(list.codes)) {
				t.Errorf("seeded %s codes = %v, want %v", list.table, seeded, sortedCopy(list.codes))
			}
			if got := yamlFieldAfter(t, "catalog/"+list.table+".yml", "- name: code", "type:"); got != "CHAR(1)" {
				t.Errorf("%s.code type = %q, want CHAR(1)", list.table, got)
			}
			for _, column := range list.columns {
				if got := yamlFieldAfter(t, column[0], "- name: "+column[1], "type:"); got != "CHAR(1)" {
					t.Errorf("%s %s type = %q, want CHAR(1)", column[0], column[1], got)
				}
			}
		})
	}
}

func TestRuntimeCheckedCodesAgreeWithSeedAndSchema(t *testing.T) {
	lists := []codeList{
		{
			constantID: "turn_queue_status", codes: codes(QueueQueued, QueueLeased, QueueAcked, QueueDead),
			valid: func(code string) bool { return TurnQueueStatus(code).Valid() },
			seed:  "runtime/runtime.yml", schema: "runtime/turn_queue.yml",
			column: "status_code", columnType: "CHAR(1)", check: "turn_queue_status_ck",
		},
		{
			constantID: "agent_run_status", codes: codes(RunCompleted, RunFailed, RunCancelled),
			valid: func(code string) bool { return RunStatus(code).Valid() },
			seed:  "runtime/runtime.yml", schema: "runtime/agent_run.yml",
			column: "status_code", columnType: "CHAR(1)", check: "agent_run_status_ck",
		},
	}
	for _, list := range lists {
		t.Run(list.constantID, func(t *testing.T) {
			want := sortedCopy(list.codes)
			for _, code := range list.codes {
				if len(code) != 1 || !list.valid(code) {
					t.Errorf("code %q is not a valid 1-character code", code)
				}
			}
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
	if run, turn := codes(RunCompleted, RunFailed, RunCancelled), codes(TurnCompleted, TurnFailed, TurnCancelled); !slices.Equal(run, turn) {
		t.Errorf("agent_run_status codes %v must equal the terminal turn_status codes %v they reference", run, turn)
	}
	for _, status := range []TurnStatus{TurnCompleted, TurnFailed, TurnCancelled} {
		if !status.Terminal() || !RunStatus(status).Valid() {
			t.Errorf("terminal turn status %q must be terminal and a run status", status)
		}
	}
	for _, status := range []TurnStatus{TurnQueued, TurnRunning, TurnStreaming, TurnSuspended} {
		if status.Terminal() || RunStatus(status).Valid() {
			t.Errorf("live turn status %q must be neither terminal nor a run status", status)
		}
	}
	reservation := codes(ReservationHeld, ReservationSettled, ReservationReleased, ReservationExpired)
	for _, code := range checkedCodes(t, "runtime/budget_reservation.yml", "budget_reservation_settled_ck") {
		if !slices.Contains(reservation, code) {
			t.Errorf("budget_reservation_settled_ck names %q, which is no reservation_status code", code)
		}
	}
	if lease := checkedCodes(t, "runtime/turn_queue.yml", "turn_queue_lease_ck"); !slices.Equal(lease, codes(QueueLeased)) {
		t.Errorf("turn_queue_lease_ck codes = %v, want the leased code", lease)
	}
	if got := yamlFieldAfter(t, "runtime/turn_queue.yml", "- name: status_code", "default:"); got != string(QueueQueued) {
		t.Errorf("turn_queue.status_code default = %q, want %q", got, QueueQueued)
	}
	if got := yamlFieldAfter(t, "runtime/agent_work_item.yml", "- name: status_code", "default:"); got != string(TurnQueued) {
		t.Errorf("agent_work_item.status_code default = %q, want %q", got, TurnQueued)
	}
	if got := yamlFieldAfter(t, "agent/agent_profile.yml", "- name: state_code", "default:"); got != string(AgentStateActive) {
		t.Errorf("agent_profile.state_code default = %q, want %q", got, AgentStateActive)
	}
}

// runtimeCatalogSeededCodes returns the first-column values of one catalog table's seed rows.
func runtimeCatalogSeededCodes(t *testing.T, table string) []string {
	t.Helper()
	var out []string
	inTable := false
	for _, line := range readSchemaFile(t, "seed/catalog/catalog.yml") {
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
