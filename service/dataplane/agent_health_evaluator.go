package dataplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/domain"
)

// maxReportedHealthErrors bounds the failures one pass returns; the report counts every one.
const maxReportedHealthErrors = 16

// healthVerdict is one alert kind's outcome for an agent: open, resolve, or neither when the
// measure cannot decide it.
type healthVerdict struct {
	kind                        domain.AgentAlertKind
	open, resolve               bool
	observed, threshold, window int64
}

// EvaluateAll pages every active agent, measures each one whose effective health_alerts policy
// sets a threshold, and opens or resolves its alerts. An alert opens only on a measure that
// breaches its threshold and resolves only on a measure with at least the policy's sample floor
// that no longer does, so absent data never resolves a rate, latency, or safety alert. Each write
// names one tenant and agent, and replays and concurrent replicas cannot open an alert twice.
func (h *AgentHealth) EvaluateAll(ctx context.Context) (domain.HealthEvaluationReport, error) {
	settings, err := h.init(ctx)
	if err != nil {
		return domain.HealthEvaluationReport{}, err
	}
	var report domain.HealthEvaluationReport
	var failures []error
	fail := func(err error) {
		report.Failed++
		if len(failures) < maxReportedHealthErrors {
			failures = append(failures, err)
		}
	}
	tenantID, agentID := int64(0), ""
	for {
		page, err := h.qs.Query(ctx, qHealthAgentsPage, tenantID, tenantID, agentID, settings.Batch)
		if err != nil {
			return report, errors.Join(append(failures, fmt.Errorf("page active agents: %w", err))...)
		}
		for _, row := range page.Rows {
			if err = ctx.Err(); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
			if len(row) != 4 {
				return report, errors.Join(append(failures, fmt.Errorf("%w: active-agent row has %d columns", domain.ErrConflict, len(row)))...)
			}
			var tenantOK bool
			if tenantID, tenantOK = common.AsInt64OK(row[0]); !tenantOK || tenantID <= 0 {
				return report, errors.Join(append(failures, fmt.Errorf("%w: active-agent tenant does not decode", domain.ErrConflict))...)
			}
			var agentTypeID string
			agentID, agentTypeID = common.AsString(row[1]), common.AsString(row[2])
			hasOpen, openOK := common.AsInt64OK(row[3])
			if strings.TrimSpace(agentID) == "" || strings.TrimSpace(agentTypeID) == "" || !openOK || hasOpen < 0 || hasOpen > 1 {
				return report, errors.Join(append(failures, fmt.Errorf("%w: active-agent row does not decode", domain.ErrConflict))...)
			}
			undelivered, err := h.evaluate(ctx, settings, tenantID, agentID, agentTypeID, hasOpen == 1, &report)
			for _, delivery := range undelivered {
				report.Undelivered++
				if len(failures) < maxReportedHealthErrors {
					failures = append(failures, delivery)
				}
			}
			if err != nil {
				fail(fmt.Errorf("agent %d/%s: %w", tenantID, agentID, err))
			}
		}
		if len(page.Rows) < settings.Batch {
			break
		}
	}
	if report.Failed+report.Undelivered > len(failures) {
		failures = append(failures, fmt.Errorf("%d more health failures not shown", report.Failed+report.Undelivered-len(failures)))
	}
	return report, errors.Join(failures...)
}

// evaluate judges one agent and records its measurement; it returns the failed alert deliveries apart.
// Disabled thresholds are resolved only for an agent the page saw with an open alert.
func (h *AgentHealth) evaluate(ctx context.Context, settings domain.AgentHealthSettings, tenantID int64, agentID, agentTypeID string, hasOpen bool, report *domain.HealthEvaluationReport) ([]error, error) {
	policy, err := h.policy(ctx, tenantID, agentID, agentTypeID)
	if err != nil {
		return nil, err
	}
	if hasOpen {
		if err = h.resolveDisabled(ctx, tenantID, agentID, policy, report); err != nil {
			return nil, err
		}
	}
	if policy == nil || !policy.Evaluates() {
		report.Skipped++
		return nil, nil
	}
	window := cmp.Or(policy.WindowMinutes, settings.WindowMinutes)
	// The state row records the evaluation time by the database clock, so the measure needs none.
	measure, err := h.measure(ctx, tenantID, agentID, window, time.Time{})
	if err != nil {
		return nil, err
	}
	verdicts := judgeHealth(*policy, measure)
	if policy.MaxSilenceMinutes > 0 {
		silence, err := h.judgeSilence(ctx, tenantID, agentID, policy.MaxSilenceMinutes)
		if err != nil {
			return nil, err
		}
		verdicts = append(verdicts, silence)
	}
	var undelivered []error
	for _, verdict := range verdicts {
		switch {
		case verdict.open:
			alert, opened, err := h.openAlert(ctx, tenantID, agentID, verdict)
			if err != nil {
				return undelivered, err
			}
			if !opened {
				continue
			}
			report.Opened++
			if h.Notifier != nil {
				if err = h.Notifier.AlertOpened(ctx, alert); err != nil {
					undelivered = append(undelivered, fmt.Errorf("deliver alert %d of agent %d/%s: %w", alert.ID, tenantID, agentID, err))
				}
			}
		case verdict.resolve:
			resolved, err := h.qs.Query(ctx, qHealthResolve, tenantID, agentID, string(verdict.kind))
			if err != nil {
				return undelivered, fmt.Errorf("resolve %s alert: %w", verdict.kind, err)
			}
			report.Resolved += len(resolved.Rows)
		}
	}
	rate, judged := measure.FailureRateBP()
	var failureRate, p95 any
	if judged {
		failureRate = rate
	}
	if measure.DurationP95MS != nil {
		p95 = *measure.DurationP95MS
	}
	if _, err = h.qs.Query(ctx, qHealthSaveState, tenantID, agentID, window, measure.Turns(), measure.Judged(), failureRate, p95,
		measure.SafetyBySeverity[domain.GuardrailSeverityHard]); err != nil {
		return undelivered, fmt.Errorf("record health state: %w", err)
	}
	report.Evaluated++
	return undelivered, nil
}

func (h *AgentHealth) resolveDisabled(ctx context.Context, tenantID int64, agentID string, policy *domain.HealthAlertPolicy, report *domain.HealthEvaluationReport) error {
	enabled := func(kind domain.AgentAlertKind) int {
		if alertEnabled(policy, kind) {
			return 1
		}
		return 0
	}
	failure, latency := enabled(domain.AlertFailureRate), enabled(domain.AlertLatency)
	safety, silence := enabled(domain.AlertSafety), enabled(domain.AlertSilence)
	if failure+latency+safety+silence == 4 {
		return nil
	}
	resolved, err := h.qs.Query(ctx, qHealthResolveDisabled, tenantID, agentID, failure, latency, safety, silence)
	if err != nil {
		return fmt.Errorf("resolve disabled alerts: %w", err)
	}
	report.Resolved += len(resolved.Rows)
	return nil
}

func alertEnabled(policy *domain.HealthAlertPolicy, kind domain.AgentAlertKind) bool {
	if policy == nil {
		return false
	}
	switch kind {
	case domain.AlertFailureRate:
		return policy.MaxFailureRateBP > 0
	case domain.AlertLatency:
		return policy.MaxP95LatencyMS > 0
	case domain.AlertSafety:
		return policy.MaxHardSafetyEvents > 0
	case domain.AlertSilence:
		return policy.MaxSilenceMinutes > 0
	default:
		return false
	}
}

// judgeHealth decides the failure rate, latency, and hard safety alerts a measure supports.
func judgeHealth(policy domain.HealthAlertPolicy, measure domain.AgentHealthMeasure) []healthVerdict {
	floor, window := policy.SampleFloor(), measure.WindowMinutes
	enough := measure.Judged() >= floor
	var verdicts []healthVerdict
	if policy.MaxFailureRateBP > 0 && enough {
		rate, _ := measure.FailureRateBP()
		verdicts = append(verdicts, healthVerdict{kind: domain.AlertFailureRate, open: rate > policy.MaxFailureRateBP,
			resolve: rate <= policy.MaxFailureRateBP, observed: rate, threshold: policy.MaxFailureRateBP, window: window})
	}
	if policy.MaxP95LatencyMS > 0 && measure.TurnsByStatus[domain.TurnCompleted] >= floor && measure.DurationP95MS != nil {
		p95 := *measure.DurationP95MS
		verdicts = append(verdicts, healthVerdict{kind: domain.AlertLatency, open: p95 > policy.MaxP95LatencyMS,
			resolve: p95 <= policy.MaxP95LatencyMS, observed: p95, threshold: policy.MaxP95LatencyMS, window: window})
	}
	if policy.MaxHardSafetyEvents > 0 {
		hard := measure.SafetyBySeverity[domain.GuardrailSeverityHard]
		verdicts = append(verdicts, healthVerdict{kind: domain.AlertSafety, open: hard > policy.MaxHardSafetyEvents,
			resolve: hard <= policy.MaxHardSafetyEvents && enough, observed: hard, threshold: policy.MaxHardSafetyEvents, window: window})
	}
	return verdicts
}

// judgeSilence opens a silence alert for an agent with a stable release that has served for the
// whole span without a turn, and resolves it once a turn arrives; an agent without a stable
// release, or serving for less than the span, is not judged.
func (h *AgentHealth) judgeSilence(ctx context.Context, tenantID int64, agentID string, span int64) (healthVerdict, error) {
	verdict := healthVerdict{kind: domain.AlertSilence, threshold: span, window: span}
	result, err := h.qs.Query(ctx, qHealthSilence, span, span, tenantID, agentID)
	if err != nil {
		return verdict, fmt.Errorf("measure silence: %w", err)
	}
	if len(result.Rows) == 0 {
		return verdict, nil
	}
	row := result.Rows[0]
	if len(row) != 2 {
		return verdict, fmt.Errorf("%w: silence health row does not decode", domain.ErrConflict)
	}
	hasTurn, turnOK := common.AsInt64OK(row[0])
	serving, servingOK := common.AsInt64OK(row[1])
	if !turnOK || !servingOK || hasTurn < 0 || hasTurn > 1 || serving < 0 || serving > 1 {
		return verdict, fmt.Errorf("%w: silence health row does not decode", domain.ErrConflict)
	}
	if hasTurn == 1 {
		verdict.resolve = true
		return verdict, nil
	}
	verdict.open = serving == 1
	return verdict, nil
}

// openAlert inserts an open alert; false when one of its kind is already open.
func (h *AgentHealth) openAlert(ctx context.Context, tenantID int64, agentID string, verdict healthVerdict) (domain.AgentAlert, bool, error) {
	result, err := h.qs.Query(ctx, qHealthOpenAlert, tenantID, agentID, string(verdict.kind), verdict.observed, verdict.threshold, verdict.window)
	if err != nil {
		return domain.AgentAlert{}, false, fmt.Errorf("open %s alert: %w", verdict.kind, err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentAlert{}, false, nil
	}
	row := result.Rows[0]
	if len(row) != 2 {
		return domain.AgentAlert{}, false, fmt.Errorf("%w: opened alert row does not decode", domain.ErrConflict)
	}
	id, idOK := common.AsInt64OK(row[0])
	openedAt, openedOK := common.AsTimeOK(row[1])
	if !idOK || id <= 0 || !openedOK {
		return domain.AgentAlert{}, false, fmt.Errorf("%w: opened alert row does not decode", domain.ErrConflict)
	}
	alert := domain.AgentAlert{
		ID: id, TenantID: tenantID, AgentID: agentID, Kind: verdict.kind, Status: domain.AlertOpen,
		Observed: verdict.observed, Threshold: verdict.threshold, WindowMinutes: verdict.window,
		OpenedAt: openedAt,
	}
	return alert, true, nil
}
