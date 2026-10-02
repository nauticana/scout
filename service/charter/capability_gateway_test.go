package charter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/charter/sdk/authority"
	"github.com/nauticana/charter/sdk/binding"
	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/evidence"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/validate"

	"github.com/nauticana/scout/domain"
)

// harborAttempt is inside the coordinator's grant and its credit approval.
var harborAttempt = time.Date(2026, 6, 18, 17, 15, 45, 0, time.UTC)

// harborInvocations attributes every call as harbor's coordinator reserving stock for order 0042 under its credit approval.
type harborInvocations struct{ refuse error }

func (h harborInvocations) Invocation(_ context.Context, _ domain.ToolCall, _ model.Ref) (capability.Invocation, error) {
	if h.refuse != nil {
		return capability.Invocation{}, h.refuse
	}
	return capability.Invocation{
		Namespace: harborNS, EnterpriseID: model.Ref{ID: "ENT-HARBOR"},
		Actor:        model.ObjectRef{Kind: model.KindAgentIdentity, ID: "AGENT-ORDER-EXCEPTION-COORDINATOR"},
		Runtime:      model.RuntimeContext{RuntimeInstanceID: model.Ref{ID: "RT-OEC-PROD-01"}},
		AssignmentID: &model.Ref{ID: "ASGN-OEC-ORDER-EXCEPTION-SUPPORT"}, ResourceScope: "order exceptions assigned to the agent",
		OrganizationalContext: "OU-SALES-OPERATIONS", ApprovedAction: "CAP-APPROVE-CREDIT-EXCEPTION",
		Measures: map[string]authority.Measure{
			"reservation-value":    {Value: 1850000, Currency: "USD", CurrencyExponent: 2},
			"reservation-duration": {Value: 48, Unit: "hour"},
			"credit-exposure":      {Value: 1850000, Currency: "USD", CurrencyExponent: 2},
		},
		MaterialInputsDigest: "sha256:4f1d2a9c7b3e5d6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f",
		SubjectRefs:          []model.ObjectRef{{Kind: "ProcessInstance", ID: "PROCINST-OE-2026-0042"}},
		At:                   harborAttempt,
	}, nil
}

// reservationHeld observes the reservation the stock capability promises.
type reservationHeld struct{}

func (reservationHeld) Observe(_ context.Context, req binding.ObservationRequest) (binding.Observation, error) {
	evaluations := make([]model.PostconditionEvaluation, len(req.Postconditions))
	for i, post := range req.Postconditions {
		evaluations[i] = model.PostconditionEvaluation{PostconditionID: post.ID, Result: model.PostconditionSatisfied, Reason: "reservation observed",
			EvidenceRecordIDs: []model.Ref{{ID: "EVR-RESERVATION-OBSERVED"}}}
	}
	return binding.Observation{Outcome: "reserved", Evaluations: evaluations}, nil
}

type harborGateway struct {
	gateway *CapabilityGateway
	runtime validate.Runtime
	inner   *[]domain.ToolCall
	answer  *func() (domain.ToolResult, error)
}

func newHarborGateway(t *testing.T) harborGateway {
	t.Helper()
	c := harbor(t)
	calls, answer := &[]domain.ToolCall{}, new(func() (domain.ToolResult, error))
	*answer = func() (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte(`{"outcome":"reserved"}`), Usage: domain.Usage{ToolCalls: 1}}, nil
	}
	inner := gatewayFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		*calls = append(*calls, call)
		return (*answer)()
	})
	key := corpus.KeyOf(harborNS, model.Ref{Namespace: harborNS, ID: "BIND-S4-RESERVE-ORDER-STOCK-1"})
	transport := &Transport{Namespace: harborNS, Bindings: binding.NewBaseProvider(c), Endpoint: &GatewayEndpoint{Gateway: inner},
		Binder: &MappedBinder{Tools: map[corpus.DocumentKey]domain.ToolReference{key: {ToolID: "reserve", Version: "v1"}}}}
	runtime, err := (Runtime{Documents: c, Transport: transport, Observer: reservationHeld{}}).Compose(context.Background(), nil, validate.External{})
	if err != nil {
		t.Fatal(err)
	}
	return harborGateway{
		gateway: &CapabilityGateway{Invoker: runtime.Invoker, Next: inner, Invocations: harborInvocations{}, Now: func() time.Time { return harborAttempt },
			Capabilities: map[string]Capability{"reserve": {ID: model.Ref{ID: "CAP-RESERVE-ORDER-STOCK"}, ContractVersion: "1"}}},
		runtime: runtime, inner: calls, answer: answer,
	}
}

func reserveCall(key string) domain.ToolCall {
	return domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, RequestID: "turn-42", ConversationID: "conv-1",
		ToolID: "reserve", ToolVersion: "v1", Arguments: []byte(`{"order":"O-42"}`), IdempotencyKey: key}
}

func TestCapabilityGatewayRecordsTheRunsActionAndKeepsTheCall(t *testing.T) {
	h := newHarborGateway(t)
	ctx := context.Background()
	result, err := h.gateway.Invoke(ctx, reserveCall("turn-42:1"))
	if err != nil || string(result.Output) != `{"outcome":"reserved"}` || result.Usage.ToolCalls != 1 {
		t.Fatalf("Invoke = %+v, %v", result, err)
	}
	if len(*h.inner) != 1 || (*h.inner)[0].ConversationID != "conv-1" || (*h.inner)[0].IdempotencyKey != "turn-42:1" {
		t.Fatalf("the governed gateway must see the loop's own call: %+v", *h.inner)
	}
	replayed, err := h.gateway.Invoke(ctx, reserveCall("turn-42:1"))
	if err != nil || len(*h.inner) != 1 || string(replayed.Output) != `{"outcome":"reserved"}` {
		t.Fatalf("a replayed key must return the recorded outcome without a second effect: %s, %v, calls %d", replayed.Output, err, len(*h.inner))
	}
	actions, err := (evidence.Queries{Provider: h.runtime.Evidence}).ActionsIn(ctx, harborNS, "turn-42")
	if err != nil || len(actions) != 1 || actions[0].Disposition != string(capability.StatusExecuted) || actions[0].CapabilityID.ID != "CAP-RESERVE-ORDER-STOCK" {
		t.Fatalf("run actions = %+v, %v", actions, err)
	}
	if other, _ := (evidence.Queries{Provider: h.runtime.Evidence}).ActionsIn(ctx, harborNS, "turn-43"); len(other) != 0 {
		t.Fatalf("another run's actions = %+v", other)
	}
}

func TestCapabilityGatewayLinksAReconciliationToTheRun(t *testing.T) {
	h := newHarborGateway(t)
	ctx := context.Background()
	timeout := errors.New("timeout")
	*h.answer = func() (domain.ToolResult, error) { return domain.ToolResult{}, timeout }
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:2")); !errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("without a reconciler a timeout stays unknown: %v", err)
	}

	h.gateway.Reconciler = h.runtime.Reconciler
	result, err := h.gateway.Invoke(ctx, reserveCall("turn-42:3"))
	if err != nil || string(result.Output) != `{"outcome":"reserved"}` {
		t.Fatalf("an observed reservation reconciles the timeout: %s, %v", result.Output, err)
	}
	actions, err := (evidence.Queries{Provider: h.runtime.Evidence}).ActionsIn(ctx, harborNS, "turn-42")
	if err != nil || len(actions) != 3 {
		t.Fatalf("run actions = %+v, %v", actions, err)
	}
	unknown, reconciling := actions[1], actions[2]
	if unknown.Disposition != string(capability.StatusUnknown) || reconciling.ReconcilesActionID == nil || reconciling.ReconcilesActionID.ID != unknown.ID {
		t.Fatalf("the reconciling record must name the attempt it settles: %+v / %+v", unknown, reconciling)
	}
	if replayed, err := h.gateway.Invoke(ctx, reserveCall("turn-42:3")); err != nil || len(*h.inner) != 2 || string(replayed.Output) != `{"outcome":"reserved"}` {
		t.Fatalf("the reconciled key replays without a second effect: %v, calls %d", err, len(*h.inner))
	}
	// The first attempt returned holding no fence; its redelivery reclaims the key and settles it by observation.
	redelivered, err := h.gateway.Invoke(ctx, reserveCall("turn-42:2"))
	if err != nil || len(*h.inner) != 2 || string(redelivered.Output) != `{"outcome":"reserved"}` {
		t.Fatalf("a redelivered unknown call must reconcile without a second effect: %s, %v, calls %d", redelivered.Output, err, len(*h.inner))
	}
	actions, _ = (evidence.Queries{Provider: h.runtime.Evidence}).ActionsIn(ctx, harborNS, "turn-42")
	settled := 0
	for _, action := range actions {
		if action.ReconcilesActionID != nil && action.Disposition == string(capability.StatusExecuted) {
			settled++
		}
	}
	if len(actions) != 5 || settled != 2 {
		t.Fatalf("both unknown keys must end in a reconciling record of the run: %+v", actions)
	}
}

func TestCapabilityGatewayRefusesAndPassesThrough(t *testing.T) {
	h := newHarborGateway(t)
	ctx := context.Background()
	*h.answer = func() (domain.ToolResult, error) { return domain.ToolResult{}, domain.ErrApprovalPending }
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:3")); !errors.Is(err, domain.ErrApprovalPending) {
		t.Fatalf("a parked call must suspend the loop, got %v", err)
	}
	*h.answer = func() (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte(`{"outcome":"reserved"}`)}, nil
	}
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:3")); err != nil {
		t.Fatalf("after approval the released key must run: %v", err)
	}
	other := reserveCall("turn-42:4")
	other.ToolID = "lookup"
	if _, err := h.gateway.Invoke(ctx, other); err != nil || (*h.inner)[len(*h.inner)-1].ToolID != "lookup" {
		t.Fatalf("an unmapped tool goes straight to the governed gateway: %v", err)
	}
	malformed := reserveCall("turn-42:5")
	malformed.RequestID = "turn 42"
	if _, err := h.gateway.Invoke(ctx, malformed); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a request id that cannot be an execution context: %v", err)
	}
	h.gateway.Invocations = harborInvocations{refuse: errors.New("no assignment")}
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:6")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an unattributable call: %v", err)
	}
	late := reserveCall("turn-42:7")
	h.gateway.Invocations = lateInvocations{}
	if _, err := h.gateway.Invoke(ctx, late); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a call outside the grant's validity is denied: %v", err)
	}
}

type lateInvocations struct{}

func (lateInvocations) Invocation(ctx context.Context, call domain.ToolCall, target model.Ref) (capability.Invocation, error) {
	inv, err := harborInvocations{}.Invocation(ctx, call, target)
	inv.At = time.Date(2027, 6, 18, 0, 0, 0, 0, time.UTC)
	return inv, err
}

// A durable ledger returns the stored outputs decoded, not as raw JSON; the replayed body must be the same.
func TestReplayedOutcomeEncodesDecodedOutputs(t *testing.T) {
	replayed := &attempt{call: domain.ToolCall{ToolID: "reserve"}}
	result, err := replayed.outcome(capability.Result{Status: capability.StatusExecuted, Outcome: "reserved",
		Outputs: map[string]any{"reservation": "R-1"}, ExternalReference: "S4-9"})
	if err != nil || string(result.Output) != `{"outcome":"reserved","outputs":{"reservation":"R-1"},"external_reference":"S4-9"}` {
		t.Fatalf("outcome = %s, %v", result.Output, err)
	}
	if _, err = replayed.outcome(capability.Result{Status: capability.StatusFailed, Reason: "boom"}); err == nil {
		t.Fatal("a failed invocation must be an error")
	}
}

func TestBusinessErrorReplacesTheToolsSuccessPayload(t *testing.T) {
	attempted := &attempt{call: domain.ToolCall{ToolID: "reserve"}, reached: true,
		result: domain.ToolResult{Output: []byte(`{"outcome":"reserved"}`), Usage: domain.Usage{ToolCalls: 1}}}
	result, err := attempted.outcome(capability.Result{Status: capability.StatusBusinessError,
		BusinessError: "insufficient_stock", Outputs: json.RawMessage(`{"available":0}`)})
	if err != nil || string(result.Output) != `{"outcome":"","business_error":"insufficient_stock","outputs":{"available":0}}` || result.Usage.ToolCalls != 1 {
		t.Fatalf("business error = %s, usage = %+v, err = %v", result.Output, result.Usage, err)
	}
	if _, err = attempted.outcome(capability.Result{Status: capability.StatusBusinessError, Outputs: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid recorded outputs must not be silently dropped")
	}
}

func TestReconciliationIsJudgedWhenItHappens(t *testing.T) {
	h := newHarborGateway(t)
	h.gateway.Reconciler = h.runtime.Reconciler
	h.gateway.Now = func() time.Time { return harborAttempt.Add(48 * time.Hour) }
	*h.answer = func() (domain.ToolResult, error) { return domain.ToolResult{}, errors.New("timeout") }
	if _, err := h.gateway.Invoke(context.Background(), reserveCall("turn-42:9")); !errors.Is(err, domain.ErrEffectUnknown) {
		t.Fatalf("a reconciliation after the approval expired must not settle the call: %v", err)
	}
}
