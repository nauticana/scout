package toolgateway

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qEgressRuleMatch = "scout_tool_egress_rule_match"

var egressPolicyQueries = map[string]string{
	qEgressRuleMatch: `
SELECT 1
  FROM tool_egress_rule
 WHERE tenant_id = ? AND protocol = ? AND host = ? AND port = ?
 LIMIT 1`,
}

// TableEgressPolicy admits a destination only when the tenant holds a
// tool_egress_rule for its exact protocol, host, and port. In-process endpoints
// leave no network, so they pass without a rule.
type TableEgressPolicy struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

func (policy *TableEgressPolicy) ValidateDestination(ctx context.Context, tenantID int64, endpoint string) error {
	destination, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || destination.Scheme == "" || destination.Host == "" {
		return fmt.Errorf("%w: tool endpoint %q is not an absolute URL", domain.ErrValidation, endpoint)
	}
	if destination.Scheme == InProcessScheme {
		return nil
	}
	protocol := strings.ToLower(destination.Scheme)
	portNo := destination.Port()
	switch {
	case portNo != "":
	case protocol == "https":
		portNo = "443"
	case protocol == "http":
		portNo = "80"
	default:
		return fmt.Errorf("%w: tool endpoint protocol %q is not allowed", domain.ErrForbidden, protocol)
	}
	number, err := strconv.Atoi(portNo)
	if err != nil {
		return fmt.Errorf("%w: tool endpoint port %q", domain.ErrValidation, portNo)
	}
	if policy.DB == nil {
		return fmt.Errorf("egress policy: database is required")
	}
	policy.once.Do(func() { policy.qs = policy.DB.GetQueryService(ctx, egressPolicyQueries) })
	matched, err := policy.qs.Query(ctx, qEgressRuleMatch, tenantID, protocol, strings.ToLower(destination.Hostname()), number)
	if err != nil {
		return fmt.Errorf("match egress rule: %w", err)
	}
	if len(matched.Rows) == 0 {
		return fmt.Errorf("%w: tenant %d has no egress rule for %s://%s:%d", domain.ErrForbidden, tenantID, protocol, destination.Hostname(), number)
	}
	return nil
}

var _ contract.ToolEgressPolicy = (*TableEgressPolicy)(nil)
