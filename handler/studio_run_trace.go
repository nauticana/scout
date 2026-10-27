package handler

import (
	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

func runTraceItem(trace domain.RunTrace) api.RunTrace {
	item := api.RunTrace{Run: agentRunItem(trace.Run), Events: make([]api.RunTraceEvent, 0, len(trace.Events)), Truncated: trace.Truncated}
	if turn := trace.Turn; turn != nil {
		item.Turn = &api.RunTraceTurn{
			ConversationID: turn.ConversationID, TurnNo: turn.TurnNo, Status: string(turn.Status), QueuedAt: turn.QueuedAt,
			StartedAt: optionalTime(turn.StartedAt), FirstResponseAt: optionalTime(turn.FirstResponseAt), CompletedAt: optionalTime(turn.CompletedAt),
		}
	}
	for _, event := range trace.Events {
		item.Events = append(item.Events, runTraceEventItem(event))
	}
	return item
}

func runTraceEventItem(event domain.RunTraceEvent) api.RunTraceEvent {
	item := api.RunTraceEvent{Sequence: event.Sequence, Kind: string(event.Kind), At: event.At}
	switch {
	case event.Model != nil:
		step := event.Model
		item.ModelStep = &api.RunTraceModelStep{Journal: journalItem(step.Journal), Usage: trajectoryUsage(step.Usage),
			Finish: string(step.Finish), ToolCalls: step.ToolCalls}
		for _, model := range step.Models {
			item.ModelStep.Models = append(item.ModelStep.Models, api.RunTraceModel{Provider: model.ProviderID, Model: model.ModelID})
		}
	case event.Tool != nil:
		call := event.Tool
		item.ToolCall = &api.RunTraceToolCall{Journal: journalItem(call.Journal), CallID: call.CallID, ToolID: call.Tool.ToolID, ToolVersion: call.Tool.Version,
			Outcome: string(call.Outcome), ErrorClass: call.ErrorClass, Effect: string(call.Effect), Reconciled: call.Reconciled,
			Usage: trajectoryUsage(call.Usage)}
		if settled := call.Settlement; settled != nil {
			item.ToolCall.Settlement = &api.RunTraceCallSettlement{Status: string(settled.Status), Resolution: string(settled.Resolution),
				ResolverKind: string(settled.Resolver.Kind), ResolverID: settled.Resolver.ID, ResolvedAt: optionalTime(settled.ResolvedAt),
				EvidenceDigest: settled.EvidenceDigest}
		}
	case event.Guardrail != nil:
		hit := event.Guardrail
		item.Guardrail = &api.RunTraceGuardrailHit{Stage: string(hit.Stage), Layer: string(hit.Layer), Action: string(hit.Action),
			Severity: string(hit.Severity), RuleIDs: append([]string{}, hit.RuleIDs...), ReleaseVersion: hit.ReleaseVersion,
			PolicyVersion: hit.PolicyVersion, DurationMs: hit.Duration.Milliseconds()}
	case event.Approval != nil:
		approval := event.Approval
		item.Approval = &api.RunTraceApproval{ID: approval.ID, ExecutionStepID: approval.ExecutionStepID, RequestedAction: approval.RequestedAction,
			Resource: approval.Resource, RuleSource: approval.RuleSource, RuleID: approval.RuleID, RiskTier: string(approval.RiskTier),
			Status: string(approval.Status), ProposedDigest: approval.ProposedDigest, RevisesStepID: approval.RevisesStepID,
			ResolvedAt: optionalTime(approval.ResolvedAt), DeciderKind: string(approval.DeciderKind), DecidedAt: optionalTime(approval.DecidedAt)}
	case event.Decision != nil:
		item.Decision = runTraceDecisionItem(*event.Decision)
	}
	return item
}

func runTraceDecisionItem(decision domain.RunTraceDecisionEvent) *api.RunTraceDecision {
	item := &api.RunTraceDecision{Source: string(decision.Source), Category: decision.Category, Action: decision.Action,
		Resource: decision.Resource, Outcome: string(decision.Outcome), PolicyID: decision.PolicyID, PolicyVersion: decision.PolicyVersion,
		ReleaseVersion: decision.ReleaseVersion, PrincipalKind: string(decision.Authority.Subject.Kind), PrincipalID: decision.Authority.Subject.ID,
		GrantID: decision.Authority.GrantID, GrantorKind: string(decision.Authority.Grantor.Kind), GrantorID: decision.Authority.Grantor.ID,
		EvidenceDigest: decision.EvidenceDigest}
	for _, obligation := range decision.Obligations {
		item.Obligations = append(item.Obligations, string(obligation))
	}
	if action := decision.Charter; action != nil {
		item.Charter = &api.RunTraceAction{ActionID: traceRef(action.ActionID), Capability: traceRef(action.Capability),
			OperationClass: action.OperationClass, Disposition: action.Disposition, Outcome: action.Outcome,
			Assignment: optionalTraceRef(action.Assignment), Authority: traceEvaluations(action.Authority),
			Approvals: traceEvaluations(action.Approvals), Reconciles: optionalTraceRef(action.Reconciles),
			SodException: optionalTraceRef(action.SodException), MaterialInputsDigest: action.MaterialInputsDigest,
			Preconditions: traceConditions(action.Preconditions), Postconditions: traceConditions(action.Postconditions),
			Effect: action.Effect, UnknownCause: action.UnknownCause}
	}
	return item
}

func traceConditions(conditions []domain.CharterCondition) []api.RunTraceCondition {
	var items []api.RunTraceCondition
	for _, condition := range conditions {
		items = append(items, api.RunTraceCondition{ID: condition.ID, Result: condition.Result, ObservedAt: optionalTime(condition.ObservedAt)})
	}
	return items
}

func journalItem(ref domain.RunTraceJournalRef) api.RunTraceJournal {
	return api.RunTraceJournal{ExecutionStepID: ref.ExecutionStepID, EntryNo: ref.EntryNo, Iteration: ref.Iteration, Digest: ref.Digest}
}

func traceEvaluations(evaluations []domain.CharterEvaluation) []api.RunTraceEvaluation {
	var items []api.RunTraceEvaluation
	for _, evaluation := range evaluations {
		items = append(items, api.RunTraceEvaluation{Ref: optionalTraceRef(evaluation.Ref), Result: evaluation.Result})
	}
	return items
}

func traceRef(ref domain.CharterRef) api.RunTraceRef {
	return api.RunTraceRef{Namespace: ref.Namespace, ID: ref.ID}
}

func optionalTraceRef(ref *domain.CharterRef) *api.RunTraceRef {
	if ref == nil {
		return nil
	}
	item := traceRef(*ref)
	return &item
}

// trajectoryUsage is the spend of one event, nil when it spent nothing.
func trajectoryUsage(usage domain.Usage) *api.TrajectoryUsage {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.ToolCalls == 0 && usage.SearchQueries == 0 && usage.CostMinorUnits == 0 {
		return nil
	}
	return &api.TrajectoryUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, ToolCalls: usage.ToolCalls,
		SearchQueries: usage.SearchQueries, CostMinorUnits: usage.CostMinorUnits, Currency: usage.Currency}
}
