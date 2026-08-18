package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// releaseBudget is the budget resource frozen into an effective release.
type releaseBudget struct {
	Tokens         int64  `json:"tokens"`
	CostMinorUnits int64  `json:"cost_minor_units"`
	Currency       string `json:"currency,omitempty"`
}

// releaseAutonomy is the autonomy resource, with an optional daily operating
// window in minutes from midnight UTC.
type releaseAutonomy struct {
	Mode       domain.AutonomyMode `json:"mode"`
	WindowFrom int                 `json:"window_from_minute,omitempty"`
	WindowTo   int                 `json:"window_to_minute,omitempty"`
}

// ReleaseLimits reads the budget and autonomy a principal was published with.
// Compilation already narrowed them against every parent scope, so nothing here
// re-derives inheritance: it reads one frozen row.
type ReleaseLimits struct {
	Releases contract.EffectiveReleaseRepository
	// Window is the rolling window the returned budget applies to.
	Window time.Duration
	// Fallback is returned when the release binds no budget; its zero value means unlimited.
	Fallback domain.BudgetLimits
}

// Budget returns the principal's ceiling within its tenant envelope.
func (l *ReleaseLimits) Budget(ctx context.Context, principal domain.Principal) (domain.BudgetLimits, error) {
	value, found, err := l.resource(ctx, principal, domain.ResourceBudget)
	if err != nil || !found {
		return l.Fallback, err
	}
	var budget releaseBudget
	if err := json.Unmarshal(value, &budget); err != nil {
		return domain.BudgetLimits{}, fmt.Errorf("%w: release budget is malformed: %v", domain.ErrValidation, err)
	}
	window := l.Window
	if window <= 0 {
		window = l.Fallback.Window
	}
	return domain.BudgetLimits{
		WindowTokens: budget.Tokens, WindowCostMinorUnits: budget.CostMinorUnits,
		Currency: budget.Currency, Window: window,
	}, nil
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
