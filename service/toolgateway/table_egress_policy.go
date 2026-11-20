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

const (
	qEgressRuleMatch  = "scout_tool_egress_rule_match"
	qEgressRuleInsert = "scout_tool_egress_rule_insert"
)

var egressPolicyQueries = map[string]string{
	qEgressRuleMatch: `
SELECT 1
  FROM tool_egress_rule
 WHERE partner_id = ? AND tool_id = ? AND tool_version = ? AND protocol = ? AND host = ? AND port = ?
 LIMIT 1`,
	qEgressRuleInsert: `
INSERT INTO tool_egress_rule (partner_id, tool_id, tool_version, protocol, host, port)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, tool_id, tool_version, protocol, host, port) DO NOTHING`,
}

// TableEgressPolicy admits a destination only when the tenant holds the tool version's
// tool_egress_rule for its exact protocol, host, and port. In-process endpoints
// leave no network, so they pass without a rule. Allow writes the rules.
type TableEgressPolicy struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

// destination is an endpoint's protocol, lowercase host, and port; local is set for an in-process endpoint.
type destination struct {
	protocol, host string
	port           int
	local          bool
}

func (policy *TableEgressPolicy) destination(endpoint string) (destination, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return destination{}, fmt.Errorf("%w: tool endpoint %q is not an absolute URL", domain.ErrValidation, endpoint)
	}
	if parsed.Scheme == InProcessScheme {
		return destination{local: true}, nil
	}
	protocol := strings.ToLower(parsed.Scheme)
	portNo := parsed.Port()
	switch {
	case portNo != "":
	case protocol == "https":
		portNo = "443"
	case protocol == "http":
		portNo = "80"
	default:
		return destination{}, fmt.Errorf("%w: tool endpoint protocol %q is not allowed", domain.ErrForbidden, protocol)
	}
	number, err := strconv.Atoi(portNo)
	if err != nil {
		return destination{}, fmt.Errorf("%w: tool endpoint port %q", domain.ErrValidation, portNo)
	}
	return destination{protocol: protocol, host: strings.ToLower(parsed.Hostname()), port: number}, nil
}

func (policy *TableEgressPolicy) init(ctx context.Context) error {
	if policy.DB == nil {
		return fmt.Errorf("egress policy: database is required")
	}
	policy.once.Do(func() { policy.qs = policy.DB.GetQueryService(ctx, egressPolicyQueries) })
	return nil
}

func (policy *TableEgressPolicy) ValidateDestination(ctx context.Context, tenantID int64, tool domain.ToolReference, endpoint string) error {
	target, err := policy.destination(endpoint)
	if err != nil || target.local {
		return err
	}
	if err = policy.init(ctx); err != nil {
		return err
	}
	matched, err := policy.qs.Query(ctx, qEgressRuleMatch, tenantID, tool.ToolID, tool.Version, target.protocol, target.host, target.port)
	if err != nil {
		return fmt.Errorf("match egress rule: %w", err)
	}
	if len(matched.Rows) == 0 {
		return fmt.Errorf("%w: tool %s@%s of tenant %d has no egress rule for %s://%s:%d", domain.ErrForbidden, tool.ToolID, tool.Version, tenantID, target.protocol, target.host, target.port)
	}
	return nil
}

// Allow records that a registered tool version may reach endpoint; repeating it is a no-op and an
// in-process endpoint needs no rule.
func (policy *TableEgressPolicy) Allow(ctx context.Context, tenantID int64, tool domain.ToolReference, endpoint string) error {
	if tenantID <= 0 || strings.TrimSpace(tool.ToolID) == "" || strings.TrimSpace(tool.Version) == "" {
		return fmt.Errorf("%w: tenant, tool id, and version are required", domain.ErrValidation)
	}
	target, err := policy.destination(endpoint)
	if err != nil || target.local {
		return err
	}
	if err = policy.init(ctx); err != nil {
		return err
	}
	if _, err = policy.qs.Query(ctx, qEgressRuleInsert, tenantID, tool.ToolID, tool.Version, target.protocol, target.host, target.port); err != nil {
		return fmt.Errorf("allow egress of %s@%s: %w", tool.ToolID, tool.Version, err)
	}
	return nil
}

var _ contract.ToolEgressPolicy = (*TableEgressPolicy)(nil)
