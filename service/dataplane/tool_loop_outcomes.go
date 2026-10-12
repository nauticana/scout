package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// outgoingCall is one non-read-only call at the journal cursor: its record's key and how it is sent.
type outgoingCall struct {
	key        domain.CallOutcomeKey
	reference  domain.ToolReference
	call       domain.ModelToolCall
	proposal   domain.ToolCall
	iteration  int
	wasPending bool
	// resend marks a call sent again under its key: an earlier dispatch may have landed.
	resend bool
}

func (run *loopRun) callKey(call domain.ModelToolCall) domain.CallOutcomeKey {
	return domain.CallOutcomeKey{TenantID: run.key.TenantID, RequestID: run.key.RequestID, ExecutionStepID: run.key.ExecutionStepID, CallID: call.CallID}
}

func (executor *ToolLoopExecutor) traits(toolID string) domain.ToolOutcomeTraits {
	if executor.Reconciler == nil {
		return domain.ToolOutcomeTraits{}
	}
	return executor.Reconciler.Traits(toolID)
}

// settleSent classifies the gateway's answer to a sent call. A definite answer settles its record
// with its observation; a call refused before it left keeps no record when the turn stops or parks
// and fails otherwise; anything else leaves the record unknown and holds the turn.
func (executor *ToolLoopExecutor) settleSent(ctx context.Context, run *loopRun, sent outgoingCall, result domain.ToolResult, err error) (domain.ModelToolObservation, error) {
	owner := run.input.Owner
	switch {
	case err != nil && sent.resend && (errors.Is(err, domain.ErrApprovalPending) || errors.Is(err, domain.ErrNotSent)):
		// This attempt never left, but the dispatch it repeats may have landed.
		return domain.ModelToolObservation{}, executor.unknown(ctx, run, sent, err)
	case err == nil && result.Acceptance != nil:
		return executor.accepted(ctx, run, sent, result)
	case err == nil:
		return executor.settle(ctx, run, sent, domain.CallCompleted, domain.ResolvedByGateway, resultRecord(result))
	case errors.Is(err, domain.ErrApprovalPending):
		// Parked before it left: the call is received, not dispatched, and runs once decided.
		retractErr := executor.Outcomes.Retract(context.WithoutCancel(ctx), sent.key, owner.Fence)
		if !sent.wasPending {
			pending := domain.LoopEntry{Kind: domain.LoopEntryApprovalPending, Iteration: sent.iteration, Tool: sent.reference,
				Observation: &domain.ModelToolObservation{CallID: sent.call.CallID, Name: sent.call.Name}}
			if _, appendErr := executor.append(ctx, run, pending); appendErr != nil {
				return domain.ModelToolObservation{}, errors.Join(err, retractErr, appendErr)
			}
		}
		return domain.ModelToolObservation{}, errors.Join(err, retractErr)
	case errors.Is(err, domain.ErrOutputWithheld):
		// It took effect; the model sees only the failure class.
		return executor.settle(ctx, run, sent, domain.CallCompleted, domain.ResolvedByGateway, failedResult(err, result))
	case errors.Is(err, domain.ErrNotSent):
		if endsLoop(ctx, err) {
			retractErr := executor.Outcomes.Retract(context.WithoutCancel(ctx), sent.key, owner.Fence)
			return domain.ModelToolObservation{}, errors.Join(stage.At(domain.StageTool, err), retractErr)
		}
		return executor.settle(ctx, run, sent, domain.CallFailed, domain.ResolvedByGateway, failedResult(err, result))
	case !errors.Is(err, domain.ErrEffectUnknown) && (errors.Is(err, domain.ErrNoEffect) || errors.Is(err, domain.ErrEffectViolated)):
		observation, settleErr := executor.settle(ctx, run, sent, domain.CallFailed, domain.ResolvedByGateway, failedResult(err, result))
		if settleErr == nil && endsLoop(ctx, err) {
			return domain.ModelToolObservation{}, stage.At(domain.StageTool, err)
		}
		return observation, settleErr
	}
	return domain.ModelToolObservation{}, executor.unknown(ctx, run, sent, err)
}

// failedResult is the record of a definitely failed or withheld call: the failure class, with the
// usage, evidence links, and effect the gateway reported.
func failedResult(cause error, result domain.ToolResult) domain.CallRecord {
	record := failedRecord(cause, result.Usage)
	record.ResourceURIs, record.Effect = resourceURIs(result.Evidence), result.Effect
	return record
}

// unknown records the call's outcome unknown and holds the turn; a moved fence leaves the record
// in flight under a stale dispatch fence, which reads unknown as well.
func (executor *ToolLoopExecutor) unknown(ctx context.Context, run *loopRun, sent outgoingCall, cause error) error {
	if err := executor.Outcomes.MarkUnknown(context.WithoutCancel(ctx), run.input.Owner, sent.key); err != nil {
		cause = errors.Join(cause, err)
	}
	return stage.At(domain.StageTool, fmt.Errorf("%w: call %q: %w", domain.ErrOutcomeUnknown, sent.call.CallID, cause))
}

// accepted records an asynchronous acceptance and looks the call up once; until the lookup is
// definite the record stays accepted and the turn holds. Only a tool whose composition declares
// lookup support may accept, else the acceptance is invalid output of a sent call: unknown.
func (executor *ToolLoopExecutor) accepted(ctx context.Context, run *loopRun, sent outgoingCall, result domain.ToolResult) (domain.ModelToolObservation, error) {
	acceptance := *result.Acceptance
	if !executor.traits(sent.reference.ToolID).Lookup {
		return domain.ModelToolObservation{}, executor.unknown(ctx, run, sent,
			fmt.Errorf("%w: tool %q accepted a call but declares no lookup", domain.ErrInvalidModelOutput, sent.reference.ToolID))
	}
	if err := acceptance.Validate(); err != nil {
		return domain.ModelToolObservation{}, executor.unknown(ctx, run, sent, err)
	}
	record := domain.CallRecord{Usage: result.Usage, Acceptance: &acceptance}
	if _, err := executor.Outcomes.Accept(context.WithoutCancel(ctx), run.input.Owner, sent.key, record, acceptance.LookupWithin+executor.Dispatch.Grace); err != nil {
		return domain.ModelToolObservation{}, errors.Join(stage.At(domain.StageCheckpoint, err), executor.unknown(ctx, run, sent, err))
	}
	return executor.reconcile(ctx, run, sent, domain.CallOutcome{Key: sent.key, Status: domain.CallAccepted})
}

// resume decides a recorded call met unobserved at the cursor: a settled record is journaled
// as it stands; an open one is resent only under a declared deduplication, else reconciled or
// held. handled is false only for a resend, which the caller sends under the same key.
func (executor *ToolLoopExecutor) resume(ctx context.Context, run *loopRun, sent outgoingCall, recorded domain.CallOutcome) (domain.ModelToolObservation, bool, error) {
	if recorded.Status.Definite() {
		observation, err := executor.journalRecorded(ctx, run, sent, recorded)
		return observation, true, err
	}
	if recorded.IdempotencyKey != sent.proposal.IdempotencyKey {
		return domain.ModelToolObservation{}, true, fmt.Errorf("%w: call %q was recorded under another idempotency key", domain.ErrConflict, sent.call.CallID)
	}
	if executor.traits(sent.reference.ToolID).DedupesByKey {
		if _, err := executor.Outcomes.Redispatch(context.WithoutCancel(ctx), run.input.Owner, sent.key, run.windows[sent.call.Name]); err != nil {
			return domain.ModelToolObservation{}, true, stage.At(domain.StageCheckpoint, err)
		}
		return domain.ModelToolObservation{}, false, nil
	}
	observation, err := executor.reconcile(ctx, run, sent, recorded)
	return observation, true, err
}

// reconcile asks the reconciliation sources about an open call and settles it when they answer
// definitely; otherwise an in-flight record, and an accepted one past its deadline, become unknown.
func (executor *ToolLoopExecutor) reconcile(ctx context.Context, run *loopRun, sent outgoingCall, recorded domain.CallOutcome) (domain.ModelToolObservation, error) {
	if executor.Reconciler != nil {
		report, err := executor.Reconciler.Reconcile(ctx, sent.proposal)
		if err != nil && endsLoop(ctx, err) {
			return domain.ModelToolObservation{}, stage.At(domain.StageTool, err)
		}
		if err == nil {
			if settlement, definite := reportSettlement(recorded, report); definite {
				return executor.settle(ctx, run, sent, settlement.Status, settlement.Resolution, settlement.Record)
			}
		}
	}
	if recorded.Status == domain.CallAccepted && !recorded.DeadlinePassed {
		return domain.ModelToolObservation{}, stage.At(domain.StageTool,
			fmt.Errorf("%w: call %q is accepted and awaits its lookup", domain.ErrOutcomeUnknown, sent.call.CallID))
	}
	return domain.ModelToolObservation{}, executor.unknown(ctx, run, sent, fmt.Errorf("no source establishes the outcome of call %q", sent.call.CallID))
}

// settle records the call's definite outcome and appends its observation in one transaction. A
// fenced-out worker reports a gateway answer late against its own dispatch instead: evidence, not progress.
func (executor *ToolLoopExecutor) settle(ctx context.Context, run *loopRun, sent outgoingCall, status domain.CallOutcomeStatus, resolution domain.CallResolution, record domain.CallRecord) (domain.ModelToolObservation, error) {
	entry, err := run.observationEntry(executor, sent, record, nil)
	if err != nil {
		return domain.ModelToolObservation{}, err
	}
	settlement := domain.CallSettlement{Key: sent.key, Status: status, Resolution: resolution, Record: record}
	stored, err := executor.Outcomes.SettleAndAppend(context.WithoutCancel(ctx), run.input.Owner, settlement, run.key, entry)
	if err != nil {
		if errors.Is(err, domain.ErrFenceLost) && resolution == domain.ResolvedByGateway {
			settlement.Resolution = domain.ResolvedByWorker
			if reportErr := executor.Outcomes.Report(context.WithoutCancel(ctx), settlement, run.input.Owner.Fence); reportErr != nil && !errors.Is(reportErr, domain.ErrConflict) {
				err = errors.Join(err, reportErr)
			}
		}
		return domain.ModelToolObservation{}, errors.Join(stage.At(domain.StageCheckpoint, fmt.Errorf("settle call %q: %w", sent.call.CallID, err)), run.account(entry.Usage))
	}
	return run.commitObservation(sent, entry, stored)
}

// journalRecorded appends the observation of a call whose record settled elsewhere: by a late
// report, a lookup, an attestation, or a reconciled confirmation.
func (executor *ToolLoopExecutor) journalRecorded(ctx context.Context, run *loopRun, sent outgoingCall, recorded domain.CallOutcome) (domain.ModelToolObservation, error) {
	record := domain.CallRecord{}
	if recorded.Result.URI != "" {
		var err error
		if record, err = executor.Outcomes.Record(ctx, recorded.Result); err != nil {
			return domain.ModelToolObservation{}, stage.At(domain.StageCheckpoint, err)
		}
		record.Acceptance = nil
	} else {
		record = settledWithoutResult(recorded.Status)
	}
	outcome := &domain.LoopOutcome{Status: recorded.Status, Resolution: recorded.Resolution, Resolver: recorded.Resolver, Evidence: recorded.Evidence}
	entry, err := run.observationEntry(executor, sent, record, outcome)
	if err != nil {
		return domain.ModelToolObservation{}, err
	}
	stored, err := executor.Journal.Append(context.WithoutCancel(ctx), run.input.Owner, run.key, entry)
	if err != nil {
		return domain.ModelToolObservation{}, errors.Join(stage.At(domain.StageCheckpoint, fmt.Errorf("append loop entry %d: %w", entry.EntryNo, err)), run.account(entry.Usage))
	}
	return run.commitObservation(sent, entry, stored)
}

// settledWithoutResult is what the model sees of a call a person settled without a tool result.
func settledWithoutResult(status domain.CallOutcomeStatus) domain.CallRecord {
	if status == domain.CallCompleted {
		output, _ := json.Marshal(map[string]string{"status": "completed"})
		return domain.CallRecord{Output: output}
	}
	return failedRecord(domain.ErrNoEffect, domain.Usage{})
}

// observationEntry is the next journal entry, the observation of the call at the cursor.
func (run *loopRun) observationEntry(executor *ToolLoopExecutor, sent outgoingCall, record domain.CallRecord, outcome *domain.LoopOutcome) (domain.LoopEntry, error) {
	if run.cursor != len(run.journal) {
		return domain.LoopEntry{}, fmt.Errorf("%w: loop journal diverged at entry %d", domain.ErrConflict, run.cursor+1)
	}
	usage := record.Usage
	usage.ToolCalls = max(usage.ToolCalls, 1)
	return domain.LoopEntry{
		EntryNo: len(run.journal) + 1, Kind: domain.LoopEntryObservation, Iteration: sent.iteration, Tool: sent.reference,
		Observation: &domain.ModelToolObservation{CallID: sent.call.CallID, Name: sent.call.Name, Output: record.Output, IsError: record.IsError},
		Usage:       usage, ResourceURIs: record.ResourceURIs, Effect: record.Effect, Outcome: outcome,
		Offset: executor.now().Sub(run.started),
	}, nil
}

// commitObservation advances the cursor past a stored observation, as append does.
func (run *loopRun) commitObservation(sent outgoingCall, offered, stored domain.LoopEntry) (domain.ModelToolObservation, error) {
	if !sameLoopEntry(stored, offered) {
		return domain.ModelToolObservation{}, fmt.Errorf("%w: another worker journaled different content at entry %d", domain.ErrConflict, offered.EntryNo)
	}
	run.journal = append(run.journal, stored)
	run.cursor++
	run.recordCall(sent.call, sent.reference, stored, sent.wasPending)
	return *stored.Observation, run.account(stored.Usage)
}
