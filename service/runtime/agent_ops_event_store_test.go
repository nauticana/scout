package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestAgentOpsEventStoreRecordsOpenEvent(t *testing.T) {
	query := &agentRunQueryFake{rows: map[string][][]any{}}
	store := &AgentOpsEventStore{qs: query}
	if err := store.RecordOperationalEvent(context.Background(), 8, " PROVISION_FAILED ", " unavailable "); err != nil {
		t.Fatalf("RecordOperationalEvent: %v", err)
	}
	args := query.args[qRecordAgentOpsEvent]
	if len(args) != 3 || args[0] != int64(8) || args[1] != "PROVISION_FAILED" || args[2] != "unavailable" {
		t.Fatalf("event args = %+v", args)
	}
	if err := store.RecordOperationalEvent(context.Background(), 8, "EVENT_WITHOUT_DETAIL", ""); err != nil {
		t.Fatalf("RecordOperationalEvent without detail: %v", err)
	}
	if query.args[qRecordAgentOpsEvent][2] != nil {
		t.Fatalf("empty detail = %#v, want nil", query.args[qRecordAgentOpsEvent][2])
	}
}

func TestAgentOpsEventStoreValidatesIdentity(t *testing.T) {
	store := &AgentOpsEventStore{qs: &agentRunQueryFake{rows: map[string][][]any{}}}
	if err := store.RecordOperationalEvent(context.Background(), 0, "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation error = %v", err)
	}
}
