package charter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/evidence"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

// Capability is the Charter capability a Scout tool realizes. An empty ContractVersion accepts the catalog's.
type Capability struct {
	ID              model.Ref
	ContractVersion string
}

// CapabilityGateway runs a tool mapped to a Charter capability through Charter's invoker, so authority, approval,
// separation of duties, the idempotency ledger, and an ActionRecord govern the call; the invoker's transport reaches
// Next, which stays the effect boundary. The turn's request id is the execution context id, so Charter's
// evidence.Queries.ActionsIn lists a run's actions and their reconciliations. With a Reconciler, an unknown outcome
// is reconciled once from observation: with this attempt's fence, or by reclaiming a key an earlier attempt left
// unknown when the call is redelivered. Unmapped tools go straight to Next. As the turn's
// ToolOutcomeLookup it reconciles a governed call's key from observation alone, never sending it.
type CapabilityGateway struct {
	Invoker      capability.Invoker
	Reconciler   capability.Reconciler
	Next         contract.GovernedToolGateway
	Capabilities map[string]Capability
	Invocations  Invocations
	// Actions finds the recorded attempt a lookup's reconciliation names; without it a lookup
	// answers unknown.
	Actions evidence.Queries
	// MaxActions bounds the latest actions of one execution context a lookup reads; zero takes
	// 1000, and values above 10000 take 10000.
	MaxActions int
	Now        func() time.Time
}

var (
	_ contract.GovernedToolGateway = (*CapabilityGateway)(nil)
	_ contract.ToolOutcomeLookup   = (*CapabilityGateway)(nil)
)

const (
	defaultMaxLookupActions = 1000
	maxLookupActions        = 10000
)

func (g *CapabilityGateway) Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	if g.Next == nil {
		return domain.ToolResult{}, fmt.Errorf("%w: capability gateway needs the governed gateway", domain.ErrNotReady)
	}
	target, governed := g.Capabilities[call.ToolID]
	if !governed {
		return g.Next.Invoke(ctx, call)
	}
	if g.Invoker == nil || g.Invocations == nil {
		return domain.ToolResult{}, fmt.Errorf("%w: %w: capability gateway needs an invoker and invocations", domain.ErrNotSent, domain.ErrNotReady)
	}
	inv, err := g.invocation(ctx, call, target)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("%w: %w", domain.ErrNotSent, err)
	}
	attempt := &attempt{call: call}
	ctx = context.WithValue(ctx, attemptKey{}, attempt)
	result := g.Invoker.Invoke(ctx, inv)
	report(call, result)
	if result.Status == capability.StatusUnknown && result.Action != nil && g.Reconciler != nil {
		// A reconciliation is judged when it happens, not when the attempt was made.
		settling := inv
		settling.At = g.now()
		reconciled := g.Reconciler.Reconcile(ctx, capability.Reconciliation{Invocation: settling,
			Action: model.Ref{Namespace: result.Action.Namespace, ID: result.Action.ID}, Fence: result.LedgerFence})
		report(call, reconciled)
		if reconciled.Status != capability.StatusDenied {
			result = reconciled
		}
	}
	return attempt.outcome(result)
}

// report logs a lost record or ledger write; the call's own outcome stands rather than becoming a failure.
func report(call domain.ToolCall, result capability.Result) {
	if result.Err != nil {
		log.Printf("charter %s for %s in request %s: %v", result.Status, call.ToolID, call.RequestID, result.Err)
	}
}

// invocation describes the call in Charter's terms, in the turn's execution context.
func (g *CapabilityGateway) invocation(ctx context.Context, call domain.ToolCall, target Capability) (capability.Invocation, error) {
	inv, err := g.Invocations.Invocation(ctx, call, target.ID)
	if err != nil {
		return capability.Invocation{}, fmt.Errorf("%w: charter invocation of %s: %w", domain.ErrForbidden, call.ToolID, err)
	}
	runtime, err := charterkeel.RuntimeContext(context.WithValue(ctx, common.RequestID, call.RequestID), inv.Runtime.RuntimeInstanceID)
	if err != nil {
		return capability.Invocation{}, fmt.Errorf("%w: request %q cannot be a charter execution context: %w", domain.ErrValidation, call.RequestID, err)
	}
	inv.Runtime, inv.CapabilityID, inv.ContractVersion = runtime, target.ID, target.ContractVersion
	inv.Inputs, inv.IdempotencyKey = json.RawMessage(call.Arguments), call.IdempotencyKey
	if inv.At.IsZero() {
		inv.At = g.now()
	}
	return inv, nil
}

// Lookup reconciles a governed call's idempotency key from observed effects: completed when
// Charter settles it executed or with a declared business error, failed when it settles failed,
// and unknown otherwise, including for a call no recorded attempt names.
func (g *CapabilityGateway) Lookup(ctx context.Context, call domain.ToolCall, _ domain.ToolDefinition) (domain.CallReport, error) {
	unknown := domain.CallReport{Status: domain.CallUnknown}
	target, governed := g.Capabilities[call.ToolID]
	if !governed || g.Reconciler == nil || g.Invocations == nil || g.Actions.Provider == nil {
		return unknown, nil
	}
	inv, err := g.invocation(ctx, call, target)
	if err != nil {
		return domain.CallReport{}, err
	}
	limit := g.MaxActions
	if limit <= 0 {
		limit = defaultMaxLookupActions
	}
	limit = min(limit, maxLookupActions)
	actions, err := g.Actions.ActionsIn(ctx, inv.Namespace, inv.Runtime.ExecutionContextID, limit)
	if err != nil {
		return domain.CallReport{}, fmt.Errorf("read charter actions of request %q: %w", call.RequestID, err)
	}
	action, found := attemptToReconcile(actions, target.ID, inv.MaterialInputsDigest)
	if !found {
		unknown.Reason = "no attempt of the call is recorded"
		return unknown, nil
	}
	inv.At = g.now()
	result := g.Reconciler.Reconcile(ctx, capability.Reconciliation{Invocation: inv, Action: action})
	report(call, result)
	switch result.Status {
	case capability.StatusExecuted, capability.StatusBusinessError:
		output, err := (&attempt{call: call}).recordedOutcome(result)
		if err != nil {
			return domain.CallReport{}, err
		}
		output.Usage.ToolCalls = max(output.Usage.ToolCalls, 1)
		return domain.CallReport{Status: domain.CallCompleted, Resolution: domain.ResolvedByLookup, Result: output, Reason: result.Reason}, nil
	case capability.StatusFailed:
		return domain.CallReport{Status: domain.CallFailed, Resolution: domain.ResolvedByLookup, Reason: result.Reason}, nil
	}
	unknown.Reason = result.Reason
	return unknown, nil
}

// attemptToReconcile names the record a reconciliation settles: the latest attempt of the
// capability left unknown and not yet reconciled, else its latest record, whose key the ledger
// answers by; a digest narrows both to the same material inputs.
func attemptToReconcile(actions []model.ActionRecord, capabilityID model.Ref, inputsDigest string) (model.Ref, bool) {
	settled := map[string]bool{}
	for _, action := range actions {
		if action.ReconcilesActionID != nil {
			settled[action.ReconcilesActionID.ID] = true
		}
	}
	var latest *model.ActionRecord
	for index := len(actions) - 1; index >= 0; index-- {
		action := actions[index]
		if action.CapabilityID.ID != capabilityID.ID || inputsDigest != "" && action.MaterialInputsDigest != inputsDigest {
			continue
		}
		if action.Disposition == string(capability.StatusUnknown) && !settled[action.ID] {
			return model.Ref{Namespace: action.Namespace, ID: action.ID}, true
		}
		if latest == nil {
			latest = &actions[index]
		}
	}
	if latest == nil {
		return model.Ref{}, false
	}
	return model.Ref{Namespace: latest.Namespace, ID: latest.ID}, true
}

func (g *CapabilityGateway) now() time.Time {
	return clk.Now(g.Now)
}

// attempt carries the originating tool call to the binder and the governed gateway's answer back, so the loop keeps
// the call's identity, usage, evidence, and effect rather than only what the capability contract declares.
type attempt struct {
	call    domain.ToolCall
	reached bool
	result  domain.ToolResult
	err     error
}

type attemptKey struct{}

func attemptFrom(ctx context.Context) *attempt {
	a, _ := ctx.Value(attemptKey{}).(*attempt)
	return a
}

func (a *attempt) outcome(result capability.Result) (domain.ToolResult, error) {
	switch {
	case result.Status == capability.StatusExecuted:
		if a.reached && a.err == nil {
			return a.result, nil
		}
		return a.recordedOutcome(result)
	case result.Status == capability.StatusBusinessError:
		// Charter may map a failed postcondition to a declared business error even
		// when the tool returned success, so its governed result is authoritative.
		return a.recordedOutcome(result)
	case a.reached && a.err != nil && result.Status != capability.StatusUnknown:
		return a.result, a.err
	case result.Status == capability.StatusDenied:
		// Charter's gates run before its transport: a denied call never left.
		return domain.ToolResult{}, fmt.Errorf("%w: %w: charter %s: %s", domain.ErrNotSent, domain.ErrForbidden, result.Requirement, result.Reason)
	case result.Status == capability.StatusUnknown:
		return domain.ToolResult{}, fmt.Errorf("%w: charter %s: %s", domain.ErrEffectUnknown, result.Requirement, result.Reason)
	case result.Status == capability.StatusFailed:
		// Charter settles failed only when the call was not executed or its effect is proven absent.
		return domain.ToolResult{}, fmt.Errorf("%w: charter %s %s: %s", domain.ErrNoEffect, result.Status, result.Requirement, result.Reason)
	}
	return domain.ToolResult{}, fmt.Errorf("%w: charter %s %s: %s", domain.ErrEffectUnknown, result.Status, result.Requirement, result.Reason)
}

func (a *attempt) recordedOutcome(result capability.Result) (domain.ToolResult, error) {
	outputs, err := rawOutputs(result.Outputs)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("encode outputs of %s: %w", a.call.ToolID, err)
	}
	output, err := json.Marshal(toolOutput{Outcome: result.Outcome, BusinessError: result.BusinessError,
		Outputs: outputs, ExternalReference: result.ExternalReference})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("encode recorded outcome of %s: %w", a.call.ToolID, err)
	}
	outcome := a.result
	outcome.Output = output
	return outcome, nil
}

func rawOutputs(outputs any) (json.RawMessage, error) {
	switch value := outputs.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		if !json.Valid(value) {
			return nil, fmt.Errorf("invalid JSON")
		}
		return value, nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}
}
