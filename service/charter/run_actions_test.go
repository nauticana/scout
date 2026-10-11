package charter

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/charter/sdk/capability"

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
