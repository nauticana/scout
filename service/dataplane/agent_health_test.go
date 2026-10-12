package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var healthNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func healthBinding(scopeID, kind, value string) domain.ScopedBinding {
	return domain.ScopedBinding{ScopeID: scopeID, ResourceKind: domain.ResourceKind(kind), ResourceID: kind,
		MergeMode: domain.MergeReplace, Value: []byte(value)}
}

// healthService answers every tenant's agent chain tenant → t:writer → a:<agent> with the bindings
// bindings returns for that tenant.
func healthService(query *queueQueryFake, bindings func(tenantID int64) []domain.ScopedBinding) *AgentHealth {
	return &AgentHealth{
		DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView},
		Scopes: &fake.ScopeRepository{
			ChainFunc: func(_ context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
				return domain.ScopeChain{
					{TenantID: tenantID, ScopeID: "tenant", ScopeKind: "tenant"},
					{TenantID: tenantID, ScopeID: "t:writer", ParentScopeID: "tenant", ScopeKind: "agent_type"},
					{TenantID: tenantID, ScopeID: scopeID, ParentScopeID: "t:writer", ScopeKind: "agent"},
				}, nil
			},
			BindingsFunc: func(_ context.Context, tenantID int64, _ []string, _ time.Time) ([]domain.ScopedBinding, error) {
				return bindings(tenantID), nil
			},
		},
	}
}

func policyOf(value string) func(int64) []domain.ScopedBinding {
	return func(int64) []domain.ScopedBinding {
		// A broken binding of another kind proves the policy compiles apart from the other settings.
		return []domain.ScopedBinding{healthBinding("tenant", "health_alerts", value), healthBinding("a:writer-a", "limits", `{"max_steps":"x"}`)}
	}
}

func noPolicy(int64) []domain.ScopedBinding { return nil }

func turnRow(status domain.TurnStatus, count int64, p95 any, last any) []any {
	return []any{string(status), count, int64(800), p95, int64(150), int64(600), last}
}

func TestStudioHealthReportsExactRecordsOverThePolicyWindow(t *testing.T) {
	completed, failed := healthNow.Add(-time.Minute), healthNow.Add(-5*time.Minute)
	query := &queueQueryFake{rows: map[string][][]any{
		qHealthAgent: {{"writer", healthNow, healthNow.Add(-30 * time.Second), int64(1)}},
		qHealthTurns: {turnRow(domain.TurnCompleted, 18, int64(4000), completed), turnRow(domain.TurnFailed, 2, nil, failed),
			{string(domain.TurnRunning), int64(1), nil, nil, nil, nil, nil}},
		qHealthSafety:       {{"hard", int64(2)}, {"soft", int64(5)}},
		qHealthOpenAlerts:   {{int64(9), "F", "O", healthNow.Add(-time.Hour), nil, int64(1000), int64(500), int64(30)}},
		qHealthOpenOutcomes: {{int64(3)}},
	}}
	report, err := healthService(query, policyOf(`{"window_minutes":30,"min_turns":10,"max_failure_rate_bp":500}`)).
		Health(context.Background(), usageActor, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if args := query.firstArgs(qHealthTurns); args[0] != int64(7) || args[1] != "writer-a" || args[2] != int64(30) {
		t.Fatalf("turn args = %v, want the actor's tenant and the policy window", args)
	}
	if args := query.firstArgs(qHealthSafety); args[0] != int64(7) || args[2] != int64(30) {
		t.Fatalf("safety args = %v", args)
	}
	if args := query.firstArgs(qHealthOpenOutcomes); report.OpenOutcomes != 3 || len(args) != 2 || args[0] != int64(7) || args[1] != "writer-a" {
		t.Fatalf("open outcomes %d read with %v, want the actor's tenant and agent", report.OpenOutcomes, args)
	}
	m := report.Measure
	if m.WindowMinutes != 30 || !m.MeasuredAt.Equal(healthNow) || m.Turns() != 21 || m.Judged() != 20 || *m.DurationP95MS != 4000 ||
		*m.FirstResponseP50MS != 150 || !m.LastCompletedAt.Equal(completed) || !m.LastFailedAt.Equal(failed) ||
		m.SafetyBySeverity[domain.GuardrailSeverityHard] != 2 || m.SafetyBySeverity[domain.GuardrailSeveritySoft] != 5 {
		t.Fatalf("measure = %+v", m)
	}
	if rate, _ := m.FailureRateBP(); rate != 1000 || report.InsufficientSamples || report.Telemetry != domain.TelemetryFresh {
		t.Fatalf("rate = %d, report = %+v", rate, report)
	}
	if report.Policy == nil || *report.Policy != (domain.HealthAlertPolicy{WindowMinutes: 30, MinTurns: 10, MaxFailureRateBP: 500}) || report.PolicyProblem != "" {
		t.Fatalf("policy = %+v, problem %q", report.Policy, report.PolicyProblem)
	}
	if len(report.OpenAlerts) != 1 || report.OpenAlerts[0].Kind != domain.AlertFailureRate || report.OpenAlerts[0].TenantID != 7 ||
		!report.OpenAlerts[0].ResolvedAt.IsZero() || !report.LastEvaluatedAt.Equal(healthNow.Add(-30*time.Second)) {
		t.Fatalf("alerts = %+v, last evaluated %v", report.OpenAlerts, report.LastEvaluatedAt)
	}
}

func TestStudioHealthNeverReportsMissingOrStaleTelemetryAsFresh(t *testing.T) {
	policy := policyOf(`{"min_turns":10,"max_failure_rate_bp":500}`)
	for name, c := range map[string]struct {
		evaluated any
		active    int64
		turns     [][]any
		bindings  func(int64) []domain.ScopedBinding
		want      domain.TelemetryState
		few       bool
	}{
		"never evaluated":         {evaluated: nil, active: 1, turns: [][]any{turnRow(domain.TurnCompleted, 30, int64(1), healthNow)}, bindings: policy, want: domain.TelemetryStale},
		"evaluated too long ago":  {evaluated: healthNow.Add(-4 * time.Minute), active: 1, turns: [][]any{turnRow(domain.TurnCompleted, 30, int64(1), healthNow)}, bindings: policy, want: domain.TelemetryStale},
		"no turn in the window":   {evaluated: healthNow, active: 1, bindings: policy, want: domain.TelemetryMissing, few: true},
		"inactive agent":          {evaluated: nil, active: 0, bindings: policy, want: domain.TelemetryMissing, few: true},
		"too few samples":         {evaluated: healthNow, active: 1, turns: [][]any{turnRow(domain.TurnCompleted, 4, int64(1), healthNow)}, bindings: policy, want: domain.TelemetryFresh, few: true},
		"no policy owes no check": {evaluated: nil, active: 1, turns: [][]any{turnRow(domain.TurnFailed, 1, nil, healthNow)}, bindings: noPolicy, want: domain.TelemetryFresh},
	} {
		query := &queueQueryFake{rows: map[string][][]any{
			qHealthAgent: {{"writer", healthNow, c.evaluated, c.active}}, qHealthTurns: c.turns, qHealthOpenOutcomes: {{int64(0)}},
		}}
		report, err := healthService(query, c.bindings).Health(context.Background(), usageActor, "writer-a")
		if err != nil || report.Telemetry != c.want || report.InsufficientSamples != c.few {
			t.Errorf("%s: telemetry %q, insufficient %v, error %v", name, report.Telemetry, report.InsufficientSamples, err)
		}
	}
}

func TestStudioHealthReadsAsNotFoundWithoutViewAndReportsABrokenPolicy(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qHealthAgent: {{"writer", healthNow, nil, int64(1)}}, qHealthOpenOutcomes: {{int64(0)}}}}
	health := healthService(query, noPolicy)
	health.Authorizer = allowStudio{domain.StudioEdit}
	if _, err := health.Health(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an agent the actor cannot view must read as not found: %v", err)
	}
	if len(query.calls) != 0 {
		t.Fatalf("nothing is read before authorization: %v", query.calls)
	}
	if _, err := health.Alerts(context.Background(), usageActor, domain.AgentAlertFilter{AgentID: "writer-a"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alerts of an agent the actor cannot view must read as not found: %v", err)
	}
	empty := healthService(&queueQueryFake{rows: map[string][][]any{}}, noPolicy)
	if _, err := empty.Health(context.Background(), usageActor, "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a missing agent must be not found: %v", err)
	}
	broken := healthService(query, func(int64) []domain.ScopedBinding {
		return []domain.ScopedBinding{healthBinding("a:writer-a", "health_alerts", `{"max_failure_rate_bp":-1}`)}
	})
	report, err := broken.Health(context.Background(), usageActor, "writer-a")
	if err != nil || report.Policy != nil || !strings.Contains(report.PolicyProblem, "max_failure_rate_bp") || report.Measure.WindowMinutes != DefaultHealthWindowMinutes {
		t.Fatalf("a policy that does not compile is reported, not served: %+v, %v", report, err)
	}
}

func TestStudioAlertsPageNewestFirstWithinBounds(t *testing.T) {
	opened := healthNow.Add(-time.Hour)
	query := &queueQueryFake{rows: map[string][][]any{qHealthAlertPage: {
		{int64(30), "S", "R", opened, healthNow, int64(0), int64(60), int64(60)},
		{int64(20), "L", "O", opened, nil, int64(9000), int64(3000), int64(60)},
		{int64(10), "H", "O", opened, nil, int64(4), int64(1), int64(60)},
	}}}
	health := healthService(query, noPolicy)
	health.Settings.AlertPageSize = 10
	health.Settings.AlertMaxPageSize = 10
	page, err := health.Alerts(context.Background(), usageActor, domain.AgentAlertFilter{AgentID: "writer-a", Status: domain.AlertOpen, Before: 40, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Alerts) != 2 || page.NextBefore != 20 || !page.Alerts[0].ResolvedAt.Equal(healthNow) || page.Alerts[1].Kind != domain.AlertLatency {
		t.Fatalf("page = %+v", page)
	}
	if args := query.firstArgs(qHealthAlertPage); args[0] != int64(7) || args[1] != "writer-a" || args[2] != "O" || args[4] != int64(40) || args[6] != 3 {
		t.Fatalf("page args = %v", args)
	}
	if _, err = health.Alerts(context.Background(), usageActor, domain.AgentAlertFilter{AgentID: "writer-a"}); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qHealthAlertPage][1]; args[2] != "" || args[6] != 11 {
		t.Fatalf("default page args = %v", args)
	}
	for name, filter := range map[string]domain.AgentAlertFilter{
		"above max":       {AgentID: "writer-a", Limit: 11},
		"negative limit":  {AgentID: "writer-a", Limit: -1},
		"negative before": {AgentID: "writer-a", Before: -1},
		"unknown status":  {AgentID: "writer-a", Status: "X"},
	} {
		if _, err = health.Alerts(context.Background(), usageActor, filter); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
}

func evaluatorQuery(turns, safety, silence [][]any) *queueQueryFake {
	return &queueQueryFake{rows: map[string][][]any{
		qHealthAgentsPage: {{int64(7), "writer-a", "writer", int64(1)}},
		qHealthTurns:      turns, qHealthSafety: safety, qHealthSilence: silence,
		qHealthOpenAlert: {{int64(51), healthNow}},
		qHealthResolve:   {{int64(44)}},
	}}
}

func callsOf(query *queueQueryFake, name string) int { return len(query.args[name]) }

func TestEvaluateAllOpensBreachedAlertsOnceAndDeliversThem(t *testing.T) {
	query := evaluatorQuery([][]any{turnRow(domain.TurnCompleted, 18, int64(4000), healthNow), turnRow(domain.TurnFailed, 2, nil, healthNow)},
		[][]any{{"hard", int64(2)}}, [][]any{{int64(1), int64(1)}})
	health := healthService(query, policyOf(`{"min_turns":10,"max_failure_rate_bp":500,"max_p95_latency_ms":3000,"max_hard_safety_events":1,"max_silence_minutes":60}`))
	var delivered []domain.AgentAlert
	health.Notifier = fake.AgentAlertNotifierFunc(func(_ context.Context, alert domain.AgentAlert) error {
		delivered = append(delivered, alert)
		return nil
	})
	report, err := health.EvaluateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report != (domain.HealthEvaluationReport{Evaluated: 1, Opened: 3, Resolved: 1}) || len(delivered) != 3 {
		t.Fatalf("report = %+v, delivered %d", report, len(delivered))
	}
	opened := query.args[qHealthOpenAlert]
	want := [][]any{{int64(7), "writer-a", "F", int64(1000), int64(500), int64(60)}, {int64(7), "writer-a", "L", int64(4000), int64(3000), int64(60)},
		{int64(7), "writer-a", "H", int64(2), int64(1), int64(60)}}
	for index, args := range want {
		for column, value := range args {
			if opened[index][column] != value {
				t.Fatalf("open %d args = %v, want %v", index, opened[index], args)
			}
		}
	}
	if delivered[0].ID != 51 || delivered[0].TenantID != 7 || delivered[0].Status != domain.AlertOpen || !delivered[0].OpenedAt.Equal(healthNow) {
		t.Fatalf("delivered = %+v", delivered[0])
	}
	if args := query.firstArgs(qHealthResolve); args[0] != int64(7) || args[1] != "writer-a" || args[2] != "S" {
		t.Fatalf("a turn in the silence span resolves silence: %v", args)
	}
	if args := query.firstArgs(qHealthSilence); args[0] != int64(60) || args[1] != int64(60) || args[2] != int64(7) || args[3] != "writer-a" {
		t.Fatalf("silence args = %v", args)
	}
	state := query.firstArgs(qHealthSaveState)
	if state[0] != int64(7) || state[2] != int64(60) || state[3] != int64(20) || state[4] != int64(20) || state[5] != int64(1000) ||
		state[6] != int64(4000) || state[7] != int64(2) {
		t.Fatalf("state args = %v", state)
	}

	// A replay or another replica finds the alerts already open: nothing opens or is delivered twice.
	query.rows[qHealthOpenAlert] = nil
	delivered = nil
	if report, err = health.EvaluateAll(context.Background()); err != nil || report.Opened != 0 || len(delivered) != 0 {
		t.Fatalf("replay report = %+v, delivered %d, error %v", report, len(delivered), err)
	}
}

func TestEvaluateAllNeverResolvesOnAbsentOrScarceData(t *testing.T) {
	policy := policyOf(`{"min_turns":10,"max_failure_rate_bp":500,"max_p95_latency_ms":3000,"max_hard_safety_events":1,"max_silence_minutes":60}`)
	for name, turns := range map[string][][]any{
		"no turn":   nil,
		"few turns": {turnRow(domain.TurnCompleted, 3, nil, healthNow), turnRow(domain.TurnFailed, 3, nil, healthNow)},
	} {
		// Silent, but not yet serving for the whole span: silence is not judged either.
		query := evaluatorQuery(turns, nil, [][]any{{int64(0), int64(0)}})
		report, err := healthService(query, policy).EvaluateAll(context.Background())
		if err != nil || report != (domain.HealthEvaluationReport{Evaluated: 1}) {
			t.Errorf("%s: report = %+v, error = %v", name, report, err)
		}
		if callsOf(query, qHealthOpenAlert) != 0 || callsOf(query, qHealthResolve) != 0 {
			t.Errorf("%s: opened %d, resolved %d; scarce data decides nothing", name, callsOf(query, qHealthOpenAlert), callsOf(query, qHealthResolve))
		}
		if state := query.firstArgs(qHealthSaveState); len(state) != 8 || state[6] != nil {
			t.Errorf("%s: state = %v, want a null p95", name, state)
		}
	}
	healthy := evaluatorQuery([][]any{turnRow(domain.TurnCompleted, 20, int64(900), healthNow)}, nil, nil)
	report, err := healthService(healthy, policy).EvaluateAll(context.Background())
	if err != nil || report.Resolved != 3 || callsOf(healthy, qHealthOpenAlert) != 0 {
		t.Fatalf("enough healthy samples resolve rate, latency, and safety: %+v, %v", report, err)
	}
}

func TestEvaluateAllResolvesAlertsWhoseThresholdWasDisabled(t *testing.T) {
	query := evaluatorQuery([][]any{turnRow(domain.TurnCompleted, 2, int64(900), healthNow)}, nil, nil)
	query.rows[qHealthResolveDisabled] = [][]any{{int64(41)}, {int64(42)}}
	query.rows[qHealthResolve] = nil
	report, err := healthService(query, policyOf(`{"max_failure_rate_bp":500}`)).EvaluateAll(context.Background())
	if err != nil || report.Resolved != 2 || report.Evaluated != 1 {
		t.Fatalf("report = %+v, error = %v", report, err)
	}
	if args := query.firstArgs(qHealthResolveDisabled); len(args) != 7 || args[2] != 1 || args[3] != 0 || args[4] != 0 || args[5] != 0 || args[6] != 0 {
		t.Fatalf("disabled-alert args = %v", args)
	}

	withoutPolicy := evaluatorQuery(nil, nil, nil)
	withoutPolicy.rows[qHealthResolveDisabled] = [][]any{{int64(43)}}
	report, err = healthService(withoutPolicy, noPolicy).EvaluateAll(context.Background())
	if err != nil || report != (domain.HealthEvaluationReport{Skipped: 1, Resolved: 1}) {
		t.Fatalf("removed policy report = %+v, error = %v", report, err)
	}
}

func TestEvaluateAllSkipsDisabledResolutionWithoutAnOpenAlert(t *testing.T) {
	query := evaluatorQuery(nil, nil, nil)
	query.rows[qHealthAgentsPage] = [][]any{{int64(7), "writer-a", "writer", int64(0)}}
	report, err := healthService(query, noPolicy).EvaluateAll(context.Background())
	if err != nil || report != (domain.HealthEvaluationReport{Skipped: 1}) || callsOf(query, qHealthResolveDisabled) != 0 {
		t.Fatalf("report = %+v, error = %v, disabled resolves = %d", report, err, callsOf(query, qHealthResolveDisabled))
	}
}

func TestAgentHealthRejectsMalformedDatabaseRows(t *testing.T) {
	shortAgent := healthService(&queueQueryFake{rows: map[string][][]any{qHealthAgent: {{"writer"}}}}, noPolicy)
	if _, err := shortAgent.Health(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("short agent row = %v", err)
	}
	shortTurn := healthService(&queueQueryFake{rows: map[string][][]any{
		qHealthAgent: {{"writer", healthNow, nil, int64(1)}}, qHealthTurns: {{string(domain.TurnCompleted)}},
	}}, noPolicy)
	if _, err := shortTurn.Health(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("short turn row = %v", err)
	}
	shortPage := healthService(&queueQueryFake{rows: map[string][][]any{qHealthAgentsPage: {{int64(7)}}}}, noPolicy)
	if _, err := shortPage.EvaluateAll(context.Background()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("short active-agent row = %v", err)
	}
	shortSilence := evaluatorQuery(nil, nil, [][]any{{int64(0)}})
	if _, err := healthService(shortSilence, policyOf(`{"max_silence_minutes":45}`)).EvaluateAll(context.Background()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("short silence row = %v", err)
	}
	shortAlert := evaluatorQuery(nil, [][]any{{"hard", int64(3)}}, nil)
	shortAlert.rows[qHealthOpenAlert] = [][]any{{int64(51)}}
	if _, err := healthService(shortAlert, policyOf(`{"max_hard_safety_events":1}`)).EvaluateAll(context.Background()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("short opened-alert row = %v", err)
	}
}

func TestEvaluateAllOpensSilenceForAServingAgentWithoutTurns(t *testing.T) {
	query := evaluatorQuery(nil, nil, [][]any{{int64(0), int64(1)}})
	report, err := healthService(query, policyOf(`{"max_silence_minutes":45}`)).EvaluateAll(context.Background())
	if err != nil || report.Opened != 1 {
		t.Fatalf("report = %+v, error = %v", report, err)
	}
	if args := query.firstArgs(qHealthOpenAlert); args[2] != "S" || args[3] != int64(0) || args[4] != int64(45) || args[5] != int64(45) {
		t.Fatalf("silence alert args = %v", args)
	}
	unreleased := evaluatorQuery(nil, nil, nil)
	if report, err = healthService(unreleased, policyOf(`{"max_silence_minutes":45}`)).EvaluateAll(context.Background()); err != nil ||
		callsOf(unreleased, qHealthOpenAlert) != 0 || callsOf(unreleased, qHealthResolve) != 0 || report.Evaluated != 1 {
		t.Fatalf("an agent without a stable release is not judged silent: %+v, %v", report, err)
	}
}

func TestEvaluateAllKeepsAnUndeliveredAlertAndReportsIt(t *testing.T) {
	query := evaluatorQuery(nil, [][]any{{"hard", int64(3)}}, nil)
	health := healthService(query, policyOf(`{"max_hard_safety_events":1}`))
	health.Notifier = fake.AgentAlertNotifierFunc(func(context.Context, domain.AgentAlert) error { return errors.New("pager down") })
	report, err := health.EvaluateAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pager down") || !strings.Contains(err.Error(), "deliver alert 51") {
		t.Fatalf("a failed delivery must be returned: %v", err)
	}
	if report != (domain.HealthEvaluationReport{Evaluated: 1, Opened: 1, Undelivered: 1}) || callsOf(query, qHealthSaveState) != 1 {
		t.Fatalf("report = %+v; the alert row and the state stand", report)
	}
}

func TestEvaluateAllPagesTenantsAndKeepsEachAgentApart(t *testing.T) {
	query := &queueQueryFake{
		rows: map[string][][]any{},
		sequence: map[string][][][]any{qHealthAgentsPage: {
			{{int64(7), "a", "writer", int64(0)}}, {{int64(8), "b", "writer", int64(0)}}, {{int64(8), "c", "writer", int64(0)}}, {},
		}},
	}
	health := healthService(query, func(tenantID int64) []domain.ScopedBinding {
		if tenantID == 7 {
			return []domain.ScopedBinding{healthBinding("tenant", "health_alerts", `{"max_failure_rate_bp":"high"}`)}
		}
		return nil
	})
	health.Settings.Batch = 1
	report, err := health.EvaluateAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agent 7/a") || report != (domain.HealthEvaluationReport{Skipped: 2, Failed: 1}) {
		t.Fatalf("report = %+v, error = %v", report, err)
	}
	pages := query.args[qHealthAgentsPage]
	if len(pages) != 4 || pages[0][0] != int64(0) || pages[1][0] != int64(7) || pages[1][2] != "a" || pages[3][1] != int64(8) || pages[3][2] != "c" || pages[3][3] != 1 {
		t.Fatalf("page cursors = %v", pages)
	}
	if callsOf(query, qHealthTurns) != 0 || callsOf(query, qHealthSaveState) != 0 {
		t.Fatal("an agent without a policy is not measured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := evaluatorQuery(nil, nil, nil)
	if _, err = healthService(cancelled, policyOf(`{"max_failure_rate_bp":5}`)).EvaluateAll(ctx); !errors.Is(err, context.Canceled) || callsOf(cancelled, qHealthTurns) != 0 {
		t.Fatalf("a cancelled pass stops: %v", err)
	}
	negative := healthService(&queueQueryFake{}, noPolicy)
	negative.Settings.Batch = -1
	if _, err = negative.EvaluateAll(context.Background()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative settings are refused: %v", err)
	}
}
