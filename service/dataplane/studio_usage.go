package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qStudioUsage = "scout_studio_usage"

	// DefaultUsageWindow bounds one usage or feedback report, and DefaultUsageLines the lines a usage report returns.
	DefaultUsageWindow = 92 * 24 * time.Hour
	DefaultUsageLines  = 1000
)

var studioUsageQueries = map[string]string{
	qStudioUsage: `
SELECT split_part(u.subject_ref, '@', 2), COALESCE(u.on_behalf_kind, ''), COALESCE(u.on_behalf_id, ''),
       COALESCE(u.work_ref, ''), u.currency_code, c.exponent,
       COUNT(DISTINCT CASE WHEN u.model_id IS NOT NULL OR NOT EXISTS (
                SELECT 1 FROM usage_event m
                 WHERE m.partner_id = u.partner_id AND m.conversation_id = u.conversation_id
                   AND m.turn_no = u.turn_no AND m.category_code = u.category_code AND m.model_id IS NOT NULL)
            THEN u.turn_no || '/' || u.conversation_id END),
       CAST(SUM(u.input_tokens) AS BIGINT),
       CAST(SUM(u.output_tokens) AS BIGINT), CAST(SUM(u.tool_calls) AS BIGINT), CAST(SUM(u.cost_minor_units) AS BIGINT),
       COALESCE(u.provider_id, ''), COALESCE(u.model_id, '')
  FROM usage_event u
  JOIN agent_conversation conv ON conv.partner_id = u.partner_id AND conv.conversation_id = u.conversation_id
  JOIN currency c ON c.code = u.currency_code
 WHERE u.partner_id = ? AND conv.agent_id = ? AND u.occurred_at >= ? AND u.occurred_at < ?
 GROUP BY 1, 2, 3, 4, 5, 6, 12, 13
 ORDER BY 1, 12, 13, 2, 3, 4, 5
 LIMIT ?`,
}

// StudioUsage reports an agent's settled usage by release, model, person, work item,
// and currency, so cost per workflow comes from the same ledger that billed it.
type StudioUsage struct {
	DB         port.DatabaseRepository
	Authorizer contract.StudioAuthorizer
	// MaxWindow bounds one report's span, default DefaultUsageWindow; MaxLines the lines it returns, default DefaultUsageLines.
	MaxWindow time.Duration
	MaxLines  int

	once sync.Once
	qs   port.QueryService
}

var _ contract.StudioUsageBackend = (*StudioUsage)(nil)

// Usage returns the lines of [from, to); a window over MaxWindow, or one that
// would return more than MaxLines lines, is refused rather than cut short.
func (s *StudioUsage) Usage(ctx context.Context, actor domain.StudioActor, agentID string, from, to time.Time) ([]domain.UsageLine, error) {
	if s.DB == nil || s.Authorizer == nil {
		return nil, fmt.Errorf("studio usage: database and authorizer are required")
	}
	if s.MaxWindow < 0 || s.MaxLines < 0 {
		return nil, fmt.Errorf("studio usage: window and line limits must not be negative")
	}
	maxWindow, maxLines := positiveOr(s.MaxWindow, DefaultUsageWindow), positiveOr(s.MaxLines, DefaultUsageLines)
	if err := checkReportWindow("usage", from, to, maxWindow); err != nil {
		return nil, err
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, studioUsageQueries) })
	result, err := s.qs.Query(ctx, qStudioUsage, actor.TenantID, agentID, from.UTC(), to.UTC(), maxLines+1)
	if err != nil {
		return nil, fmt.Errorf("report agent usage: %w", err)
	}
	if len(result.Rows) > maxLines {
		return nil, fmt.Errorf("%w: the window has more than %d usage lines; narrow it", domain.ErrValidation, maxLines)
	}
	lines := make([]domain.UsageLine, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 13 {
			return nil, fmt.Errorf("decode agent usage: expected 13 columns, got %d", len(row))
		}
		line := domain.UsageLine{
			Release: common.AsString(row[0]), WorkRef: common.AsString(row[3]),
			Currency: strings.TrimSpace(common.AsString(row[4])), Exponent: int(common.AsInt64(row[5])),
			Turns: common.AsInt64(row[6]), InputTokens: common.AsInt64(row[7]), OutputTokens: common.AsInt64(row[8]),
			ToolCalls: common.AsInt64(row[9]), CostMinorUnits: common.AsInt64(row[10]),
			Model: domain.ModelReference{ProviderID: common.AsString(row[11]), ModelID: common.AsString(row[12])},
		}
		if kind := common.AsString(row[1]); kind != "" {
			line.OnBehalfOf = domain.PrincipalRef{Kind: domain.PrincipalKind(kind), ID: common.AsString(row[2])}
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// checkReportWindow refuses a report window that is empty, backwards, or longer than maxWindow.
func checkReportWindow(report string, from, to time.Time, maxWindow time.Duration) error {
	if from.IsZero() || !to.After(from) || to.Sub(from) > maxWindow {
		return fmt.Errorf("%w: a %s window ends after it starts and spans at most %s", domain.ErrValidation, report, reportSpan(maxWindow))
	}
	return nil
}

func reportSpan(window time.Duration) string {
	if window%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", window/(24*time.Hour))
	}
	return window.String()
}
