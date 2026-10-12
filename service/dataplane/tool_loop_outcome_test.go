package dataplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

// reconcilerFake answers lookups from a scripted report and declares traits by tool id.
type reconcilerFake struct {
	traits map[string]domain.ToolOutcomeTraits
	report domain.CallReport
	err    error
	calls  []domain.ToolCall
}

func (r *reconcilerFake) Reconcile(_ context.Context, call domain.ToolCall) (domain.CallReport, error) {
	r.calls = append(r.calls, call)
	return r.report, r.err
}

func (r *reconcilerFake) Traits(toolID string) domain.ToolOutcomeTraits { return r.traits[toolID] }

var writeKey = domain.CallOutcomeKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11, CallID: "b"}

func writeOnly() []domain.ModelResult {
	return []domain.ModelResult{proposes(10, toolCall("b", "shop_write", `{"sku":"1"}`)), answers("done")}
}

func (harness *loopHarness) fences() *MemoryTurnFences { return fencesOf(harness.journal) }

// reacquire hands the turn to its next owner, as a redelivery or release does.
func (harness *loopHarness) reacquire(t *testing.T) domain.StepInput {
	t.Helper()
	owner, err := harness.fences().Acquire(7, "request-1")
	if err != nil {
		t.Fatal(err)
	}
	input := loopInput()
	input.Owner = owner
	return input
}

func (harness *loopHarness) loaded(t *testing.T) []domain.LoopEntry {
	t.Helper()
	journal, err := harness.journal.Load(context.Background(), domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11})
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func (harness *loopHarness) outcome(t *testing.T) domain.CallOutcome {
	t.Helper()
	outcome, found, err := harness.outcomes.Find(context.Background(), writeKey)
	if err != nil || !found {
		t.Fatalf("no outcome of call b: %v", err)
	}
	return outcome
}

// §4 intent before effect: a mutating call is recorded in flight before it leaves and settled
// with its observation; a read-only call records nothing.
func TestToolLoopRecordsTheIntentBeforeAMutatingCallLeaves(t *testing.T) {
	harness := newLoopHarness(t, proposes(10, toolCall("a", "search", `{"q":"x"}`), toolCall("b", "shop_write", `{"sku":"1"}`)), answers("done"))
	harness.executor()
	var atSend []domain.CallOutcomeStatus
	harness.invoke = func(call domain.ToolCall) (domain.ToolResult, error) {
		recorded, found, _ := harness.outcomes.Find(context.Background(), domain.CallOutcomeKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11, CallID: call.ToolID})
		if call.ToolID == "shop.write" {
			recorded, found, _ = harness.outcomes.Find(context.Background(), writeKey)
		}
		if found {
			atSend = append(atSend, recorded.Status)
		} else {
			atSend = append(atSend, "")
		}
		return domain.ToolResult{}, nil
	}
	if _, err := harness.executor().Execute(context.Background(), loopInput()); err != nil {
		t.Fatal(err)
	}
	if len(atSend) != 2 || atSend[0] != "" || atSend[1] != domain.CallInFlight {
		t.Fatalf("records at send = %v, want none for the read-only call and in flight for the mutation", atSend)
	}
	settled := harness.outcome(t)
	if settled.Status != domain.CallCompleted || settled.Resolution != domain.ResolvedByGateway || settled.IdempotencyKey != harness.calls[1].IdempotencyKey {
		t.Fatalf("settled = %+v", settled)
	}
	if listed, _ := harness.outcomes.List(context.Background(), 7, "request-1"); len(listed) != 1 {
		t.Fatalf("a read-only call wrote a record: %+v", listed)
	}
}

// §8.5: an unknown effect never reaches the model as a failure: no observation is journaled, the
// record is unknown, and the step returns ErrOutcomeUnknown for the turn to hold. A transport
// failure after a mutating call left is unknown the same way; a read-only call's is an observation.
func TestToolLoopHoldsOnAnUnknownOutcomeWithoutTellingTheModel(t *testing.T) {
	for name, failure := range map[string]error{
		"effect unknown":  fmt.Errorf("%w: tool %q: the effect could not be observed", domain.ErrEffectUnknown, "shop.write"),
		"transport reset": errors.New("read tcp: connection reset by peer"),
	} {
		harness := newLoopHarness(t, writeOnly()...)
		harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) { return domain.ToolResult{}, failure }
		_, err := harness.executor().Execute(context.Background(), loopInput())
		if !errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Fatalf("%s: error = %v, want ErrOutcomeUnknown", name, err)
		}
		if journal := harness.loaded(t); len(journal) != 1 || journal[0].Kind != domain.LoopEntryModel || harness.modelCalls != 1 {
			t.Fatalf("%s: journal %+v, model calls %d; no observation may be made up", name, journal, harness.modelCalls)
		}
		if outcome := harness.outcome(t); outcome.Status != domain.CallUnknown {
			t.Fatalf("%s: outcome = %+v", name, outcome)
		}
	}
	readOnly := newLoopHarness(t, proposes(10, toolCall("a", "search", `{"q":"x"}`)), answers("done"))
	readOnly.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, errors.New("connection reset")
	}
	if result, err := readOnly.executor().Execute(context.Background(), loopInput()); err != nil || string(result.State) != "done" {
		t.Fatalf("a read-only failure stays an observation: %s, %v", result.State, err)
	}
}

// A definite failure settles the record failed with an error observation; a call parked for
// approval never left, so it keeps no record.
func TestToolLoopSettlesADefiniteFailureAndKeepsNoRecordOfAParkedCall(t *testing.T) {
	for name, failure := range map[string]error{
		"no effect": fmt.Errorf("%w: tool refused the order", domain.ErrNoEffect),
		"not sent":  fmt.Errorf("%w: %w: tool rate", domain.ErrNotSent, domain.ErrRateLimited),
	} {
		harness := newLoopHarness(t, writeOnly()...)
		harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) { return domain.ToolResult{}, failure }
		if _, err := harness.executor().Execute(context.Background(), loopInput()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		seen := harness.lastRequest.Messages[1].Observations[0]
		if outcome := harness.outcome(t); outcome.Status != domain.CallFailed || !seen.IsError {
			t.Fatalf("%s: outcome %+v, observation %+v", name, outcome, seen)
		}
	}
	parked := newLoopHarness(t, writeOnly()...)
	parked.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("%w: %w", domain.ErrNotSent, domain.ErrApprovalPending)
	}
	if _, err := parked.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatalf("want ErrApprovalPending, got %v", err)
	}
	if _, found, _ := parked.outcomes.Find(context.Background(), writeKey); found {
		t.Fatal("a call parked before it left must keep no record")
	}
}

// §8.4: an intent committed before a crash, its call never observed, is unknown on resume and the
// turn holds again without resending; once a lookup settles it, the resumed loop journals the
// observation from the record and still never sends the call again.
func TestToolLoopResumesAnUnobservedCallFromItsRecord(t *testing.T) {
	harness := newLoopHarness(t, writeOnly()...)
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		// The worker dies after the intent is durable, before the transport returns.
		harness.fences().Hold(7, "request-1")
		return domain.ToolResult{}, context.Canceled
	}
	if _, err := harness.executor().Execute(context.Background(), loopInput()); err == nil {
		t.Fatal("the dead worker must not succeed")
	}
	if outcome := harness.outcome(t); outcome.Status != domain.CallInFlight || outcome.DispatchFence != 1 {
		t.Fatalf("the stale intent = %+v", outcome)
	}
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		t.Fatal("an unobserved call must never be sent again")
		return domain.ToolResult{}, nil
	}
	if _, err := harness.executor().Execute(context.Background(), harness.reacquire(t)); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("resume without a source = %v, want ErrOutcomeUnknown", err)
	}
	if outcome := harness.outcome(t); outcome.Status != domain.CallUnknown {
		t.Fatalf("resumed record = %+v", outcome)
	}
	reconciler := &reconcilerFake{report: domain.CallReport{Status: domain.CallCompleted, Resolution: domain.ResolvedByLookup,
		Result: domain.ToolResult{Output: []byte(`{"order":"o-1"}`), Usage: domain.Usage{ToolCalls: 1}}}}
	harness.reconciler = reconciler
	result, err := harness.executor().Execute(context.Background(), harness.reacquire(t))
	if err != nil || string(result.State) != "done" || len(reconciler.calls) != 1 || reconciler.calls[0].IdempotencyKey != harness.calls[0].IdempotencyKey ||
		string(reconciler.calls[0].Arguments) != `{"sku":"1"}` {
		t.Fatalf("resolved resume = %s, %v, lookups %+v", result.State, err, reconciler.calls)
	}
	journal := harness.loaded(t)
	if len(journal) != 3 || journal[1].Kind != domain.LoopEntryObservation || string(journal[1].Observation.Output) != `{"order":"o-1"}` {
		t.Fatalf("journal = %+v", journal)
	}
	if outcome := harness.outcome(t); outcome.Status != domain.CallCompleted || outcome.Resolution != domain.ResolvedByLookup {
		t.Fatalf("record = %+v", outcome)
	}
}

// A record settled outside the loop, by attestation or a late report, is journaled as it stands
// with its resolution; the call is not sent.
func TestToolLoopJournalsARecordSettledElsewhere(t *testing.T) {
	for name, settle := range map[string]func(*loopHarness) error{
		"attested": func(harness *loopHarness) error {
			_, err := harness.outcomes.Attest(context.Background(), domain.CallAttestation{Key: writeKey,
				Attester: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, Outcome: domain.CallFailed, Note: "never arrived", Persons: 1, TurnFence: 2})
			return err
		},
		"late report": func(harness *loopHarness) error {
			return harness.outcomes.Report(context.Background(), domain.CallSettlement{Key: writeKey, Status: domain.CallCompleted,
				Resolution: domain.ResolvedByWorker, Record: domain.CallRecord{Output: []byte(`{"late":true}`)}}, 1)
		},
	} {
		harness := newLoopHarness(t, writeOnly()...)
		harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
			harness.fences().Hold(7, "request-1")
			return domain.ToolResult{}, context.Canceled
		}
		harness.executor().Execute(context.Background(), loopInput())
		harness.outcomes.Now = func() time.Time { return time.Now().Add(time.Hour) }
		if err := settle(harness); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
			t.Fatalf("%s: a settled call was sent again", name)
			return domain.ToolResult{}, nil
		}
		if _, err := harness.executor().Execute(context.Background(), harness.reacquire(t)); err != nil {
			t.Fatalf("%s: resume = %v", name, err)
		}
		entry := harness.loaded(t)[1]
		outcome := harness.outcome(t)
		if entry.Outcome == nil || entry.Outcome.Status != outcome.Status || entry.Outcome.Resolution != outcome.Resolution ||
			entry.Observation.IsError != (outcome.Status == domain.CallFailed) {
			t.Fatalf("%s: journaled %+v from %+v", name, entry, outcome)
		}
	}
}

// Decision 3: only a tool the composition declares deduplicating is sent again, under the same key.
func TestToolLoopResendsOnlyADeclaredDeduplicatingCall(t *testing.T) {
	harness := newLoopHarness(t, writeOnly()...)
	harness.reconciler = &reconcilerFake{traits: map[string]domain.ToolOutcomeTraits{"shop.write": {DedupesByKey: true}},
		report: domain.CallReport{Status: domain.CallUnknown}}
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		harness.fences().Hold(7, "request-1")
		return domain.ToolResult{}, context.Canceled
	}
	harness.executor().Execute(context.Background(), loopInput())
	first := harness.calls[0].IdempotencyKey
	harness.invoke = nil
	if result, err := harness.executor().Execute(context.Background(), harness.reacquire(t)); err != nil || string(result.State) != "done" {
		t.Fatalf("resend = %s, %v", result.State, err)
	}
	if len(harness.calls) != 2 || harness.calls[1].IdempotencyKey != first {
		t.Fatalf("calls = %+v, want one resend under key %s", harness.calls, first)
	}
	if outcome := harness.outcome(t); outcome.Status != domain.CallCompleted || outcome.DispatchFence != 3 {
		t.Fatalf("resent record = %+v", outcome)
	}
}

// An effect proven absent settles failed only once the call cannot still be in flight.
func TestToolLoopTrustsAnAbsentEffectOnlyAfterTheDeadline(t *testing.T) {
	harness := newLoopHarness(t, writeOnly()...)
	harness.reconciler = &reconcilerFake{report: domain.CallReport{Status: domain.CallFailed, Resolution: domain.ResolvedByEffect}}
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		harness.fences().Hold(7, "request-1")
		return domain.ToolResult{}, context.Canceled
	}
	harness.executor().Execute(context.Background(), loopInput())
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		t.Fatal("the call must not be sent again")
		return domain.ToolResult{}, nil
	}
	if _, err := harness.executor().Execute(context.Background(), harness.reacquire(t)); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("an absent effect before the deadline = %v, want ErrOutcomeUnknown", err)
	}
	harness.outcomes.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := harness.executor().Execute(context.Background(), harness.reacquire(t)); err != nil {
		t.Fatalf("after the deadline = %v", err)
	}
	if outcome := harness.outcome(t); outcome.Status != domain.CallFailed || outcome.Resolution != domain.ResolvedByEffect {
		t.Fatalf("record = %+v", outcome)
	}
}

// Decision 15: an acceptance is valid only from a tool whose composition declares lookup support;
// otherwise it is invalid output of a sent call, unknown. An accepted call awaits its lookup.
func TestToolLoopAcceptsOnlyFromADeclaredLookupTool(t *testing.T) {
	accept := func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte{}, Acceptance: &domain.ToolAcceptance{Handle: "job-7", LookupWithin: time.Minute}}, nil
	}
	undeclared := newLoopHarness(t, writeOnly()...)
	undeclared.invoke = accept
	if _, err := undeclared.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrOutcomeUnknown) || undeclared.outcome(t).Status != domain.CallUnknown {
		t.Fatalf("an undeclared acceptance = %v, %+v", err, undeclared.outcome(t))
	}
	declared := newLoopHarness(t, writeOnly()...)
	lookup := &reconcilerFake{traits: map[string]domain.ToolOutcomeTraits{"shop.write": {Lookup: true}}, report: domain.CallReport{Status: domain.CallUnknown}}
	declared.reconciler = lookup
	declared.invoke = accept
	if _, err := declared.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("an accepted call awaiting its lookup = %v", err)
	}
	if outcome := declared.outcome(t); outcome.Status != domain.CallAccepted || len(lookup.calls) != 1 {
		t.Fatalf("accepted record = %+v, lookups %d", outcome, len(lookup.calls))
	}
	lookup.report = domain.CallReport{Status: domain.CallCompleted, Resolution: domain.ResolvedByLookup, Result: domain.ToolResult{Output: []byte(`{"job":"done"}`)}}
	declared.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		t.Fatal("an accepted call must not be sent again")
		return domain.ToolResult{}, nil
	}
	if result, err := declared.executor().Execute(context.Background(), declared.reacquire(t)); err != nil || string(result.State) != "done" {
		t.Fatalf("resume after the lookup = %s, %v", result.State, err)
	}
	invalid := newLoopHarness(t, writeOnly()...)
	invalid.reconciler = lookup
	invalid.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Acceptance: &domain.ToolAcceptance{Handle: " ", LookupWithin: time.Minute}}, errors.New("x")
	}
	if _, err := invalid.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("a failed acceptance = %v", err)
	}
}

// §4 late report: a worker fenced out while its call ran settles only its own dispatch with what
// came back, as evidence; its journal append is refused and the next owner journals it.
func TestToolLoopReportsALateResultOfAFencedOutWorker(t *testing.T) {
	harness := newLoopHarness(t, writeOnly()...)
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		harness.fences().Hold(7, "request-1")
		return domain.ToolResult{Output: []byte(`{"order":"o-9"}`), Usage: domain.Usage{ToolCalls: 1}}, nil
	}
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("the fenced-out worker = %v, want ErrFenceLost", err)
	}
	outcome := harness.outcome(t)
	if outcome.Status != domain.CallCompleted || outcome.Resolution != domain.ResolvedByWorker || outcome.ResolvedFence != 1 || len(harness.loaded(t)) != 1 {
		t.Fatalf("late report = %+v, journal %d entries", outcome, len(harness.loaded(t)))
	}
	harness.invoke = nil
	if result, err := harness.executor().Execute(context.Background(), harness.reacquire(t)); err != nil || string(result.State) != "done" || len(harness.calls) != 1 {
		t.Fatalf("the next owner = %s, %v, calls %d", result.State, err, len(harness.calls))
	}
	if entry := harness.loaded(t)[1]; string(entry.Observation.Output) != `{"order":"o-9"}` || entry.Outcome.Resolution != domain.ResolvedByWorker {
		t.Fatalf("journaled = %+v", entry)
	}
}

// A decision may not reuse a call id of its step: the id binds the call's key, approval, and record.
func TestToolLoopRefusesAReusedCallID(t *testing.T) {
	harness := newLoopHarness(t, proposes(10, toolCall("b", "shop_write", `{"sku":"1"}`)), proposes(10, toolCall("b", "shop_write", `{"sku":"2"}`)))
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrInvalidModelOutput) || len(harness.calls) != 1 {
		t.Fatalf("a reused call id = %v, calls %d", err, len(harness.calls))
	}
}
