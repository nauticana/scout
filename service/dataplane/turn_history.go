package dataplane

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

const (
	qTurnHistory = "scout_turn_history"

	// DefaultTurnHistoryPageSize and DefaultTurnHistoryMaxPageSize bound one page of a person's turns.
	DefaultTurnHistoryPageSize    = 50
	DefaultTurnHistoryMaxPageSize = 200
	maxTurnHistoryPageSize        = 1000
)

var turnHistoryQueries = map[string]string{
	qTurnHistory: `
SELECT t.request_id, t.conversation_id, c.agent_id, c.agent_version, t.status_code, COALESCE(t.work_ref, ''),
       COALESCE(d.task_kind, ''), COALESCE(d.input_summary, ''), COALESCE(d.error_text, ''),
       COALESCE(CAST(d.result_payload AS TEXT), ''), t.queued_at, t.completed_at,
       COALESCE(u.cost, 0), COALESCE(u.currency, ''), COALESCE(cur.exponent, 0), u.currencies
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
  LEFT JOIN conversation_turn_detail d ON d.partner_id = t.partner_id AND d.conversation_id = t.conversation_id AND d.turn_no = t.turn_no
  LEFT JOIN LATERAL (
       SELECT CAST(SUM(e.cost_minor_units) AS BIGINT) AS cost, MIN(e.currency_code) AS currency, COUNT(DISTINCT e.currency_code) AS currencies
         FROM usage_event e
        WHERE e.partner_id = t.partner_id AND e.conversation_id = t.conversation_id AND e.turn_no = t.turn_no) u ON TRUE
  LEFT JOIN currency cur ON cur.code = u.currency
 WHERE t.partner_id = ? AND c.end_user_ref = ? AND (? = '' OR t.work_ref = ?)
   AND (? = '' OR t.queued_at < ? OR t.queued_at = ? AND t.request_id < ?)
 ORDER BY t.queued_at DESC, t.request_id DESC
 LIMIT ?`,
}

// TurnHistory pages the turns of the conversations a person opened, with each turn's settled
// cost. The person is the conversation's end user; nobody reads another person's turns.
type TurnHistory struct {
	DB port.DatabaseRepository
	// PageSize is the page a caller naming no limit gets and MaxPageSize the largest it may ask
	// for; zero takes DefaultTurnHistoryPageSize and DefaultTurnHistoryMaxPageSize.
	PageSize    int
	MaxPageSize int

	once sync.Once
	qs   port.QueryService
}

var _ contract.TurnHistory = (*TurnHistory)(nil)

func (h *TurnHistory) Turns(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, filter domain.TurnHistoryFilter) (domain.TurnHistoryPage, error) {
	if h.DB == nil {
		return domain.TurnHistoryPage{}, fmt.Errorf("%w: turn history needs a database", domain.ErrNotReady)
	}
	pageSize, maxPageSize := cmp.Or(h.PageSize, DefaultTurnHistoryPageSize), cmp.Or(h.MaxPageSize, DefaultTurnHistoryMaxPageSize)
	if h.PageSize < 0 || h.MaxPageSize < 0 || maxPageSize > maxTurnHistoryPageSize || pageSize > maxPageSize {
		return domain.TurnHistoryPage{}, fmt.Errorf("%w: turn history page size %d must lie within 1..%d and its maximum within 1..%d",
			domain.ErrNotReady, pageSize, maxPageSize, maxTurnHistoryPageSize)
	}
	switch {
	case tenant.TenantID <= 0 || person.Kind != domain.PrincipalHuman || strings.TrimSpace(person.ID) == "":
		return domain.TurnHistoryPage{}, fmt.Errorf("%w: turn history needs a tenant and a person", domain.ErrForbidden)
	case filter.Limit < 0 || filter.Limit > maxPageSize:
		return domain.TurnHistoryPage{}, fmt.Errorf("%w: limit must lie within 0..%d", domain.ErrValidation, maxPageSize)
	case len(filter.WorkRef) > 200:
		return domain.TurnHistoryPage{}, fmt.Errorf("%w: work_ref is at most 200 bytes", domain.ErrValidation)
	}
	beforeAt, beforeRequest, err := decodeTurnCursor(filter.Before)
	if err != nil {
		return domain.TurnHistoryPage{}, err
	}
	h.once.Do(func() { h.qs = h.DB.GetQueryService(ctx, turnHistoryQueries) })
	limit := cmp.Or(filter.Limit, pageSize)
	result, err := h.qs.Query(ctx, qTurnHistory, tenant.TenantID, person.ID, filter.WorkRef, filter.WorkRef,
		filter.Before, beforeAt, beforeAt, beforeRequest, limit+1)
	if err != nil {
		return domain.TurnHistoryPage{}, fmt.Errorf("list turns: %w", err)
	}
	page := domain.TurnHistoryPage{Turns: make([]domain.TurnSummary, 0, min(len(result.Rows), limit))}
	for index, row := range result.Rows {
		if index == limit {
			last := page.Turns[limit-1]
			page.NextBefore = encodeTurnCursor(last.QueuedAt, last.RequestID)
			break
		}
		if common.AsInt64(row[15]) > 1 {
			return domain.TurnHistoryPage{}, fmt.Errorf("turn %s settled in more than one currency", common.AsString(row[0]))
		}
		turn := domain.TurnSummary{
			RequestID: common.AsString(row[0]), ConversationID: common.AsString(row[1]),
			AgentID: common.AsString(row[2]), AgentVersion: common.AsString(row[3]),
			Status: domain.TurnStatus(common.AsString(row[4])), WorkRef: common.AsString(row[5]),
			TaskKind: common.AsString(row[6]), InputSummary: common.AsString(row[7]), ErrorText: clientTurnError(common.AsString(row[8])),
			QueuedAt: common.AsTime(row[10]), CompletedAt: common.AsTime(row[11]),
			CostMinorUnits: common.AsInt64(row[12]), Currency: strings.TrimSpace(common.AsString(row[13])), Exponent: int(common.AsInt64(row[14])),
		}
		if payload := common.AsString(row[9]); payload != "" {
			turn.Result = json.RawMessage(payload)
		}
		page.Turns = append(page.Turns, turn)
	}
	return page, nil
}

// clientTurnError keeps the codes the runtime records, such as a rejected or expired approval,
// and hides any other stored failure text.
func clientTurnError(stored string) string {
	switch stored {
	case "", string(domain.ApprovalStatusRejected), string(domain.ApprovalStatusExpired), turnSubscribeFailed, turnDispatchFailed:
		return stored
	}
	if stage.KnownErrorClass(stored) {
		return stored
	}
	return stage.ErrorClassInternal
}
