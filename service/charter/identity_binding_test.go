package charter

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/charter/sdk/agent"

	"github.com/nauticana/scout/domain"
)

func TestIdentityBindingRefusesAReleaseCharterDoesNotDeclare(t *testing.T) {
	binding := &IdentityBinding{Agents: agent.NewBaseProvider(harbor(t)), Namespace: func(int64) string { return harborNS }}
	ctx := context.Background()
	release := domain.AgentDefinition{AgentID: "AGENT-ORDER-EXCEPTION-COORDINATOR", Version: "1"}
	if err := binding.WriteRelease(ctx, nil, 7, release); err != nil {
		t.Fatalf("a declared identity and version: %v", err)
	}
	release.Version = "2"
	if err := binding.WriteRelease(ctx, nil, 7, release); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an undeclared version: want ErrValidation, got %v", err)
	}
	release.AgentID = "AGENT-NOWHERE"
	if err := binding.WriteRelease(ctx, nil, 7, release); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown identity: want ErrValidation, got %v", err)
	}
	unmapped := &IdentityBinding{Agents: binding.Agents, Namespace: func(int64) string { return "" }}
	if err := unmapped.WriteRelease(ctx, nil, 7, release); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("an unmapped tenant: want ErrNotReady, got %v", err)
	}
}

func TestIdentityBindingKeepsToTheTenantsNamespace(t *testing.T) {
	c := harbor(t)
	definitionDoc, _ := c.Get(harborNS, oecDefinition)
	addVariant(t, c, definitionDoc, map[string]any{
		"namespace": otherNS, "id": "AGENTDEF-FOREIGN", "definitionVersion": "9",
		"agentIdentityId": map[string]any{"namespace": harborNS, "id": oecAgent},
	})
	ctx := context.Background()
	binding := &IdentityBinding{Agents: agent.NewBaseProvider(c), Namespace: func(int64) string { return harborNS }}
	if err := binding.WriteRelease(ctx, nil, 7, domain.AgentDefinition{AgentID: oecAgent, Version: "9"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a definition in another namespace declared a version: want ErrValidation, got %v", err)
	}
	if err := binding.WriteRelease(ctx, nil, 7, domain.AgentDefinition{AgentID: oecAgent, Version: "1"}); err != nil {
		t.Fatalf("the tenant's own definition: %v", err)
	}
	if err := binding.WriteRelease(ctx, nil, 7, domain.AgentDefinition{AgentID: "POS-SALES-OPERATIONS-MANAGER", Version: "1"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an id naming another kind: want ErrValidation, got %v", err)
	}
	var unwired IdentityBinding
	if err := unwired.WriteRelease(ctx, nil, 7, domain.AgentDefinition{AgentID: oecAgent, Version: "1"}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a zero-value binding: want ErrNotReady, got %v", err)
	}
}
