package charter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

func TestRunActionsReadsTheDispositionsOfOneRunInTheTenantsNamespace(t *testing.T) {
	h := newHarborGateway(t)
	ctx := context.Background()
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:1")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.gateway.Invoke(ctx, reserveCall("turn-42:2")); err != nil {
		t.Fatal(err)
	}
	namespaces := map[int64]string{7: harborNS, 8: "other.example"}
	source := &RunActions{Evidence: h.runtime.Evidence, Namespace: func(tenantID int64) string { return namespaces[tenantID] }}
	actions, err := source.RunActions(ctx, 7, "turn-42", 10)
	if err != nil || len(actions) != 2 {
		t.Fatalf("actions = %+v, %v", actions, err)
	}
	action := actions[0]
	if action.Disposition != string(capability.StatusExecuted) || action.Capability.ID != "CAP-RESERVE-ORDER-STOCK" || action.ActionID.Namespace != harborNS ||
		action.Assignment == nil || action.Assignment.ID != "ASGN-OEC-ORDER-EXCEPTION-SUPPORT" || action.At.IsZero() || len(action.Authority) == 0 {
		t.Fatalf("action = %+v", action)
	}
	if capped, err := source.RunActions(ctx, 7, "turn-42", 1); err != nil || len(capped) != 1 {
		t.Fatalf("limit 1 = %+v, %v", capped, err)
	}
	for tenant, request := range map[int64]string{8: "turn-42", 7: "turn-43"} {
		if other, err := source.RunActions(ctx, tenant, request, 10); err != nil || len(other) != 0 {
			t.Fatalf("tenant %d request %s = %+v, %v", tenant, request, other, err)
		}
	}
	if _, err := source.RunActions(ctx, 9, "turn-42", 10); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a tenant without a namespace = %v, want not ready", err)
	}
	if _, err := source.RunActions(ctx, 7, " ", 10); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a blank request = %v, want validation", err)
	}
	if _, err := (&RunActions{}).RunActions(ctx, 7, "turn-42", 10); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("an uncomposed source = %v, want not ready", err)
	}
}

func TestCharterActionKeepsConditionResultsAndEffectButNotReasons(t *testing.T) {
	observed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	action := charterAction(model.ActionRecord{
		Envelope: model.Envelope{Namespace: harborNS, ID: "ACT-9"}, CapabilityID: model.Ref{ID: "CAP-RESERVE-ORDER-STOCK"},
		PreconditionEvaluations: []model.PreconditionEvaluation{{PreconditionID: "PRE-STOCK", Result: "satisfied", Reason: "SECRET-PRE", ObservedAt: observed}},
		PostconditionEvaluations: []model.PostconditionEvaluation{
			{PostconditionID: "POST-RESERVED", Result: model.PostconditionSatisfied, Reason: "SECRET-POST", ObservedAt: observed.Add(time.Second)},
			{PostconditionID: "POST-NOTIFIED", Result: model.PostconditionUnknown, Reason: "SECRET-POST"},
		},
		Effect: model.EffectPartial, UnknownCause: model.UnknownObservationFailed,
	})
	if len(action.Preconditions) != 1 || action.Preconditions[0] != (domain.CharterCondition{ID: "PRE-STOCK", Result: "satisfied", ObservedAt: observed}) {
		t.Fatalf("preconditions = %+v", action.Preconditions)
	}
	if len(action.Postconditions) != 2 || action.Postconditions[0].ID != "POST-RESERVED" || !action.Postconditions[0].ObservedAt.Equal(observed.Add(time.Second)) ||
		action.Postconditions[1].Result != model.PostconditionUnknown || !action.Postconditions[1].ObservedAt.IsZero() {
		t.Fatalf("postconditions = %+v", action.Postconditions)
	}
	if action.Effect != model.EffectPartial || action.UnknownCause != model.UnknownObservationFailed {
		t.Fatalf("effect = %q, unknown cause = %q", action.Effect, action.UnknownCause)
	}
	if strings.Contains(fmt.Sprintf("%+v", action), "SECRET") {
		t.Fatalf("a condition reason leaked: %+v", action)
	}
	if bare := charterAction(model.ActionRecord{}); bare.Preconditions != nil || bare.Postconditions != nil || bare.Effect != "" {
		t.Fatalf("an action without conditions = %+v", bare)
	}
}
