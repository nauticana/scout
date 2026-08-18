package toolgateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BindingAuthorizer enforces the calling principal's agent_tool_binding at invoke
// time. A tenant holding a tool grants nothing on its own: the principal's pinned
// release must bind that tool, at that version.
type BindingAuthorizer struct {
	Registry contract.ToolRegistry
	// Verifier is optional; when set, a delegated principal's authority chain is
	// checked before its bindings are.
	Verifier contract.DelegationVerifier
}

// Authorize reports whether the principal may invoke the requested tool version.
func (a *BindingAuthorizer) Authorize(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) error {
	if a.Registry == nil {
		return fmt.Errorf("binding authorizer: tool registry is required")
	}
	if call.Principal.Kind != domain.PrincipalAgent && call.Principal.Kind != domain.PrincipalService {
		return fmt.Errorf("%w: %s principals cannot invoke tools directly", domain.ErrForbidden, call.Principal.Kind)
	}
	if strings.TrimSpace(call.Principal.Release) == "" {
		return fmt.Errorf("%w: principal %q is not pinned to a release", domain.ErrForbidden, call.Principal.ID)
	}
	if len(call.Principal.Authority) > 0 && a.Verifier == nil {
		return fmt.Errorf("%w: delegated principal %q has no authority-chain verifier", domain.ErrDegraded, call.Principal.ID)
	}
	if a.Verifier != nil {
		if _, err := a.Verifier.Verify(ctx, call.Principal); err != nil {
			return err
		}
	}
	bound, err := a.Registry.List(ctx, call.TenantContext.TenantID, call.Principal.ID, call.Principal.Release)
	if err != nil {
		return err
	}
	for _, candidate := range bound {
		if candidate.ToolID == definition.ToolID && candidate.Version == definition.Version {
			return nil
		}
	}
	return fmt.Errorf("%w: release %s of agent %q does not bind tool %s@%s",
		domain.ErrForbidden, call.Principal.Release, call.Principal.ID, definition.ToolID, definition.Version)
}

var _ contract.ToolAuthorizer = (*BindingAuthorizer)(nil)
