package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// releaseAutonomy is the autonomy resource, with an optional daily operating
// window in minutes from midnight UTC.
type releaseAutonomy struct {
	Mode       domain.AutonomyMode `json:"mode"`
	WindowFrom int                 `json:"window_from_minute,omitempty"`
	WindowTo   int                 `json:"window_to_minute,omitempty"`
}

// releaseExecutionLimits is the limits resource; zero is not limited.
type releaseExecutionLimits struct {
	TurnsPerMinute     int64 `json:"turns_per_minute"`
	MaxSteps           int64 `json:"max_steps"`
	MaxToolCalls       int64 `json:"max_tool_calls"`
	MaxDurationSeconds int64 `json:"max_duration_seconds"`
}

// ReleaseLimits reads the budget, autonomy, and execution limits a principal was
// published with. Compilation already narrowed them against every parent scope, so
// nothing here re-derives inheritance: it reads one frozen row.
type ReleaseLimits struct {
	Releases contract.EffectiveReleaseRepository
	// Fallback is returned with Source fallback when the release binds no budget; its zero value
	// is unbounded, and a bounded Fallback needs a valid period.
	Fallback domain.ReleaseBudget
}

// Budget returns the principal's frozen budget and its period.
func (l *ReleaseLimits) Budget(ctx context.Context, principal domain.Principal) (domain.ReleaseBudget, error) {
	value, found, err := l.resource(ctx, principal, domain.ResourceBudget)
	if err != nil {
		return domain.ReleaseBudget{}, err
	}
	if !found {
		return l.fallback()
	}
	budget, err := scope.DecodeBudget(value)
	if err != nil {
		return domain.ReleaseBudget{}, fmt.Errorf("release budget is malformed: %w", err)
	}
	return budget, nil
}

func (l *ReleaseLimits) fallback() (domain.ReleaseBudget, error) {
	if !l.Fallback.Bounded() {
		return domain.ReleaseBudget{}, nil
	}
	if err := l.Fallback.Period.Validate(); err != nil {
		return domain.ReleaseBudget{}, fmt.Errorf("release limits: fallback budget period: %v", err)
	}
	budget := l.Fallback
	budget.Source = domain.BudgetSourceFallback
	return budget, nil
}

// AutonomyMode degrades a bounded mode to execute-with-approval outside its
// operating window, so an out-of-hours agent asks instead of stopping.
func (l *ReleaseLimits) AutonomyMode(ctx context.Context, principal domain.Principal, at time.Time) (domain.AutonomyMode, error) {
	value, found, err := l.resource(ctx, principal, domain.ResourceAutonomy)
	if err != nil {
		return "", err
	}
	if !found {
		return domain.AutonomyHumanOnly, nil
	}
	var autonomy releaseAutonomy
	if err := json.Unmarshal(value, &autonomy); err != nil {
		return "", fmt.Errorf("%w: release autonomy is malformed: %v", domain.ErrValidation, err)
	}
	if autonomy.Mode != domain.AutonomyBounded || autonomy.WindowTo == 0 {
		return autonomy.Mode, nil
	}
	minute := at.UTC().Hour()*60 + at.UTC().Minute()
	if minute < autonomy.WindowFrom || minute >= autonomy.WindowTo {
		return domain.AutonomyExecuteWithApproval, nil
	}
	return autonomy.Mode, nil
}

// ExecutionLimits returns the frozen limits binding; a release that binds none limits nothing.
func (l *ReleaseLimits) ExecutionLimits(ctx context.Context, principal domain.Principal) (domain.ExecutionLimits, error) {
	value, found, err := l.resource(ctx, principal, domain.ResourceLimits)
	if err != nil || !found {
		return domain.ExecutionLimits{}, err
	}
	return decodeExecutionLimits(value)
}

// FrozenExecutionLimits reads the limits binding of settings frozen but not stored, such as a
// frozen draft's; settings binding none limit nothing.
func FrozenExecutionLimits(resources []domain.EffectiveResource) (domain.ExecutionLimits, error) {
	for _, resource := range resources {
		if resource.ResourceKind == domain.ResourceLimits {
			return decodeExecutionLimits(resource.Value)
		}
	}
	return domain.ExecutionLimits{}, nil
}

func decodeExecutionLimits(value []byte) (domain.ExecutionLimits, error) {
	var frozen releaseExecutionLimits
	if err := json.Unmarshal(value, &frozen); err != nil {
		return domain.ExecutionLimits{}, fmt.Errorf("%w: release limits are malformed: %v", domain.ErrValidation, err)
	}
	for _, limit := range []int64{frozen.TurnsPerMinute, frozen.MaxSteps, frozen.MaxToolCalls, frozen.MaxDurationSeconds} {
		if limit < 0 || limit > domain.MaxExecutionLimit {
			return domain.ExecutionLimits{}, fmt.Errorf("%w: release limits are outside 0 to %d", domain.ErrValidation, domain.MaxExecutionLimit)
		}
	}
	return domain.ExecutionLimits{
		TurnsPerMinute: int(frozen.TurnsPerMinute), MaxSteps: int(frozen.MaxSteps), MaxToolCalls: int(frozen.MaxToolCalls),
		MaxDuration: time.Duration(frozen.MaxDurationSeconds) * time.Second,
	}, nil
}

func (l *ReleaseLimits) resource(ctx context.Context, principal domain.Principal, kind domain.ResourceKind) ([]byte, bool, error) {
	if l.Releases == nil {
		return nil, false, fmt.Errorf("release limits: effective release repository is required")
	}
	if principal.Release == "" {
		return nil, false, fmt.Errorf("%w: principal %q is not pinned to a release", domain.ErrForbidden, principal.ID)
	}
	release, err := l.Releases.Get(ctx, principal.TenantID, principal.ID, principal.Release)
	if err != nil {
		return nil, false, err
	}
	for _, resource := range release.Resources {
		if resource.ResourceKind == kind {
			return resource.Value, true, nil
		}
	}
	return nil, false, nil
}

var _ contract.PrincipalLimits = (*ReleaseLimits)(nil)
