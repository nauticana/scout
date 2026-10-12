package dataplane

import (
	"context"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

// Decision 17: while any turn of the agent has been held for the policy's span with an open call
// outcome, an unresolved-outcome alert opens, once; it resolves when no turn is, from an exact
// count read afresh, so no sample floor is waited for.
func TestEvaluateAllAlertsOnOutcomesLeftUnresolved(t *testing.T) {
	policy := policyOf(`{"min_turns":10,"max_unresolved_minutes":45}`)
	query := evaluatorQuery(nil, nil, nil)
	query.rows[qHealthUnresolved] = [][]any{{int64(2)}}
	report, err := healthService(query, policy).EvaluateAll(context.Background())
	if err != nil || report.Opened != 1 || report.Evaluated != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	opened := query.firstArgs(qHealthOpenAlert)
	if opened[2] != string(domain.AlertUnresolved) || opened[3] != int64(2) || opened[4] != int64(45) || opened[5] != int64(45) {
		t.Fatalf("opened = %v, want kind U, two turns, the 45-minute span", opened)
	}
	if args := query.firstArgs(qHealthUnresolved); args[0] != int64(7) || args[1] != "writer-a" || args[2] != int64(45) {
		t.Fatalf("unresolved args = %v, want the agent's tenant and the span", args)
	}

	// The insert-if-absent open makes a second pass, or another replica, open nothing more.
	query.rows[qHealthOpenAlert] = nil
	if report, err = healthService(query, policy).EvaluateAll(context.Background()); err != nil || report.Opened != 0 {
		t.Fatalf("a second pass = %+v, %v", report, err)
	}

	resolved := evaluatorQuery(nil, nil, nil)
	resolved.rows[qHealthUnresolved] = [][]any{{int64(0)}}
	if report, err := healthService(resolved, policy).EvaluateAll(context.Background()); err != nil || report.Resolved != 1 {
		t.Fatalf("no unresolved turn = %+v, %v", report, err)
	}
	if args := resolved.firstArgs(qHealthResolve); args[2] != string(domain.AlertUnresolved) {
		t.Fatalf("resolve args = %v", args)
	}
	sql := healthQueries[qHealthUnresolved]
	for _, part := range []string{"t.status_code = " + sqlTurnHeld, "t.held_at <= CURRENT_TIMESTAMP - make_interval(mins => ?)",
		"o.status_code IN (" + sqlCallOpen + ")", "t.partner_id = ? AND c.agent_id = ?"} {
		if !strings.Contains(sql, part) {
			t.Errorf("unresolved count lacks %q", part)
		}
	}
	if sql := healthQueries[qHealthOpenOutcomes]; !strings.Contains(sql, "o.partner_id = ? AND c.agent_id = ?") || !strings.Contains(sql, sqlCallOpen) {
		t.Errorf("open outcomes count = %s", sql)
	}
}

// Disabling the unresolved span resolves its open alert like any other disabled threshold.
func TestEvaluateAllResolvesADisabledUnresolvedAlert(t *testing.T) {
	query := evaluatorQuery([][]any{turnRow(domain.TurnCompleted, 2, int64(900), healthNow)}, nil, nil)
	query.rows[qHealthResolveDisabled] = [][]any{{int64(45)}}
	query.rows[qHealthResolve] = nil
	query.rows[qHealthUnresolved] = [][]any{{int64(0)}}
	if _, err := healthService(query, policyOf(`{"max_unresolved_minutes":30}`)).EvaluateAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if args := query.firstArgs(qHealthResolveDisabled); args[6] != 1 || args[2] != 0 {
		t.Fatalf("an enabled unresolved span must keep its alert: %v", args)
	}
	if !strings.Contains(healthQueries[qHealthResolveDisabled], "kind_code = "+sqlAlertUnresolved) {
		t.Fatal("disabled resolution must cover the unresolved kind")
	}
}
