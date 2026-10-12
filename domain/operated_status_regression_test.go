package domain

import (
	"slices"
	"testing"
)

// Slice 3's closed codes: the operated status and the operator call origin are valid codes, the
// operated status is live, and the CHECKs that tie hold columns to status admit it.
func TestTheOperatedCodesAreSeededAndChecked(t *testing.T) {
	if status := TurnStatus("O"); !status.Valid() || status.Terminal() {
		t.Fatalf("O valid = %t, terminal = %t", status.Valid(), status.Terminal())
	}
	if !CallOrigin("O").Valid() {
		t.Fatal("the operator call origin is not valid")
	}
	if checked := checkedCodes(t, "runtime/conversation_turn.yml", "conversation_turn_hold_until_ck"); !slices.Equal(checked, []string{"O"}) {
		t.Fatalf("conversation_turn_hold_until_ck codes = %v", checked)
	}
	if checked := checkedCodes(t, "runtime/conversation_turn.yml", "conversation_turn_held_code_ck"); !slices.Equal(checked, []string{"H", "O"}) {
		t.Fatalf("conversation_turn_held_code_ck codes = %v", checked)
	}
	if got := yamlFieldAfter(t, "runtime/budget_reservation.yml", "- name: budget_reservation_request_attempt_uq", "columns:"); got != "[partner_id, request_id, principal_kind, attempt_no]" {
		t.Fatalf("a person's holds collide with the agent's attempts: %s", got)
	}
}
