package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func routingChange(value string) domain.AgentSettingChange {
	return domain.AgentSettingChange{
		AgentID: "writer-a", ScopeID: "a:writer-a", ResourceKind: domain.ResourceApprovalRouting, ResourceID: "approval_routing",
		ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2, Value: json.RawMessage(value),
	}
}

func TestSaveSettingHoldsApprovalRoutingToTheLimits(t *testing.T) {
	service, query := settingsService(nil)
	if _, err := service.SaveSetting(context.Background(), humanActor(8, 9), routingChange(payRoute)); err != nil {
		t.Fatalf("zero limits take the structural bounds: %v", err)
	}
	if !slices.Contains(query.queries, qStudioInsertSetting) {
		t.Fatal("the routing was not bound")
	}
	for name, limits := range map[string]domain.ApprovalRoutingLimits{
		"raised floor":    {MinDurationSeconds: 3601},
		"lowered ceiling": {MaxDurationSeconds: 3599},
		"short window":    {MaxWindowSeconds: 4199},
	} {
		refused, query := settingsService(nil)
		refused.ApprovalRoutingLimits = limits
		_, err := refused.SaveSetting(context.Background(), humanActor(8, 9), routingChange(payRoute))
		if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"value"}) || slices.Contains(query.queries, qStudioInsertSetting) {
			t.Errorf("%s: fields = %v, err = %v", name, fields, err)
		}
	}
	misconfigured, query := settingsService(nil)
	misconfigured.ApprovalRoutingLimits = domain.ApprovalRoutingLimits{MaxRoutesPerBinding: -1}
	_, err := misconfigured.SaveSetting(context.Background(), humanActor(8, 9), routingChange(payRoute))
	if err == nil || errors.Is(err, domain.ErrValidation) || slices.Contains(query.queries, qStudioInsertSetting) {
		t.Fatalf("invalid limits are a composition error, not the client's: %v", err)
	}
	removal := routingChange("")
	removal.Remove, removal.Value = true, nil
	if _, err := misconfigured.SaveSetting(context.Background(), humanActor(8, 9), removal); err != nil {
		t.Fatalf("removing a routing checks no value: %v", err)
	}
}

func TestStudioImportHoldsApprovalRoutingToTheLimits(t *testing.T) {
	h := newPortabilityHarness(t)
	mapping := map[domain.ResourceKind]map[string]string{domain.ResourceApprovalRouting: {"role:cfo": "role:finance", "position:lead": "position:p-9"}}
	adopt := func() error {
		_, err := h.studio.adopt(context.Background(), humanActor(9, 21), "writer", "document.bindings", []domain.PortableBinding{routingBinding()}, mapping, adoptImport, nil)
		return err
	}
	h.studio.ApprovalRoutingLimits = domain.ApprovalRoutingLimits{MaxToolPatterns: 1, MinDurationSeconds: 3600, MaxDurationSeconds: 3600, MaxWindowSeconds: 7200}
	if err := adopt(); err != nil {
		t.Fatalf("a routing within the limits: %v", err)
	}
	h.studio.ApprovalRoutingLimits.MaxWindowSeconds = 7199
	if fields := fieldsOf(t, adopt()); !slices.Equal(fields, []string{"document.bindings[0].value"}) {
		t.Fatalf("fields = %v", fields)
	}
	h.studio.ApprovalRoutingLimits = domain.ApprovalRoutingLimits{MaxToolPatterns: 99}
	if err := adopt(); err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid limits are a composition error: %v", err)
	}
}

func TestApprovalGuardBoundsTheEffectiveRoutes(t *testing.T) {
	var checked []string
	twoRoutes := `{"routes":[{"id":"wire","match":{"tools":["pay.wire"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600},` +
		`{"id":"pay","match":{"tools":["pay.*"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600}]}`
	candidate := routingCandidate([]string{"pay.wire"}, frozenSetting(t, domain.ResourceAutonomy, executeWithApproval), frozenSetting(t, domain.ResourceApprovalRouting, twoRoutes))
	guard := &StudioApprovalRouting{Resolver: knownReferences(&checked)}
	if err := guard.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("zero takes domain.MaxApprovalRoutes: %v", err)
	}
	guard.MaxRoutes = 2
	if err := guard.CheckPublish(context.Background(), candidate); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	guard.MaxRoutes = 1
	messages := routingFields(t, guard.CheckPublish(context.Background(), candidate))
	if len(messages) != 1 || !strings.Contains(messages[0], "holds 2 routes, the limit is 1") {
		t.Fatalf("messages = %v", messages)
	}
	for _, invalid := range []int{-1, domain.MaxApprovalRoutes + 1} {
		guard.MaxRoutes = invalid
		if err := guard.CheckPublish(context.Background(), candidate); err == nil || errors.Is(err, domain.ErrValidation) {
			t.Fatalf("MaxRoutes %d: want a composition error, got %v", invalid, err)
		}
	}
}
