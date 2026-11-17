package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/observability"
)

// LookupOutcome asks the reconciliation sources about one open call of a turn held or ended at
// the fence the operator saw, and settles it when they answer definitely. An effect proven
// absent before the call's deadline is not definite: the call may still be in flight.
func (s *StudioTurnControl) LookupOutcome(ctx context.Context, actor domain.StudioActor, request domain.TurnOutcomeRequest) (domain.CallOutcome, error) {
	if _, err := s.ready(ctx); err != nil {
		return domain.CallOutcome{}, err
	}
	if err := validOutcomeRequest(request); err != nil {
		return domain.CallOutcome{}, err
	}
	if err := s.authorize(ctx, actor, domain.OperationOperate, request.AgentID); err != nil {
		return domain.CallOutcome{}, err
	}
	outcome, err := s.unworkedOutcome(ctx, actor.TenantID, request)
	if err != nil {
		return outcome, err
	}
	if !outcome.Status.Open() {
		if outcome.Resolver == actor.Principal && (outcome.Resolution == domain.ResolvedByEffect || outcome.Resolution == domain.ResolvedByLookup) {
			err = s.auditOutcome(ctx, actor, request, observability.AuditCategoryCallOutcomeResolved, outcome, string(outcome.Resolution))
		}
		return outcome, err
	}
	resolved, _, err := s.calls().settle(ctx, outcome, request.AgentID, request.Fence, actor.Principal)
	return resolved, err
}

// AttestOutcome records the actor's attestation of one open call of a turn held or ended at the
// fence the operator saw, once the call's deadline passed. A call the separation policy marks
// for two people settles on the second agreeing person; a policy that cannot be evaluated refuses.
func (s *StudioTurnControl) AttestOutcome(ctx context.Context, actor domain.StudioActor, request domain.TurnAttestRequest) (domain.AttestationResult, error) {
	if _, err := s.ready(ctx); err != nil {
		return domain.AttestationResult{}, err
	}
	if err := validOutcomeRequest(request.TurnOutcomeRequest); err != nil {
		return domain.AttestationResult{}, err
	}
	attestation := domain.CallAttestation{
		Key:      domain.CallOutcomeKey{TenantID: actor.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, CallID: request.CallID},
		Attester: actor.Principal, Outcome: request.Outcome, Note: request.Note, Evidence: request.Evidence, Persons: 1, TurnFence: request.Fence,
	}
	if err := attestation.Validate(); err != nil {
		return domain.AttestationResult{}, err
	}
	if err := s.authorize(ctx, actor, domain.OperationReconcile, request.AgentID); err != nil {
		return domain.AttestationResult{}, err
	}
	outcome, err := s.unworkedOutcome(ctx, actor.TenantID, request.TurnOutcomeRequest)
	if err != nil {
		return domain.AttestationResult{}, err
	}
	if s.Separation == nil {
		return domain.AttestationResult{}, fmt.Errorf("%w: no operation separation policy is composed, so no outcome may be attested", domain.ErrNotReady)
	}
	at := s.now()
	second, err := s.Separation.Required(ctx, actor.TenantID, request.AgentID, outcome, at)
	if err != nil {
		return domain.AttestationResult{}, fmt.Errorf("%w: the separation of attesting call %q cannot be evaluated: %w", domain.ErrNotReady, request.CallID, err)
	}
	if err = s.Separation.Permit(ctx, actor.TenantID, request.AgentID, outcome, actor.Principal, at); err != nil {
		return domain.AttestationResult{}, fmt.Errorf("%w: %s may not attest call %q: %w", domain.ErrForbidden, actor.Principal.ID, request.CallID, err)
	}
	if second {
		attestation.Persons = 2
	}
	if attestation.Evidence.URI != "" {
		if s.Evidence == nil {
			return domain.AttestationResult{}, fmt.Errorf("%w: no evidence verifier is composed", domain.ErrNotReady)
		}
		if err = s.Evidence.VerifyObject(ctx, actor.TenantID, attestation.Evidence); err != nil {
			return domain.AttestationResult{}, fmt.Errorf("%w: attestation evidence: %w", domain.ErrValidation, err)
		}
	}
	result, err := s.Outcomes.Attest(ctx, attestation)
	if err != nil {
		return domain.AttestationResult{}, err
	}
	if result.AlreadyStated {
		if err = s.auditOutcome(ctx, actor, request.TurnOutcomeRequest, observability.AuditCategoryCallOutcomeAttested, result.Outcome, string(request.Outcome)); err != nil {
			return domain.AttestationResult{}, err
		}
		if !result.AwaitsSecond && result.Outcome.Resolution == domain.ResolvedByAttestation {
			err = s.auditOutcome(ctx, actor, request.TurnOutcomeRequest, observability.AuditCategoryCallOutcomeResolved, result.Outcome, string(domain.ResolvedByAttestation))
		}
		return result, err
	}
	if err = s.auditOutcome(ctx, actor, request.TurnOutcomeRequest, observability.AuditCategoryCallOutcomeAttested, result.Outcome, string(request.Outcome)); err != nil {
		return domain.AttestationResult{}, err
	}
	if result.AwaitsSecond {
		return result, nil
	}
	return result, s.auditOutcome(ctx, actor, request.TurnOutcomeRequest, observability.AuditCategoryCallOutcomeResolved, result.Outcome, string(domain.ResolvedByAttestation))
}

// auditOutcome records one person's act on the call a request names.
func (s *StudioTurnControl) auditOutcome(ctx context.Context, actor domain.StudioActor, request domain.TurnOutcomeRequest, category string, outcome domain.CallOutcome, reason string) error {
	key := domain.CallOutcomeKey{TenantID: actor.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, CallID: request.CallID}
	return s.calls().audit(ctx, actor.Principal, key, request.AgentID, request.Fence, category, outcome, reason)
}

func validOutcomeRequest(request domain.TurnOutcomeRequest) error {
	if request.Fence <= 0 || request.ExecutionStepID <= 0 || strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.CallID) == "" {
		return fmt.Errorf("%w: request_id, the fence, execution_step_id, and call_id are required", domain.ErrValidation)
	}
	return nil
}

// unworkedOutcome reads one call of a turn of the agent that stands held or ended at the
// request's fence; any other turn state or fence is domain.ErrConflict.
func (s *StudioTurnControl) unworkedOutcome(ctx context.Context, tenantID int64, request domain.TurnOutcomeRequest) (domain.CallOutcome, error) {
	turn, err := s.turn(ctx, tenantID, request.AgentID, request.RequestID)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	if (turn.Status != domain.TurnHeld && !turn.Status.Terminal()) || turn.Fence != request.Fence {
		return domain.CallOutcome{}, fmt.Errorf("%w: turn %q is %s at fence %d, not held or ended at %d", domain.ErrConflict, request.RequestID, turn.Status, turn.Fence, request.Fence)
	}
	key := domain.CallOutcomeKey{TenantID: tenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, CallID: request.CallID}
	outcome, found, err := s.Outcomes.Find(ctx, key)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	if !found {
		return domain.CallOutcome{}, fmt.Errorf("%w: call %q of turn %q", domain.ErrNotFound, request.CallID, request.RequestID)
	}
	return outcome, nil
}

// reportSettlement turns a reconciliation answer into the call's settlement; false when the
// answer is not definite, as an effect proven absent before the call's deadline is not.
func reportSettlement(outcome domain.CallOutcome, report domain.CallReport) (domain.CallSettlement, bool) {
	settlement := domain.CallSettlement{Key: outcome.Key, Status: report.Status, Resolution: report.Resolution}
	switch {
	case report.Resolution != domain.ResolvedByEffect && report.Resolution != domain.ResolvedByLookup:
		return domain.CallSettlement{}, false
	case report.Status == domain.CallCompleted && report.OutputWithheld:
		settlement.Record = failedRecord(domain.ErrOutputWithheld, report.Result.Usage)
	case report.Status == domain.CallCompleted:
		settlement.Record = resultRecord(report.Result)
	case report.Status == domain.CallFailed && (report.Resolution == domain.ResolvedByLookup || outcome.DeadlinePassed):
		class := domain.ErrNoEffect
		if report.Resolution == domain.ResolvedByEffect {
			class = domain.ErrEffectViolated
		}
		settlement.Record = failedRecord(class, report.Result.Usage)
	default:
		return domain.CallSettlement{}, false
	}
	return settlement, true
}

// resultRecord is the record of a completed call: its guarded output, usage, evidence, and effect.
func resultRecord(result domain.ToolResult) domain.CallRecord {
	return domain.CallRecord{Output: result.Output, Usage: result.Usage, ResourceURIs: resourceURIs(result.Evidence), Effect: result.Effect}
}

// failedRecord is the record of a failed call: the model sees the failure class, never its text.
func failedRecord(cause error, usage domain.Usage) domain.CallRecord {
	output, _ := json.Marshal(map[string]string{"error": stage.ErrorClass(cause)})
	return domain.CallRecord{Output: output, IsError: true, Usage: usage}
}
