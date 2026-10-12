package toolgateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

var _ contract.ToolOutcomeReconciler = (*GovernedGateway)(nil)

// Traits reports what the composition declares about a tool's outcomes.
func (gateway *GovernedGateway) Traits(toolID string) domain.ToolOutcomeTraits {
	return gateway.Outcomes[toolID]
}

// Reconcile settles a dispatched call whose result was never observed, without sending it: a
// VerifyEffect tool's effect is observed first, then the composed lookup is asked when the
// composition declares the tool's lookup support. A completed answer passes the output
// validation and guardrails a sent call's does; anything short of a definite answer is unknown.
func (gateway *GovernedGateway) Reconcile(ctx context.Context, call domain.ToolCall) (domain.CallReport, error) {
	if err := gateway.validate(call); err != nil {
		return domain.CallReport{}, err
	}
	definition, err := gateway.Registry.Get(ctx, call.TenantContext.TenantID, call.ToolID, call.ToolVersion)
	if err != nil {
		return domain.CallReport{}, err
	}
	if definition.ToolID != call.ToolID || definition.Version != call.ToolVersion || strings.TrimSpace(definition.Endpoint) == "" {
		return domain.CallReport{}, fmt.Errorf("%w: registered tool definition is inconsistent", domain.ErrConflict)
	}
	unknown := domain.CallReport{Status: domain.CallUnknown}
	if definition.VerifyEffect && gateway.Effects != nil {
		observation, _ := gateway.observe(ctx, call, definition, nil)
		switch observation.Status {
		case domain.EffectSatisfied:
			observation.Reconciled = true
			return gateway.completed(ctx, call, definition, domain.ToolResult{Output: observation.Output, Effect: &observation}, domain.ResolvedByEffect)
		case domain.EffectViolated:
			return domain.CallReport{Status: domain.CallFailed, Resolution: domain.ResolvedByEffect, Reason: observation.Reason}, nil
		}
		unknown.Reason = observation.Reason
	}
	if !gateway.Traits(call.ToolID).Lookup || gateway.Lookup == nil {
		return unknown, nil
	}
	report, err := gateway.Lookup.Lookup(ctx, call, definition)
	if err != nil {
		return domain.CallReport{}, fmt.Errorf("look up call of tool %q: %w", call.ToolID, err)
	}
	switch {
	case report.Status == domain.CallCompleted && report.Result.Acceptance == nil:
		return gateway.completed(ctx, call, definition, report.Result, domain.ResolvedByLookup)
	case report.Status == domain.CallFailed:
		return domain.CallReport{Status: domain.CallFailed, Resolution: domain.ResolvedByLookup, Result: domain.ToolResult{Usage: report.Result.Usage}, Reason: report.Reason}, nil
	}
	unknown.Reason = report.Reason
	return unknown, nil
}

// completed validates and guards a reconciled result; invalid output leaves the call unknown, and
// output a guardrail blocks completes the call with its output withheld.
func (gateway *GovernedGateway) completed(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, result domain.ToolResult, source domain.CallResolution) (domain.CallReport, error) {
	if err := gateway.Validator.Validate(ctx, definition, result); err != nil {
		return domain.CallReport{Status: domain.CallUnknown, Reason: "the reconciled output does not satisfy the tool's output schema"}, nil
	}
	report := domain.CallReport{Status: domain.CallCompleted, Resolution: source, Result: result}
	if gateway.Guardrails == nil {
		return report, nil
	}
	guardrails, err := gateway.guardrailConfig(ctx, call)
	if err != nil {
		return domain.CallReport{}, err
	}
	subject := domain.GuardrailSubject{
		TenantID: call.TenantContext.TenantID, Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID},
		RequestID: call.RequestID, ConversationID: call.ConversationID, ReleaseVersion: call.Principal.Release,
	}
	guarded, err := gateway.Guardrails.AfterTool(ctx, guardrails, subject, result)
	if err != nil {
		report.Result, report.OutputWithheld, report.Reason = domain.ToolResult{Usage: result.Usage}, true, stage.ErrorClass(err)
		return report, nil
	}
	report.Result = guarded
	return report, nil
}
