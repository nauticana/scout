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
