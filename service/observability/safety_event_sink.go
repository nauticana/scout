package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qSafetyEventInsert = "scout_safety_event_insert"

var safetyEventQueries = map[string]string{
	qSafetyEventInsert: `
INSERT INTO safety_event
       (id, tenant_id, principal_kind, principal_id, stage_code, layer_code, action_code, severity_code,
        rule_ids, release_version, policy_version, duration_ms, occurred_at)
VALUES (nextval('safety_event_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
}

// TableSafetyEventSink records guardrail interventions in safety_event. It keeps
// rule ids and attribution only; the inspected content is never stored.
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
		string(event.Stage), string(event.Layer), string(event.Action), string(event.Severity),
		string(ruleIDs), nullableText(event.ReleaseVersion), nullableText(event.PolicyVersion),
		event.Duration.Milliseconds(), event.OccurredAt.UTC()); err != nil {
		return fmt.Errorf("record safety event: %w", err)
	}
	return nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ contract.SafetyEventSink = (*TableSafetyEventSink)(nil)
