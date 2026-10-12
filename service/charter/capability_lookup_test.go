package charter

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/evidence"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

var _ contract.ToolOutcomeLookup = (*CapabilityGateway)(nil)

// As the turn's outcome lookup, the gateway reconciles a governed call's key from observation
// alone: the attempt its run recorded unknown settles executed without a second effect, and the
// answer is completed with the governed output. A call no recorded attempt names, an unmapped
// tool, and a gateway that cannot find actions all answer unknown.
func TestCapabilityGatewayLooksAnUnknownCallUpWithoutSendingIt(t *testing.T) {
	h := newHarborGateway(t)
	ctx := context.Background()
	*h.answer = func() (domain.ToolResult, error) { return domain.ToolResult{}, errors.New("timeout") }
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:2")); !errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("the attempt = %v, want unknown", err)
	}
	sent := len(*h.inner)
	h.gateway.Reconciler = h.runtime.Reconciler
	if report, err := h.gateway.Lookup(ctx, reserveCall("turn-42:2"), domain.ToolDefinition{}); err != nil || report.Status != domain.CallUnknown {
		t.Fatalf("a lookup that cannot read actions = %+v, %v", report, err)
	}
	h.gateway.Actions = evidence.Queries{Provider: h.runtime.Evidence}
	h.gateway.MaxActions = 1
	if bounded, err := h.gateway.Lookup(ctx, reserveCall("turn-42:2"), domain.ToolDefinition{}); err != nil || bounded.Status != domain.CallUnknown {
		t.Fatalf("a lookup with truncated history = %+v, %v", bounded, err)
	}
	h.gateway.MaxActions = 1000
	report, err := h.gateway.Lookup(ctx, reserveCall("turn-42:2"), domain.ToolDefinition{})
	if err != nil || report.Status != domain.CallCompleted || report.Resolution != domain.ResolvedByLookup ||
		string(report.Result.Output) != `{"outcome":"reserved"}` || report.Result.Usage.ToolCalls != 1 {
		t.Fatalf("lookup = %+v, %v", report, err)
	}
	if len(*h.inner) != sent {
		t.Fatalf("a lookup sent the call: %d calls, was %d", len(*h.inner), sent)
	}
	actions, _ := (evidence.Queries{Provider: h.runtime.Evidence}).ActionsIn(ctx, harborNS, "turn-42", 10)
	if last := actions[len(actions)-1]; last.ReconcilesActionID == nil || last.Disposition != string(capability.StatusExecuted) {
		t.Fatalf("the lookup's reconciliation must name the unknown attempt: %+v", last)
	}
	if again, err := h.gateway.Lookup(ctx, reserveCall("turn-42:2"), domain.ToolDefinition{}); err != nil || again.Status != domain.CallCompleted {
		t.Fatalf("a repeated lookup = %+v, %v", again, err)
	}
	if never, err := h.gateway.Lookup(ctx, reserveCall("turn-42:8"), domain.ToolDefinition{}); err != nil || never.Status != domain.CallUnknown || len(*h.inner) != sent {
		t.Fatalf("a call no attempt names = %+v, %v", never, err)
	}
	unmapped := reserveCall("turn-42:2")
	unmapped.ToolID = "lookup"
	if report, err := h.gateway.Lookup(ctx, unmapped, domain.ToolDefinition{}); err != nil || report.Status != domain.CallUnknown {
		t.Fatalf("an unmapped tool = %+v, %v", report, err)
	}
}

// What Charter decided becomes what the turn records: a denial never left, a failure had no
// effect, and anything Charter cannot settle is unknown.
func TestCapabilityGatewayClassifiesCharterDecisions(t *testing.T) {
	notReached := &attempt{call: domain.ToolCall{ToolID: "reserve"}}
	if _, err := notReached.outcome(capability.Result{Status: capability.StatusDenied, Reason: "no authority"}); !errors.Is(err, domain.ErrNotSent) || !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a denial = %v", err)
	}
	if _, err := notReached.outcome(capability.Result{Status: capability.StatusFailed, Reason: "postcondition violated"}); !errors.Is(err, domain.ErrNoEffect) || errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("a failure = %v", err)
	}
	if _, err := notReached.outcome(capability.Result{Status: capability.StatusUnknown}); !errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("an unknown outcome = %v", err)
	}
	h := newHarborGateway(t)
	h.gateway.Invocations = harborInvocations{refuse: errors.New("no assignment")}
	if _, err := h.gateway.Invoke(context.Background(), reserveCall("turn-42:6")); !errors.Is(err, domain.ErrNotSent) {
		t.Fatalf("an unattributable call never left: %v", err)
	}
}
