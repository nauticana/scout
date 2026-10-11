package observability

import (
	"context"
	"encoding/json"
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
	qSafetyEventInsert = "scout_safety_event_insert"
	qSafetyEventPage   = "scout_safety_event_page"

	// MaxSafetyEventPage bounds one page of safety events.
	MaxSafetyEventPage = 500
)

var safetyEventQueries = map[string]string{
	qSafetyEventInsert: `
INSERT INTO safety_event
       (id, partner_id, principal_kind, principal_id, agent_id, request_id, stage_code, layer_code, action_code,
        severity_code, rule_ids, release_version, policy_version, duration_ms, occurred_at)
VALUES (nextval('safety_event_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qSafetyEventPage: `
SELECT id, principal_kind, principal_id, request_id, stage_code, layer_code, action_code, severity_code,
       rule_ids, release_version, policy_version, duration_ms, occurred_at
  FROM safety_event
 WHERE partner_id = ? AND agent_id = ? AND (? = 0 OR id < ?)
 ORDER BY id DESC
 LIMIT ?`,
}

// TableSafetyEventSink records guardrail interventions in safety_event and pages them per
// agent. It keeps rule ids and attribution only; the inspected content is never stored.
type TableSafetyEventSink struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

func (sink *TableSafetyEventSink) Record(ctx context.Context, event domain.SafetyEvent) error {
	if sink.DB == nil {
		return fmt.Errorf("safety event sink: database is required")
	}
	if event.TenantID <= 0 || event.Stage == "" || event.Layer == "" || event.Action == "" || event.Severity == "" || event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: a safety event needs tenant, stage, layer, action, severity, and time", domain.ErrValidation)
	}
	ruleIDs, err := json.Marshal(append([]string{}, event.RuleIDs...))
	if err != nil {
		return fmt.Errorf("encode safety event rules: %w", err)
	}
	sink.once.Do(func() { sink.qs = sink.DB.GetQueryService(ctx, safetyEventQueries) })
	if _, err = sink.qs.Query(context.WithoutCancel(ctx), qSafetyEventInsert,
		event.TenantID, nullableText(string(event.Principal.Kind)), nullableText(event.Principal.ID),
		nullableText(event.AgentID), nullableText(event.RequestID), string(event.Stage), string(event.Layer), string(event.Action), string(event.Severity),
		string(ruleIDs), nullableText(event.ReleaseVersion), nullableText(event.PolicyVersion),
		event.Duration.Milliseconds(), event.OccurredAt.UTC()); err != nil {
		return fmt.Errorf("record safety event: %w", err)
	}
	return nil
}

// SafetyEvents returns up to filter.Limit of the agent's events with ids below filter.Before
// (every id when zero), newest first.
func (sink *TableSafetyEventSink) SafetyEvents(ctx context.Context, tenantID int64, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error) {
	if sink.DB == nil {
		return nil, fmt.Errorf("safety event sink: database is required")
	}
	if tenantID <= 0 || strings.TrimSpace(filter.AgentID) == "" {
		return nil, fmt.Errorf("%w: tenant and agent are required", domain.ErrValidation)
	}
	if filter.Limit < 1 || filter.Limit > MaxSafetyEventPage || filter.Before < 0 {
		return nil, fmt.Errorf("%w: limit must be within 1..%d and before non-negative", domain.ErrValidation, MaxSafetyEventPage)
	}
	sink.once.Do(func() { sink.qs = sink.DB.GetQueryService(ctx, safetyEventQueries) })
	result, err := sink.qs.Query(ctx, qSafetyEventPage, tenantID, filter.AgentID, filter.Before, filter.Before, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("page safety events: %w", err)
	}
	events := make([]domain.SafetyEvent, 0, len(result.Rows))
	for _, row := range result.Rows {
		event, err := safetyEventFromRow(tenantID, filter.AgentID, row)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func safetyEventFromRow(tenantID int64, agentID string, row []any) (domain.SafetyEvent, error) {
	if len(row) != 13 {
		return domain.SafetyEvent{}, fmt.Errorf("%w: safety event row has %d columns", domain.ErrConflict, len(row))
	}
	var ruleIDs []string
	if err := json.Unmarshal([]byte(common.AsString(row[8])), &ruleIDs); err != nil {
		return domain.SafetyEvent{}, fmt.Errorf("%w: safety event rule ids do not decode: %v", domain.ErrConflict, err)
	}
	occurredAt, ok := common.AsTimeOK(row[12])
	if !ok {
		return domain.SafetyEvent{}, fmt.Errorf("%w: safety event time does not decode", domain.ErrConflict)
	}
	return domain.SafetyEvent{
		ID: common.AsInt64(row[0]), TenantID: tenantID, AgentID: agentID,
		Principal: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[1])), ID: common.AsString(row[2])},
		RequestID: common.AsString(row[3]), Stage: domain.GuardrailStage(common.AsString(row[4])),
		Layer: domain.GuardrailLayer(common.AsString(row[5])), Action: domain.GuardrailAction(common.AsString(row[6])),
		Severity: domain.GuardrailSeverity(common.AsString(row[7])), RuleIDs: ruleIDs,
		ReleaseVersion: common.AsString(row[9]), PolicyVersion: common.AsString(row[10]),
		Duration: time.Duration(common.AsInt64(row[11])) * time.Millisecond, OccurredAt: occurredAt,
	}, nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var (
	_ contract.SafetyEventSink  = (*TableSafetyEventSink)(nil)
	_ contract.SafetyEventQuery = (*TableSafetyEventSink)(nil)
)
