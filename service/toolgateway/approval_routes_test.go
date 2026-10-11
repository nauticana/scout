package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/approval"
	"github.com/nauticana/scout/service/guardrail"
)

// routedChain composes the gateway with the durable gate and the layered guardrails: the operating
// mode's ask routes to role clerk, the irreversible rule's ask to position cfo.
func routedChain(t *testing.T, sent *int) (*GovernedGateway, map[int64]domain.ApprovalRequest) {
	t.Helper()
	requests := map[int64]domain.ApprovalRequest{}
	store := &fake.ApprovalStore{
		OpenFunc: func(_ context.Context, request domain.ApprovalRequest) (domain.ApprovalRequest, error) {
			request.Status = domain.ApprovalStatusPending
			requests[request.ExecutionStepID] = request
			return request, nil
		},
		GetFunc: func(_ context.Context, key domain.ApprovalKey) (domain.ApprovalRequest, error) {
			if request, ok := requests[key.ExecutionStepID]; ok {
				return request, nil
			}
			return domain.ApprovalRequest{}, domain.ErrNotFound
		},
	}
	gate := &approval.Gate{Store: store, Deadline: time.Hour, Router: fake.ApproverRouterFunc(
		func(_ context.Context, request domain.ApprovalRequest, _ domain.PrincipalRef) (domain.ApprovalAssignment, error) {
			if request.Source == domain.ApprovalSourceGuardrail {
				return domain.ApprovalAssignment{Approver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "90"},
					Authority: domain.ApprovalRouteAuthority{RouteID: "irrev", Reference: domain.ApproverReference{Kind: domain.ApproverPosition, Ref: "cfo"}}}, nil
			}
			return domain.ApprovalAssignment{Approver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"},
				Authority: domain.ApprovalRouteAuthority{RouteID: "ops", Reference: domain.ApproverReference{Kind: domain.ApproverRole, Ref: "clerk"}}}, nil
		})}
	compiler, err := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	enforcer, err := guardrail.NewLayeredEnforcer(guardrail.EnforcerConfig{
		Baseline: domain.GuardrailRuleSet{SchemaVersion: guardrail.RuleSetSchemaVersion}, Compiler: compiler,
		Approvals: gate, Events: &fake.SafetyEventSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(domain.GuardrailRuleSet{SchemaVersion: guardrail.RuleSetSchemaVersion, Rules: []domain.GuardrailRule{{
		ID: "irreversible", Kind: domain.GuardrailKindIrreversibleToolApproval, Action: domain.GuardrailActionBlock,
		Severity: domain.GuardrailSeverityHard, Params: json.RawMessage(`{"tools":["search"]}`),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	config := domain.GuardrailConfig{Version: "policy-1", RulesDigest: guardrail.Digest(encoded), Rules: encoded}
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, "", sent, &asked)
	gateway.Approvals, gateway.Guardrails = gate, enforcer
	gateway.GuardrailConfigs = fake.ToolGuardrailConfigResolverFunc(func(context.Context, domain.ToolCall) (domain.GuardrailConfig, error) {
		return config, nil
	})
	return gateway, requests
}

func approveAll(requests map[int64]domain.ApprovalRequest) {
	for step, request := range requests {
		request.Status = domain.ApprovalStatusApproved
		requests[step] = request
	}
}

func TestAGuardrailAskWaitsForItsOwnRouteAfterTheModesApproval(t *testing.T) {
	sent := 0
	gateway, requests := routedChain(t, &sent)
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrApprovalPending) || len(requests) != 1 {
		t.Fatalf("first ask: %v, requests %d", err, len(requests))
	}
	approveAll(requests)
	// The clerk's approval does not decide the irreversible rule's ask, routed to the cfo.
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrApprovalPending) || sent != 0 || len(requests) != 2 {
		t.Fatalf("guardrail ask: %v, sent %d, requests %d", err, sent, len(requests))
	}
	var guarded domain.ApprovalRequest
	for _, request := range requests {
		if request.Source == domain.ApprovalSourceGuardrail {
			guarded = request
		}
	}
	if guarded.Route.RouteID != "irrev" || guarded.Approver.ID != "90" || guarded.RuleID != "irreversible" {
		t.Fatalf("guardrail request = %+v", guarded)
	}
	approveAll(requests)
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil || sent != 1 || len(requests) != 2 {
		t.Fatalf("both approved: %v, sent %d, requests %d", err, sent, len(requests))
	}
}
