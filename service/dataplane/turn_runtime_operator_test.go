package dataplane

import (
	"context"
	"slices"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type operatorEntriesFunc func(context.Context, int64, string) ([]domain.OperatorEntry, error)

func (function operatorEntriesFunc) OperatorEntries(ctx context.Context, tenantID int64, requestID string) ([]domain.OperatorEntry, error) {
	return function(ctx, tenantID, requestID)
}

// operatedEntries are what a person did while holding the turn after its first step: a note, a
// reply published at sequence 1 between the two step frames, and a handback instruction.
func operatedEntries() []domain.OperatorEntry {
	at := domain.OperatorPosition{StepNo: 2}
	reply := domain.TurnReply{TenantID: 7, RequestID: "request-1", ConversationID: "conversation-1", ReplyRoute: "route:request-1",
		Sequence: 1, Fence: 3, Payload: []byte("we are on it"), AgentVersion: "v1",
		Events: []domain.TurnEvent{{Version: domain.TurnEventVersion, Kind: domain.TurnEventOperatorReply}}}
	return []domain.OperatorEntry{
		{Fence: 3, EntryNo: 1, Kind: domain.OperatorNote, Position: at, Body: domain.OperatorEntryBody{Text: "the customer is a VIP"}},
		{Fence: 3, EntryNo: 2, Kind: domain.OperatorReply, Position: at, Body: domain.OperatorEntryBody{Frame: &reply}},
		{Fence: 3, EntryNo: 3, Kind: domain.OperatorHandback, Position: at, Body: domain.OperatorEntryBody{Text: "refund in full"}},
	}
}

// §5: on resume a step is handed the entries positioned at it through StepInput.Operator, and a
// reply a person sent is published again at its sequence under the new owner's fence, so the
// agent's later frames continue after it.
func TestTurnRuntimeHandsOperatorEntriesToTheirStepAndRepublishesReplies(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Operator = operatorEntriesFunc(func(_ context.Context, tenantID int64, requestID string) ([]domain.OperatorEntry, error) {
		if tenantID != 7 || requestID != "request-1" {
			t.Errorf("entries read for %d/%s", tenantID, requestID)
		}
		return operatedEntries(), nil
	})
	handed := map[string][]int{}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			for _, entry := range input.Operator {
				handed[input.Step.StepID] = append(handed[input.Step.StepID], entry.EntryNo)
			}
			return stepExecutor(recorder).Execute(ctx, input)
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatal(err)
	}
	if len(handed["step-1"]) != 0 || !slices.Equal(handed["step-2"], []int{1, 2, 3}) {
		t.Fatalf("handed = %v", handed)
	}
	var sequences []int64
	for _, frame := range recorder.frames {
		sequences = append(sequences, frame.Sequence)
	}
	if !slices.Equal(sequences, []int64{0, 1, 2, 3}) {
		t.Fatalf("sequences = %v", sequences)
	}
	reply := recorder.frames[1]
	if string(reply.Payload) != "we are on it" || reply.Fence != runtimeOwner.Fence || reply.Final || reply.Events[0].Kind != domain.TurnEventOperatorReply {
		t.Fatalf("republished reply = %+v", reply)
	}
}

// A terminal replay's final frame follows every reply a person sent.
func TestTurnRuntimeTerminalReplayCountsOperatorReplies(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	recorder.snapshot = domain.SessionSnapshot{ConversationID: "conversation-1", AgentVersion: "v1", Revision: 3, LatestTurnNo: 1, LatestStepNo: 2}
	runtime.Records = &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (domain.TurnRecord, error) {
		return domain.TurnRecord{TurnNo: 1, Status: domain.TurnCompleted, Fence: 5, Payload: []byte("done")}, nil
	}}
	runtime.Operator = operatorEntriesFunc(func(context.Context, int64, string) ([]domain.OperatorEntry, error) { return operatedEntries(), nil })
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.frames) != 1 || !recorder.frames[0].Final || recorder.frames[0].Sequence != 3 {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}
