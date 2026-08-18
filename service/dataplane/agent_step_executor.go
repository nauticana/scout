package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/principal"
)

// AgentStepKind is the graph step kind for a delegated agent invocation.
const AgentStepKind = "agent"

// agentStepConfig is the compiled configuration of one agent step.
type agentStepConfig struct {
	Target domain.PrincipalRef `json:"target"`
	Action string              `json:"action"`
	// TaskKind labels the work item created for the delegation.
	TaskKind string `json:"task_kind,omitempty"`
}

// AgentStepExecutor admits one agent-to-agent delegation: it verifies the grant,
// narrows the bounds, refuses a cycle, records the work item, and hands the call
// to an injected invoker. Scout owns the authority; the transport does not
// belong here, so an in-process runtime and an A2A client compose identically.
type AgentStepExecutor struct {
	Delegation contract.DelegationAuthorizer
	Work       contract.WorkItemStore
	Invoker    contract.AgentInvoker
	Objects    ObjectStateCodec
	// Audit is optional; when set every admitted or refused delegation is evidence.
	Audit contract.AuditSink
}

// Execute admits and performs one delegated call.
func (e *AgentStepExecutor) Execute(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
	if e.Delegation == nil || e.Work == nil || e.Invoker == nil || e.Objects == nil {
		return domain.StepResult{}, fmt.Errorf("agent step: delegation, work items, invoker, and object codec are required")
	}
	config, err := decodeAgentStep(input.Step)
	if err != nil {
		return domain.StepResult{}, err
	}
	caller := input.Principal
	if caller.Kind == "" || strings.TrimSpace(caller.ID) == "" {
		return domain.StepResult{}, fmt.Errorf("%w: an agent step requires the calling principal", domain.ErrPrincipalUnknown)
	}

	authorization, err := e.Delegation.Authorize(ctx, caller, config.Target, config.Action)
	if err != nil {
		e.record(ctx, caller, config, domain.DecisionDeny, err.Error())
		return domain.StepResult{}, err
	}
	next := authorization.Bounds
	if len(caller.Authority) > 0 || hasDelegationBounds(input.Bounds) {
		next, err = principal.Narrow(input.Bounds, authorization.Bounds)
		if err != nil {
			e.record(ctx, caller, config, domain.DecisionDeny, err.Error())
			return domain.StepResult{}, err
		}
	}
	if err := e.checkCycle(ctx, caller, config.Target, input); err != nil {
		e.record(ctx, caller, config, domain.DecisionDeny, err.Error())
		return domain.StepResult{}, err
	}

	ref, err := e.Objects.Dehydrate(ctx, delegationInputName(input), input.Snapshot.State)
	if err != nil {
		return domain.StepResult{}, fmt.Errorf("dehydrate delegated input: %w", err)
	}
	item, err := e.Work.Assign(ctx, domain.WorkItem{
		TenantID: caller.TenantID, Assignee: config.Target,
		Requester: domain.PrincipalRef{Kind: caller.Kind, ID: caller.ID},
		GrantID:   authorization.GrantID, ParentID: input.WorkItemID, Depth: input.WorkItemDepth + 1,
		ScopeID: caller.ScopeID, TaskKind: taskKindOr(config), Input: ref,
		RequestID:        delegationRequestID(input, config),
		BudgetMinorUnits: next.BudgetMinorUnits, Currency: next.Currency,
	})
	if err != nil {
		return domain.StepResult{}, err
	}
	e.record(ctx, caller, config, domain.DecisionAllow, "delegated")

	result, err := e.Invoker.Invoke(ctx, domain.DelegatedCall{
		Caller: caller, Target: config.Target, Authority: authorization.Authority, Bounds: next, WorkItem: item,
		Input: input.Snapshot.State, RequestID: item.RequestID,
	})
	if err != nil {
		if completeErr := e.Work.Complete(ctx, caller.TenantID, item.ID, "failed"); completeErr != nil {
			return domain.StepResult{}, fmt.Errorf("%w (work item not closed: %v)", err, completeErr)
		}
		return domain.StepResult{}, stage.At(domain.StageTool, err)
	}
	if err := e.Work.Complete(ctx, caller.TenantID, item.ID, "completed"); err != nil {
		return domain.StepResult{}, err
	}
	return result, nil
}

func hasDelegationBounds(bounds domain.DelegationBounds) bool {
	return bounds.RemainingDepth != 0 || bounds.BudgetMinorUnits != 0 || bounds.Currency != "" ||
		bounds.ScopeID != "" || bounds.ApprovalRequired
}

// checkCycle refuses a delegation whose target already appears in the chain that
// led here. Without it a mutual delegation would consume budget until a ceiling
// stopped it, long after the loop became obvious.
func (e *AgentStepExecutor) checkCycle(ctx context.Context, caller domain.Principal, target domain.PrincipalRef, input domain.StepInput) error {
	if target.Kind == caller.Kind && target.ID == caller.ID {
		return fmt.Errorf("%w: agent %q cannot delegate to itself", domain.ErrLoopDetected, caller.ID)
	}
	parent := parentWorkItem(input)
	if parent <= 0 {
		return nil
	}
	ancestors, err := e.Work.Ancestors(ctx, caller.TenantID, parent)
	if err != nil {
		return err
	}
	for _, ancestor := range ancestors {
		if ancestor.Assignee == target {
			return fmt.Errorf("%w: %q already appears in this delegation chain", domain.ErrLoopDetected, target.ID)
		}
	}
	return nil
}

func (e *AgentStepExecutor) record(ctx context.Context, caller domain.Principal, config agentStepConfig, outcome domain.DecisionOutcome, reason string) {
	if e.Audit == nil {
		return
	}
	// Evidence must not mask the delegation outcome the caller is about to see.
	_ = e.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: caller.TenantID, Principal: domain.PrincipalRef{Kind: caller.Kind, ID: caller.ID},
		ScopeID: caller.ScopeID, Category: domain.DecisionCategoryToolInvoke, Action: config.Action,
		Resource: config.Target.ID, ReleaseVersion: caller.Release, Outcome: outcome, Reason: reason,
	})
}

func decodeAgentStep(step domain.ExecutionStep) (agentStepConfig, error) {
	var config agentStepConfig
	if err := json.Unmarshal(step.Configuration, &config); err != nil {
		return config, fmt.Errorf("%w: agent step %q has invalid configuration: %v", domain.ErrValidation, step.StepID, err)
	}
	if config.Target.Kind == "" || strings.TrimSpace(config.Target.ID) == "" || strings.TrimSpace(config.Action) == "" {
		return config, fmt.Errorf("%w: agent step %q needs a target principal and an action", domain.ErrValidation, step.StepID)
	}
	return config, nil
}

func parentWorkItem(input domain.StepInput) int64 { return input.WorkItemID }

func taskKindOr(config agentStepConfig) string {
	if strings.TrimSpace(config.TaskKind) != "" {
		return config.TaskKind
	}
	return config.Action
}

// delegationRequestID is deterministic so a replayed step re-attaches to the
// work item it already created instead of delegating twice.
func delegationRequestID(input domain.StepInput, config agentStepConfig) string {
	return fmt.Sprintf("%s:%d:%s", input.RequestID, input.Step.ExecutionStepID, config.Target.ID)
}

func delegationInputName(input domain.StepInput) string {
	return fmt.Sprintf("delegated-input/%s/%d", input.RequestID, input.Step.ExecutionStepID)
}

var _ contract.StepExecutor = (*AgentStepExecutor)(nil)
