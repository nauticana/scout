package isolation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	keelcommon "github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qEntitlementPlan          = "scout_entitlement_plan"
	qEntitlementExternalUsage = "scout_entitlement_external_usage"
)

var entitlementPolicyQueries = map[string]string{
	qEntitlementPlan: `
SELECT MAX(q.max_value), q.period_type
  FROM subscription_quota q
  JOIN partner_plan_subscription subscription ON subscription.plan_id = q.plan_id
 WHERE subscription.partner_id = ?
   AND subscription.status = 'A'
   AND subscription.begda <= CURRENT_TIMESTAMP
   AND (subscription.endda IS NULL OR subscription.endda >= CURRENT_TIMESTAMP)
   AND q.resource_id = ?
 GROUP BY q.period_type
 ORDER BY MAX(q.max_value) DESC
 LIMIT 1`,

	// usage_ledger remains the product-wide entitlement ledger while scout's
	// budget_reservation is authoritative for governed turns. Subtract events
	// already represented by settled reservations so the same cost is not
	// counted twice when consumers outside the governed path still use the
	// entitlement ledger.
	qEntitlementExternalUsage: `
SELECT GREATEST(
         COALESCE((SELECT SUM(amount)
                     FROM usage_ledger
                    WHERE partner_id = ? AND resource_name = ? AND usage_time >= ?), 0)
       - COALESCE((SELECT SUM(cost_minor_units)
                     FROM usage_event
                    WHERE tenant_id = ? AND category_code = ? AND occurred_at >= ?), 0),
         0)`,
}

// EntitlementBudgetPolicy projects a keel subscription entitlement into
// scout's rolling-window budget contract. It is intentionally an adapter: plan
// changes remain owned by the keel subscription tables and are visible
// immediately without copying per-tenant rows into tenant_quota.
type EntitlementBudgetPolicy struct {
	DB keelport.DatabaseRepository
	// Resource is the subscription_quota/usage_ledger resource id metered by
	// this budget (e.g. an AI-credit resource).
	Resource string
	// Currency denominates the budget in the model catalog (minor units).
	Currency string
	// UsageCategory is the usage_event category settled through the budget
	// ledger, subtracted from the entitlement ledger to avoid double counting.
	UsageCategory string

	once sync.Once
	qs   keelport.QueryService
	now  func() time.Time
}

var _ contract.TenantBudgetPolicy = (*EntitlementBudgetPolicy)(nil)

func (policy *EntitlementBudgetPolicy) init(ctx context.Context) error {
	if policy.DB == nil || policy.Resource == "" || policy.Currency == "" || policy.UsageCategory == "" {
		return errors.New("entitlement budget policy: DB, Resource, Currency, and UsageCategory are required")
	}
	policy.once.Do(func() {
		policy.qs = policy.DB.GetQueryService(ctx, entitlementPolicyQueries)
	})
	if policy.qs == nil {
		return errors.New("entitlement budget policy: query service is required")
	}
	return nil
}

func (policy *EntitlementBudgetPolicy) clock() time.Time {
	if policy.now != nil {
		return policy.now().UTC()
	}
	return time.Now().UTC()
}

// BudgetFor returns the still-available non-governed allowance as the ceiling
// scout combines with live/settled reservations. The entitlement is a cost
// budget; the token ceiling is deliberately non-binding.
func (policy *EntitlementBudgetPolicy) BudgetFor(ctx context.Context, tenantID int64) (domain.BudgetLimits, error) {
	if tenantID <= 0 {
		return domain.BudgetLimits{}, fmt.Errorf("%w: positive tenant is required", domain.ErrValidation)
	}
	if err := policy.init(ctx); err != nil {
		return domain.BudgetLimits{}, err
	}
	plan, err := policy.qs.Query(ctx, qEntitlementPlan, tenantID, policy.Resource)
	if err != nil {
		return domain.BudgetLimits{}, fmt.Errorf("load %s plan for tenant %d: %w", policy.Resource, tenantID, err)
	}
	if len(plan.Rows) == 0 || len(plan.Rows[0]) < 2 || plan.Rows[0][0] == nil {
		return domain.BudgetLimits{}, fmt.Errorf("%w: tenant %d has no %s entitlement", domain.ErrBudgetExceeded, tenantID, policy.Resource)
	}
	limit := keelcommon.AsInt64(plan.Rows[0][0])
	period := keelcommon.AsString(plan.Rows[0][1])
	now := policy.clock()
	windowStart, window, err := entitlementWindow(period, now)
	if err != nil {
		return domain.BudgetLimits{}, err
	}
	if limit < 0 {
		return domain.BudgetLimits{
			WindowTokens: math.MaxInt64 / 4, WindowCostMinorUnits: math.MaxInt64 / 4,
			Currency: policy.Currency, Window: window,
		}, nil
	}
	usage, err := policy.qs.Query(ctx, qEntitlementExternalUsage,
		tenantID, policy.Resource, windowStart, tenantID, policy.UsageCategory, windowStart)
	if err != nil {
		return domain.BudgetLimits{}, fmt.Errorf("load external %s usage for tenant %d: %w", policy.Resource, tenantID, err)
	}
	external := int64(0)
	if len(usage.Rows) > 0 && len(usage.Rows[0]) > 0 {
		external = keelcommon.AsInt64(usage.Rows[0][0])
	}
	remaining := limit - external
	if remaining < 0 {
		remaining = 0
	}
	return domain.BudgetLimits{
		WindowTokens: math.MaxInt64 / 4, WindowCostMinorUnits: remaining,
		Currency: policy.Currency, Window: window,
	}, nil
}

// entitlementWindow maps a subscription period to the calendar window scout's
// rolling-window ledger will look back over: elapsed time since the period
// start, so "reservations within the window" equals "reservations this period".
func entitlementWindow(period string, now time.Time) (time.Time, time.Duration, error) {
	var start time.Time
	switch period {
	case "D":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "M":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}, 0, fmt.Errorf("%w: entitlement period %q is not a bounded budget window", domain.ErrValidation, period)
	}
	window := max(now.Sub(start), time.Second)
	return start, window, nil
}
