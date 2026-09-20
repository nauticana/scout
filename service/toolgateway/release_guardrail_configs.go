package toolgateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ReleaseGuardrailConfigs resolves a tool call's guardrail policy from the release its
// agent principal is pinned to, which is the version the runtime pins the conversation to.
// A principal that is not an agent runs no release, so the baseline alone governs its call.
type ReleaseGuardrailConfigs struct {
	Configs contract.GuardrailConfigRepository
}

var _ contract.ToolGuardrailConfigResolver = (*ReleaseGuardrailConfigs)(nil)

func (resolver *ReleaseGuardrailConfigs) GuardrailConfig(ctx context.Context, call domain.ToolCall) (domain.GuardrailConfig, error) {
	if resolver.Configs == nil {
		return domain.GuardrailConfig{}, fmt.Errorf("%w: release guardrail configs need a guardrail config repository", domain.ErrValidation)
	}
	principal := call.Principal
	if principal.Kind != domain.PrincipalAgent {
		return domain.GuardrailConfig{}, nil
	}
	if strings.TrimSpace(principal.ID) == "" || strings.TrimSpace(principal.Release) == "" {
		return domain.GuardrailConfig{}, fmt.Errorf("%w: agent principal %q is not pinned to a release", domain.ErrForbidden, principal.ID)
	}
	if principal.TenantID != call.TenantContext.TenantID {
		return domain.GuardrailConfig{}, fmt.Errorf("%w: principal and call tenants differ", domain.ErrForbidden)
	}
	return resolver.Configs.Get(ctx, principal.TenantID, principal.ID, principal.Release)
}
