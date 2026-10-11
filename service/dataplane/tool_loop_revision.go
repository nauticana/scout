package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/nauticana/scout/domain"
)

// defaultCallRevisions bounds how often deciders may revise one proposed call in a turn
// when ToolLoopLimits.MaxCallRevisions is zero.
const defaultCallRevisions = 8

// revision returns the edit a decider made to a parked call: the journaled substitution on
// replay, or, at the head of the journal, a newly read edit that is journaled before it runs.
func (executor *ToolLoopExecutor) revision(ctx context.Context, run *loopRun, iteration int, tool domain.ToolReference,
	call domain.ModelToolCall, proposal domain.ToolCall) (domain.CallRevision, bool, error) {
	if entry, replayed := run.next(domain.LoopEntryRevision); replayed {
		if entry.Observation == nil || entry.Observation.CallID != call.CallID || entry.Revision == nil {
			return domain.CallRevision{}, false, fmt.Errorf("%w: loop journal entry %d does not revise call %q", domain.ErrConflict, entry.EntryNo, call.CallID)
		}
		return *entry.Revision, true, validRevision(*entry.Revision)
	}
	if run.cursor != len(run.journal) || executor.Revisions == nil {
		return domain.CallRevision{}, false, nil
	}
	revision, revised, err := executor.Revisions.Revision(ctx, proposal)
	if err != nil {
		return domain.CallRevision{}, false, fmt.Errorf("read revision of call %q: %w", call.CallID, err)
	}
	if !revised {
		return domain.CallRevision{}, false, nil
	}
	if err := validRevision(revision); err != nil {
		return domain.CallRevision{}, false, err
	}
	entry, err := executor.append(ctx, run, domain.LoopEntry{
		Kind: domain.LoopEntryRevision, Iteration: iteration, Tool: tool,
		Observation: &domain.ModelToolObservation{CallID: call.CallID, Name: call.Name}, Revision: &revision,
	})
	if err != nil {
		return domain.CallRevision{}, false, err
	}
	return *entry.Revision, true, nil
}

func validRevision(revision domain.CallRevision) error {
	if revision.RevisesProposal <= 0 || len(revision.Arguments) == 0 || DigestBytes(revision.Arguments) != revision.ArgumentsDigest {
		return fmt.Errorf("%w: a call revision needs its proposal and arguments matching their digest", domain.ErrConflict)
	}
	return nil
}

// revisedCall is the call a revision runs: the revised arguments under a key of their own,
// so neither its approval nor its effect can be mistaken for the original's.
func revisedCall(proposal domain.ToolCall, revision domain.CallRevision) domain.ToolCall {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%d\x1f%s", proposal.IdempotencyKey, revision.RevisesProposal, revision.ArgumentsDigest)))
	proposal.Arguments = revision.Arguments
	proposal.IdempotencyKey = "revision:" + hex.EncodeToString(sum[:])
	proposal.RevisesProposal = revision.RevisesProposal
	return proposal
}

// recordRevision emits that a decider edited the parked call before it ran.
func (run *loopRun) recordRevision(call domain.ModelToolCall, tool domain.ToolReference) {
	run.events = append(run.events, domain.TurnEvent{
		Version: domain.TurnEventVersion, Kind: domain.TurnEventApprovalResolved,
		Approval: &domain.TurnApprovalEvent{CallID: call.CallID, ToolID: tool.ToolID, Decision: string(domain.ApprovalEdited)},
	})
}
