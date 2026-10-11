package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/nauticana/scout/domain"
)

// revisionDesk plays the approval side: it parks shop.write calls and hands back the
// edit recorded for a call's idempotency key.
type revisionDesk struct {
	edits    map[string]domain.CallRevision
	approved map[string]bool
	reads    int
}

func (desk *revisionDesk) Revision(_ context.Context, call domain.ToolCall) (domain.CallRevision, bool, error) {
	desk.reads++
	revision, ok := desk.edits[call.IdempotencyKey]
	return revision, ok, nil
}

func (desk *revisionDesk) invoke(call domain.ToolCall) (domain.ToolResult, error) {
	if call.ToolID != "shop.write" {
		return domain.ToolResult{}, nil
	}
	if _, edited := desk.edits[call.IdempotencyKey]; edited {
		return domain.ToolResult{}, fmt.Errorf("gate: %w", domain.ErrProposalRevised)
	}
	if !desk.approved[call.IdempotencyKey] {
		return domain.ToolResult{}, fmt.Errorf("gate: %w", domain.ErrApprovalPending)
	}
	return domain.ToolResult{}, nil
}

func revisionOf(step int64, arguments string) domain.CallRevision {
	return domain.CallRevision{RevisesProposal: step, Arguments: []byte(arguments), ArgumentsDigest: DigestBytes([]byte(arguments))}
}

func revisionHarness(t *testing.T) (*loopHarness, *revisionDesk) {
	harness := newLoopHarness(t, proposes(10, toolCall("b", "shop_write", `{"sku":"1","qty":100}`)), answers("published"))
	desk := &revisionDesk{edits: map[string]domain.CallRevision{}, approved: map[string]bool{}}
	harness.invoke, harness.revisions = desk.invoke, desk
	return harness, desk
}

func TestToolLoopRunsTheRevisedCallInPlaceOfTheEditedOne(t *testing.T) {
	harness, desk := revisionHarness(t)
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatalf("want the turn parked, got %v", err)
	}
	original := harness.calls[0]
	desk.edits[original.IdempotencyKey] = revisionOf(77, `{"sku":"1","qty":5}`)
	desk.approved[revisedCall(original, desk.edits[original.IdempotencyKey]).IdempotencyKey] = true

	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	ran := harness.calls[len(harness.calls)-1]
	if string(ran.Arguments) != `{"sku":"1","qty":5}` || ran.RevisesProposal != 77 || ran.IdempotencyKey == original.IdempotencyKey {
		t.Fatalf("resume ran %+v, want the revision under its own key", ran)
	}
	if len(harness.effects) != 1 || harness.effects[ran.IdempotencyKey] != 1 || harness.modelCalls != 2 {
		t.Fatalf("effects %v, model calls %d: only the revision runs, once", harness.effects, harness.modelCalls)
	}
	if shown := harness.lastRequest.Messages[0].ToolCalls[0].Arguments; string(shown) != `{"sku":"1","qty":5}` {
		t.Fatalf("the model was shown %s, want the arguments the call ran with", shown)
	}
	var decisions []string
	var proposed json.RawMessage
	for _, event := range result.Events {
		switch event.Kind {
		case domain.TurnEventApprovalResolved:
			decisions = append(decisions, event.Approval.Decision)
		case domain.TurnEventToolProposal:
			proposed = event.Tool.Arguments
		}
	}
	if !reflect.DeepEqual(decisions, []string{"edited"}) || string(proposed) != `{"sku":"1","qty":5}` {
		t.Fatalf("events: decisions %v, proposal %s", decisions, proposed)
	}
	journal, _ := harness.journal.Load(context.Background(), domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11})
	var kinds []domain.LoopEntryKind
	for _, entry := range journal {
		kinds = append(kinds, entry.Kind)
	}
	want := []domain.LoopEntryKind{domain.LoopEntryModel, domain.LoopEntryApprovalPending, domain.LoopEntryRevision, domain.LoopEntryObservation, domain.LoopEntryModel}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("journal = %v, want %v", kinds, want)
	}
	// The trajectory keeps the model's proposal and shows the decider's substitution before the observation it produced.
	trajectory := LoopTrajectory(journal)
	if len(trajectory) != 5 || string(trajectory[0].Payload) != `{"sku":"1","qty":100}` ||
		trajectory[2].Kind != domain.TrajectoryPolicy || trajectory[2].Name != "shop.write" || string(trajectory[2].Payload) != `{"sku":"1","qty":5}` ||
		trajectory[3].Kind != domain.TrajectoryObservation {
		t.Fatalf("trajectory = %+v", trajectory)
	}

	// Replay substitutes from the journal: the edit is not read again and nothing repeats.
	reads := desk.reads
	delete(desk.edits, original.IdempotencyKey)
	replayed, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil || string(replayed.State) != "published" || desk.reads != reads || len(harness.effects) != 1 || harness.effects[ran.IdempotencyKey] != 1 {
		t.Fatalf("replay = %s, %v, reads %d→%d, effects %v", replayed.State, err, reads, desk.reads, harness.effects)
	}
}

func TestToolLoopParksARevisionThatAwaitsItsOwnApprover(t *testing.T) {
	harness, desk := revisionHarness(t)
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatal(err)
	}
	original := harness.calls[0]
	desk.edits[original.IdempotencyKey] = revisionOf(77, `{"sku":"1","qty":5}`)
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatalf("an unapproved revision parks the turn again, got %v", err)
	}
	revised := harness.calls[len(harness.calls)-1]
	desk.approved[revised.IdempotencyKey] = true
	if _, err := harness.executor().Execute(context.Background(), loopInput()); err != nil {
		t.Fatalf("resume after the revision's approval: %v", err)
	}
	if last := harness.calls[len(harness.calls)-1]; last.IdempotencyKey != revised.IdempotencyKey || harness.effects[revised.IdempotencyKey] != 1 {
		t.Fatalf("ran %+v, effects %v", last, harness.effects)
	}
	journal, _ := harness.journal.Load(context.Background(), domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11})
	if journal[3].Kind != domain.LoopEntryApprovalPending || journal[4].Kind != domain.LoopEntryObservation {
		t.Fatalf("journal = %+v", journal)
	}
}

func TestToolLoopFailsAnEditedCallItCannotRevise(t *testing.T) {
	harness, desk := revisionHarness(t)
	harness.revisions = nil
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatal(err)
	}
	desk.edits[harness.calls[0].IdempotencyKey] = revisionOf(77, `{"qty":5}`)
	_, err := harness.executor().Execute(context.Background(), loopInput())
	if !errors.Is(err, domain.ErrProposalRevised) || harness.modelCalls != 1 || len(harness.effects) != 0 {
		t.Fatalf("an edited call without revisions must end the turn: %v, model calls %d, effects %v", err, harness.modelCalls, harness.effects)
	}
}

func TestToolLoopRefusesARevisionThatDoesNotMatchItsDigest(t *testing.T) {
	harness, desk := revisionHarness(t)
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatal(err)
	}
	forged := revisionOf(77, `{"qty":5}`)
	forged.Arguments = []byte(`{"qty":500}`)
	desk.edits[harness.calls[0].IdempotencyKey] = forged
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrConflict) || len(harness.effects) != 0 {
		t.Fatalf("forged revision: %v, effects %v", err, harness.effects)
	}
}

func TestToolLoopBoundsRevisionsOfOneCall(t *testing.T) {
	harness, desk := revisionHarness(t)
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatal(err)
	}
	var err error
	for round := 0; round <= defaultCallRevisions && !errors.Is(err, domain.ErrExecutionLimit); round++ {
		parked := harness.calls[len(harness.calls)-1]
		desk.edits[parked.IdempotencyKey] = revisionOf(int64(100+round), fmt.Sprintf(`{"qty":%d}`, round))
		_, err = harness.executor().Execute(context.Background(), loopInput())
	}
	if !errors.Is(err, domain.ErrExecutionLimit) || len(harness.effects) != 0 {
		t.Fatalf("endless revisions: %v, effects %v", err, harness.effects)
	}
}

func TestRevisionEntrySurvivesTheJournalEncoding(t *testing.T) {
	revision := revisionOf(77, `{"qty":5}`)
	entry := domain.LoopEntry{EntryNo: 3, Kind: domain.LoopEntryRevision, Iteration: 1, Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"},
		Observation: &domain.ModelToolObservation{CallID: "b", Name: "shop_write"}, Revision: &revision}
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.LoopEntry
	if err := json.Unmarshal(payload, &decoded); err != nil || !sameLoopEntry(decoded, entry) {
		t.Fatalf("decoded %+v, %v", decoded, err)
	}
}
