package dataplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
	"github.com/nauticana/scout/service/scope"
)

// Defaults of AgentHealthSettings, taken for each zero field.
const (
	DefaultHealthPeriod        = time.Minute
	DefaultHealthBatch         = 100
	DefaultHealthWindowMinutes = 60
	DefaultHealthStalePeriods  = 3
	DefaultAlertPageSize       = 50
	DefaultAlertMaxPageSize    = 200
	maxHealthPeriod            = 24 * time.Hour
	maxHealthBatch             = 10000
	maxHealthStalePeriods      = 1000
	maxAlertPageSize           = 1000
)

const (
	qHealthAgent           = "scout_health_agent"
	qHealthTurns           = "scout_health_turns"
	qHealthSafety          = "scout_health_safety"
	qHealthOpenAlerts      = "scout_health_open_alerts"
	qHealthAlertPage       = "scout_health_alert_page"
	qHealthAgentsPage      = "scout_health_agents_page"
	qHealthSilence         = "scout_health_silence"
	qHealthOpenAlert       = "scout_health_open_alert"
	qHealthResolve         = "scout_health_resolve_alert"
	qHealthResolveDisabled = "scout_health_resolve_disabled_alerts"
	qHealthSaveState       = "scout_health_save_state"
	qHealthOpenOutcomes    = "scout_health_open_outcomes"
	qHealthUnresolved      = "scout_health_unresolved"

	healthAlertColumns = `id, kind_code, status_code, opened_at, resolved_at, observed_value, threshold_value, window_minutes`
	// healthTurnsOfAgent joins an agent's turns through their conversations; its window bound is the last argument.
	healthTurnsOfAgent = `
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND c.agent_id = ? AND t.queued_at > CURRENT_TIMESTAMP - make_interval(mins => ?)`
)

var healthQueries = map[string]string{
	// The agent's type, the database clock, its last health evaluation, and whether it is active.
	qHealthAgent: `
SELECT p.agent_type_id, CURRENT_TIMESTAMP, s.evaluated_at, CASE WHEN p.state_code = ` + sqlAgentActive + ` THEN 1 ELSE 0 END
  FROM agent_profile p
  LEFT JOIN agent_health_state s ON s.partner_id = p.partner_id AND s.agent_id = p.agent_id
 WHERE p.partner_id = ? AND p.agent_id = ?`,
	// Per status, the turns queued in the window, and over each status's turns the duration and
	// time to first response percentiles from queueing and the latest completion.
	qHealthTurns: `
SELECT t.status_code, COUNT(*),
       CAST(ROUND(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.completed_at - t.queued_at)) * 1000)) AS BIGINT),
       CAST(ROUND(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.completed_at - t.queued_at)) * 1000)) AS BIGINT),
       CAST(ROUND(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.first_response_at - t.queued_at)) * 1000)) AS BIGINT),
       CAST(ROUND(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.first_response_at - t.queued_at)) * 1000)) AS BIGINT),
       MAX(t.completed_at)` + healthTurnsOfAgent + `
 GROUP BY t.status_code`,
	qHealthSafety: `
SELECT severity_code, COUNT(*)
  FROM safety_event
 WHERE partner_id = ? AND agent_id = ? AND occurred_at > CURRENT_TIMESTAMP - make_interval(mins => ?)
 GROUP BY severity_code`,
	qHealthOpenAlerts: `
SELECT ` + healthAlertColumns + `
  FROM agent_alert
 WHERE partner_id = ? AND agent_id = ? AND status_code = ` + sqlAlertOpen + `
 ORDER BY id DESC`,
	qHealthAlertPage: `
SELECT ` + healthAlertColumns + `
  FROM agent_alert
 WHERE partner_id = ? AND agent_id = ? AND (? = '' OR status_code = ?) AND (? = 0 OR id < ?)
 ORDER BY id DESC
 LIMIT ?`,
	// Active agents after the (tenant, agent) cursor.
	qHealthAgentsPage: `
SELECT p.partner_id, p.agent_id, p.agent_type_id,
       CASE WHEN EXISTS (SELECT 1 FROM agent_alert a
                          WHERE a.partner_id = p.partner_id AND a.agent_id = p.agent_id AND a.status_code = ` + sqlAlertOpen + `) THEN 1 ELSE 0 END
  FROM agent_profile p
 WHERE p.state_code = ` + sqlAgentActive + ` AND (p.partner_id > ? OR (p.partner_id = ? AND p.agent_id > ?))
 ORDER BY p.partner_id, p.agent_id
 LIMIT ?`,
	// For an agent with a stable release: whether a turn was queued in the silence span, and
	// whether it has been serving, active with its current traffic policy, for at least that span.
	qHealthSilence: `
SELECT CASE WHEN EXISTS (SELECT 1
                           FROM conversation_turn t
                           JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
                          WHERE t.partner_id = p.partner_id AND c.agent_id = p.agent_id
                            AND t.queued_at > CURRENT_TIMESTAMP - make_interval(mins => ?)) THEN 1 ELSE 0 END,
       CASE WHEN GREATEST(COALESCE(p.state_changed_at, p.created_at), d.updated_at) <= CURRENT_TIMESTAMP - make_interval(mins => ?)
            THEN 1 ELSE 0 END
  FROM agent_profile p
  JOIN agent_deployment d ON d.partner_id = p.partner_id AND d.agent_id = p.agent_id
 WHERE p.partner_id = ? AND p.agent_id = ?`,
	// The open-alert unique index makes a replica's or a replay's second open a no-op.
	qHealthOpenAlert: `
INSERT INTO agent_alert (id, partner_id, agent_id, kind_code, status_code, opened_at, observed_value, threshold_value, window_minutes)
VALUES (nextval('agent_alert_seq'), ?, ?, ?, ` + sqlAlertOpen + `, CURRENT_TIMESTAMP, ?, ?, ?)
ON CONFLICT (partner_id, agent_id, kind_code) WHERE status_code = ` + sqlAlertOpen + ` DO NOTHING
RETURNING id, opened_at`,
	qHealthResolve: `
UPDATE agent_alert
   SET status_code = ` + sqlAlertResolved + `, resolved_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND kind_code = ? AND status_code = ` + sqlAlertOpen + `
RETURNING id`,
	qHealthResolveDisabled: `
UPDATE agent_alert
   SET status_code = ` + sqlAlertResolved + `, resolved_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND status_code = ` + sqlAlertOpen + `
   AND ((kind_code = ` + sqlAlertFailure + ` AND ? = 0) OR (kind_code = ` + sqlAlertLatency + ` AND ? = 0)
     OR (kind_code = ` + sqlAlertSafety + ` AND ? = 0) OR (kind_code = ` + sqlAlertSilence + ` AND ? = 0)
     OR (kind_code = ` + sqlAlertUnresolved + ` AND ? = 0))
RETURNING id`,
	// Every open call of the agent's turns, whenever the turn was queued.
	qHealthOpenOutcomes: `
SELECT COUNT(*)
  FROM turn_call_outcome o
  JOIN conversation_turn t ON t.partner_id = o.partner_id AND t.request_id = o.request_id
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE o.partner_id = ? AND c.agent_id = ? AND o.status_code IN (` + sqlCallOpen + `)`,
	// The agent's turns held, or operated by a person, at least the span with a call whose outcome is still open.
	qHealthUnresolved: `
SELECT COUNT(*)
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND c.agent_id = ? AND t.status_code IN (` + sqlTurnHeldOrOperated + `)
   AND t.held_at <= CURRENT_TIMESTAMP - make_interval(mins => ?)
   AND EXISTS (SELECT 1 FROM turn_call_outcome o
                WHERE o.partner_id = t.partner_id AND o.request_id = t.request_id AND o.status_code IN (` + sqlCallOpen + `))`,
	qHealthSaveState: `
INSERT INTO agent_health_state (partner_id, agent_id, evaluated_at, window_minutes, turns, judged_turns, failure_rate_bp, p95_latency_ms, hard_safety_events)
VALUES (?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, agent_id) DO UPDATE
   SET evaluated_at = EXCLUDED.evaluated_at, window_minutes = EXCLUDED.window_minutes, turns = EXCLUDED.turns,
       judged_turns = EXCLUDED.judged_turns, failure_rate_bp = EXCLUDED.failure_rate_bp,
       p95_latency_ms = EXCLUDED.p95_latency_ms, hard_safety_events = EXCLUDED.hard_safety_events`,
}

// AgentHealth measures agents' production health from exact records — conversation_turn through
// agent_conversation, and safety_event — over each agent's health_alerts window. It serves
// Studio's health detail and alert list, and EvaluateAll opens and resolves agent alerts.
type AgentHealth struct {
	DB port.DatabaseRepository
	// Authorizer is required for the Studio views; the evaluator does not use it.
	Authorizer contract.StudioAuthorizer
	// Scopes reads the scope tables when nil.
	Scopes contract.ScopeRepository
	// Layout is optional; nil uses controlplane.BasePromptScopeLayout.
	Layout contract.PromptScopeLayout
	// Mergers is optional; nil registers the platform mergers.
	Mergers       contract.ResourceMergerRegistry
	MaxScopeDepth int
	// Notifier delivers each newly opened alert; nil delivers nothing.
	Notifier contract.AgentAlertNotifier
	Settings domain.AgentHealthSettings

	once sync.Once
	qs   port.QueryService
}

var _ contract.StudioHealthBackend = (*AgentHealth)(nil)

func (h *AgentHealth) init(ctx context.Context) (domain.AgentHealthSettings, error) {
	if h.DB == nil {
		return domain.AgentHealthSettings{}, fmt.Errorf("agent health: database is required")
	}
	settings := h.Settings
	if settings.Period < 0 || settings.Period > maxHealthPeriod || settings.Batch < 0 || settings.Batch > maxHealthBatch ||
		settings.WindowMinutes < 0 || settings.WindowMinutes > domain.MaxHealthWindowMinutes ||
		settings.StalePeriods < 0 || settings.StalePeriods > maxHealthStalePeriods ||
		settings.AlertPageSize < 0 || settings.AlertPageSize > maxAlertPageSize ||
		settings.AlertMaxPageSize < 0 || settings.AlertMaxPageSize > maxAlertPageSize {
		return domain.AgentHealthSettings{}, fmt.Errorf("%w: agent health settings are outside their supported range", domain.ErrValidation)
	}
	if settings.Period > 0 && settings.Period < time.Second {
		return domain.AgentHealthSettings{}, fmt.Errorf("%w: agent health period must be at least one second", domain.ErrValidation)
	}
	settings.Period = cmp.Or(settings.Period, DefaultHealthPeriod)
	settings.Batch = cmp.Or(settings.Batch, DefaultHealthBatch)
	settings.WindowMinutes = cmp.Or(settings.WindowMinutes, DefaultHealthWindowMinutes)
	settings.StalePeriods = cmp.Or(settings.StalePeriods, DefaultHealthStalePeriods)
	settings.AlertPageSize = cmp.Or(settings.AlertPageSize, DefaultAlertPageSize)
	settings.AlertMaxPageSize = cmp.Or(settings.AlertMaxPageSize, DefaultAlertMaxPageSize)
	if settings.AlertPageSize > settings.AlertMaxPageSize {
		return domain.AgentHealthSettings{}, fmt.Errorf("%w: alert page size %d exceeds maximum %d", domain.ErrValidation, settings.AlertPageSize, settings.AlertMaxPageSize)
	}
	h.once.Do(func() { h.qs = h.DB.GetQueryService(ctx, healthQueries) })
	if h.qs == nil {
		return domain.AgentHealthSettings{}, fmt.Errorf("agent health: query service is required")
	}
	return settings, nil
}

// Health reports one agent's health over its policy window; an agent the actor cannot VIEW is not found.
func (h *AgentHealth) Health(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentHealthReport, error) {
	settings, err := h.init(ctx)
	if err != nil {
		return domain.AgentHealthReport{}, err
	}
	if err = h.authorizeView(ctx, actor, agentID); err != nil {
		return domain.AgentHealthReport{}, err
	}
	agent, err := h.qs.Query(ctx, qHealthAgent, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentHealthReport{}, fmt.Errorf("read agent %q: %w", agentID, err)
	}
	if len(agent.Rows) == 0 {
		return domain.AgentHealthReport{}, fmt.Errorf("%w: agent %q", domain.ErrNotFound, agentID)
	}
	row := agent.Rows[0]
	if len(row) != 4 {
		return domain.AgentHealthReport{}, fmt.Errorf("%w: agent health row has %d columns", domain.ErrConflict, len(row))
	}
	now, nowOK := common.AsTimeOK(row[1])
	lastEvaluated, lastOK := common.AsTimeOK(row[2])
	active, activeOK := common.AsInt64OK(row[3])
	if strings.TrimSpace(common.AsString(row[0])) == "" || !nowOK || row[2] != nil && !lastOK || !activeOK || active < 0 || active > 1 {
		return domain.AgentHealthReport{}, fmt.Errorf("%w: agent health row does not decode", domain.ErrConflict)
	}
	report := domain.AgentHealthReport{AgentID: agentID}
	report.LastEvaluatedAt = lastEvaluated
	report.Policy, err = h.policy(ctx, actor.TenantID, agentID, common.AsString(row[0]))
	if policyProblem(err) {
		report.PolicyProblem = err.Error()
	} else if err != nil {
		return domain.AgentHealthReport{}, err
	}
	policy := domain.HealthAlertPolicy{}
	if report.Policy != nil {
		policy = *report.Policy
	}
	if report.Measure, err = h.measure(ctx, actor.TenantID, agentID, cmp.Or(policy.WindowMinutes, settings.WindowMinutes), now); err != nil {
		return domain.AgentHealthReport{}, err
	}
	report.InsufficientSamples = report.Measure.Judged() < policy.SampleFloor()
	stale := time.Duration(settings.StalePeriods) * settings.Period
	switch {
	case policy.Evaluates() && active == 1 && (report.LastEvaluatedAt.IsZero() || now.Sub(report.LastEvaluatedAt) > stale):
		report.Telemetry = domain.TelemetryStale
	case report.Measure.Turns() == 0:
		report.Telemetry = domain.TelemetryMissing
	default:
		report.Telemetry = domain.TelemetryFresh
	}
	if report.OpenOutcomes, err = h.count(ctx, qHealthOpenOutcomes, actor.TenantID, agentID); err != nil {
		return domain.AgentHealthReport{}, fmt.Errorf("count open call outcomes of %q: %w", agentID, err)
	}
	open, err := h.qs.Query(ctx, qHealthOpenAlerts, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentHealthReport{}, fmt.Errorf("read open alerts of %q: %w", agentID, err)
	}
	if report.OpenAlerts, err = alertsFromRows(actor.TenantID, agentID, open.Rows); err != nil {
		return domain.AgentHealthReport{}, err
	}
	return report, nil
}

// Alerts pages one agent's alerts newest first.
func (h *AgentHealth) Alerts(ctx context.Context, actor domain.StudioActor, filter domain.AgentAlertFilter) (domain.AgentAlertPage, error) {
	settings, err := h.init(ctx)
	if err != nil {
		return domain.AgentAlertPage{}, err
	}
	if filter.Limit < 0 || filter.Limit > settings.AlertMaxPageSize || filter.Before < 0 {
		return domain.AgentAlertPage{}, fmt.Errorf("%w: limit must be within 0..%d and before non-negative", domain.ErrValidation, settings.AlertMaxPageSize)
	}
	if filter.Status != "" && !filter.Status.Valid() {
		return domain.AgentAlertPage{}, fmt.Errorf("%w: status must be %s or %s", domain.ErrValidation, domain.AlertOpen, domain.AlertResolved)
	}
	if err = h.authorizeView(ctx, actor, filter.AgentID); err != nil {
		return domain.AgentAlertPage{}, err
	}
	limit := cmp.Or(filter.Limit, settings.AlertPageSize)
	status := string(filter.Status)
	result, err := h.qs.Query(ctx, qHealthAlertPage, actor.TenantID, filter.AgentID, status, status, filter.Before, filter.Before, limit+1)
	if err != nil {
		return domain.AgentAlertPage{}, fmt.Errorf("page alerts of %q: %w", filter.AgentID, err)
	}
	alerts, err := alertsFromRows(actor.TenantID, filter.AgentID, result.Rows)
	if err != nil {
		return domain.AgentAlertPage{}, err
	}
	page := domain.AgentAlertPage{Alerts: alerts}
	if len(alerts) > limit {
		page.Alerts = alerts[:limit]
		page.NextBefore = alerts[limit-1].ID
	}
	return page, nil
}

func (h *AgentHealth) authorizeView(ctx context.Context, actor domain.StudioActor, agentID string) error {
	if h.Authorizer == nil {
		return fmt.Errorf("agent health: a Studio authorizer is required")
	}
	if actor.TenantID <= 0 {
		return fmt.Errorf("%w: a Studio actor needs a tenant", domain.ErrUnauthorized)
	}
	err := h.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID)
	if errors.Is(err, domain.ErrForbidden) {
		return fmt.Errorf("%w: agent %q", domain.ErrNotFound, agentID)
	}
	return err
}

// policy compiles only the health_alerts bindings on the agent's scope chain; nil when none is
// bound or the agent has no scope chain.
func (h *AgentHealth) policy(ctx context.Context, tenantID int64, agentID, agentTypeID string) (*domain.HealthAlertPolicy, error) {
	layout := h.Layout
	if layout == nil {
		layout = controlplane.BasePromptScopeLayout{}
	}
	scopes, err := layout.Scopes(ctx, tenantID, agentID, agentTypeID)
	if err != nil {
		return nil, err
	}
	var repository contract.ScopeRepository = h.Scopes
	if repository == nil {
		repository = &scope.TableScopeRepository{DB: h.DB}
	}
	mergers := h.Mergers
	if mergers == nil {
		if mergers, err = scope.NewMergerRegistry(); err != nil {
			return nil, err
		}
	}
	compiler, err := scope.NewCompiler(kindScopes{ScopeRepository: repository, kind: domain.ResourceHealthAlerts}, mergers, &scope.LatticeChecker{}, h.MaxScopeDepth)
	if err != nil {
		return nil, err
	}
	release, err := compiler.Compile(ctx, domain.CompileRequest{
		TenantID: tenantID, AgentID: agentID, AgentVersion: string(domain.ResourceHealthAlerts), ScopeID: scopes.AgentScopeID,
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(release.Resources) > 1 {
		return nil, fmt.Errorf("%w: setting kind %q has more than one resource", domain.ErrValidation, domain.ResourceHealthAlerts)
	}
	value := scope.HealthAlertsValue(release.Resources)
	if value == nil {
		return nil, nil
	}
	policy, err := scope.DecodeHealthAlerts(value)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

// measure reads the agent's turns and safety events queued or occurring in the last window minutes.
func (h *AgentHealth) measure(ctx context.Context, tenantID int64, agentID string, window int64, now time.Time) (domain.AgentHealthMeasure, error) {
	measure := domain.AgentHealthMeasure{
		WindowMinutes: window, MeasuredAt: now,
		TurnsByStatus: map[domain.TurnStatus]int64{}, SafetyBySeverity: map[domain.GuardrailSeverity]int64{},
	}
	turns, err := h.qs.Query(ctx, qHealthTurns, tenantID, agentID, window)
	if err != nil {
		return domain.AgentHealthMeasure{}, fmt.Errorf("measure turns of %q: %w", agentID, err)
	}
	for _, row := range turns.Rows {
		if len(row) != 7 {
			return domain.AgentHealthMeasure{}, fmt.Errorf("%w: turn health row has %d columns", domain.ErrConflict, len(row))
		}
		status := domain.TurnStatus(common.AsString(row[0]))
		count, ok := common.AsInt64OK(row[1])
		if !status.Valid() || !ok || count < 0 {
			return domain.AgentHealthMeasure{}, fmt.Errorf("%w: turn health row %v does not decode", domain.ErrConflict, row[:2])
		}
		measure.TurnsByStatus[status] = count
		last, lastOK := common.AsTimeOK(row[6])
		if row[6] != nil && !lastOK {
			return domain.AgentHealthMeasure{}, fmt.Errorf("%w: turn health time does not decode", domain.ErrConflict)
		}
		switch status {
		case domain.TurnCompleted:
			values := []*int64{}
			for _, raw := range row[2:6] {
				value, valid := nullableMS(raw)
				if !valid {
					return domain.AgentHealthMeasure{}, fmt.Errorf("%w: turn health percentile does not decode", domain.ErrConflict)
				}
				values = append(values, value)
			}
			measure.DurationP50MS, measure.DurationP95MS = values[0], values[1]
			measure.FirstResponseP50MS, measure.FirstResponseP95MS = values[2], values[3]
			measure.LastCompletedAt = last
		case domain.TurnFailed:
			measure.LastFailedAt = last
		}
	}
	safety, err := h.qs.Query(ctx, qHealthSafety, tenantID, agentID, window)
	if err != nil {
		return domain.AgentHealthMeasure{}, fmt.Errorf("measure safety events of %q: %w", agentID, err)
	}
	for _, row := range safety.Rows {
		if len(row) != 2 {
			return domain.AgentHealthMeasure{}, fmt.Errorf("%w: safety health row has %d columns", domain.ErrConflict, len(row))
		}
		severity := domain.GuardrailSeverity(common.AsString(row[0]))
		count, ok := common.AsInt64OK(row[1])
		if !ok || count < 0 || severity != domain.GuardrailSeverityHard && severity != domain.GuardrailSeveritySoft {
			return domain.AgentHealthMeasure{}, fmt.Errorf("%w: safety health row %v does not decode", domain.ErrConflict, row)
		}
		measure.SafetyBySeverity[severity] = count
	}
	return measure, nil
}

// count reads one non-negative count.
func (h *AgentHealth) count(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := h.qs.Query(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		return 0, fmt.Errorf("%w: a count returned %d rows", domain.ErrConflict, len(result.Rows))
	}
	count, ok := common.AsInt64OK(result.Rows[0][0])
	if !ok || count < 0 {
		return 0, fmt.Errorf("%w: a count does not decode", domain.ErrConflict)
	}
	return count, nil
}

func nullableMS(value any) (*int64, bool) {
	if value == nil {
		return nil, true
	}
	ms, ok := common.AsInt64OK(value)
	if !ok || ms < 0 {
		return nil, false
	}
	return &ms, true
}

func alertsFromRows(tenantID int64, agentID string, rows [][]any) ([]domain.AgentAlert, error) {
	alerts := make([]domain.AgentAlert, 0, len(rows))
	for _, row := range rows {
		if len(row) != 8 {
			return nil, fmt.Errorf("%w: alert row has %d columns", domain.ErrConflict, len(row))
		}
		id, idOK := common.AsInt64OK(row[0])
		observed, observedOK := common.AsInt64OK(row[5])
		threshold, thresholdOK := common.AsInt64OK(row[6])
		window, windowOK := common.AsInt64OK(row[7])
		alert := domain.AgentAlert{
			ID: id, TenantID: tenantID, AgentID: agentID,
			Kind: domain.AgentAlertKind(common.AsString(row[1])), Status: domain.AgentAlertStatus(common.AsString(row[2])),
			Observed: observed, Threshold: threshold, WindowMinutes: window,
		}
		var ok bool
		if alert.OpenedAt, ok = common.AsTimeOK(row[3]); !ok || !idOK || !observedOK || !thresholdOK || !windowOK || alert.ID <= 0 || !alert.Kind.Valid() || !alert.Status.Valid() ||
			alert.Observed < 0 || alert.Threshold < 0 || alert.WindowMinutes <= 0 {
			return nil, fmt.Errorf("%w: alert %d does not decode", domain.ErrConflict, alert.ID)
		}
		resolved, resolvedOK := common.AsTimeOK(row[4])
		if alert.Status == domain.AlertResolved && (!resolvedOK || resolved.Before(alert.OpenedAt)) || alert.Status == domain.AlertOpen && resolvedOK {
			return nil, fmt.Errorf("%w: alert %d has an invalid resolution", domain.ErrConflict, alert.ID)
		}
		alert.ResolvedAt = resolved
		alerts = append(alerts, alert)
	}
	return alerts, nil
}

// policyProblem reports a scope chain that does not compile because of what it binds.
func policyProblem(err error) bool {
	return errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrAuthorityExceeded) || errors.Is(err, domain.ErrSealed)
}

// kindScopes answers only one kind's bindings, so a health policy compiles apart from the
// agent's other settings.
type kindScopes struct {
	contract.ScopeRepository
	kind domain.ResourceKind
}

func (k kindScopes) Bindings(ctx context.Context, tenantID int64, scopeIDs []string, asOf time.Time) ([]domain.ScopedBinding, error) {
	bindings, err := k.ScopeRepository.Bindings(ctx, tenantID, scopeIDs, asOf)
	if err != nil {
		return nil, err
	}
	kept := bindings[:0:0]
	for _, binding := range bindings {
		if binding.ResourceKind == k.kind {
			kept = append(kept, binding)
		}
	}
	return kept, nil
}
