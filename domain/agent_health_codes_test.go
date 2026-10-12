package domain

import "testing"

func TestAgentAlertCodesAgreeWithSeedsAndChecks(t *testing.T) {
	for _, d := range []studioCodeDomain{
		{
			name: "agent_alert_kind", width: 1,
			codes: studioCodesOf(AlertFailureRate, AlertLatency, AlertSafety, AlertSilence, AlertUnresolved),
			seed:  "agent/agent.yml", schema: "agent/agent_alert.yml", check: "agent_alert_kind_ck", column: "kind_code",
		},
		{
			name: "agent_alert_status", width: 1,
			codes: studioCodesOf(AlertOpen, AlertResolved),
			seed:  "agent/agent.yml", schema: "agent/agent_alert.yml", check: "agent_alert_status_ck", column: "status_code",
		},
	} {
		t.Run(d.name, func(t *testing.T) {
			if seeded := studioSeededCodes(t, d.seed, d.name); !studioSameCodes(seeded, d.codes) {
				t.Errorf("seed values %v, Go constants %v", seeded, d.codes)
			}
			if checked := studioCheckedCodes(t, d.schema, d.check, d.column); !studioSameCodes(checked, d.codes) {
				t.Errorf("%s lists %v, Go constants %v", d.check, checked, d.codes)
			}
			if column := studioColumnType(t, d.schema, d.column); column != "CHAR(1)" {
				t.Errorf("%s is %s", d.column, column)
			}
		})
	}
	for _, kind := range []AgentAlertKind{AlertFailureRate, AlertLatency, AlertSafety, AlertSilence, AlertUnresolved} {
		if !kind.Valid() {
			t.Errorf("%q must be valid", kind)
		}
	}
	if AgentAlertKind("X").Valid() || AgentAlertStatus("X").Valid() || !AlertOpen.Valid() || !AlertResolved.Valid() {
		t.Fatal("alert code validity is wrong")
	}
}

func TestHealthMeasureJudgesOnlyFinishedTurns(t *testing.T) {
	measure := AgentHealthMeasure{TurnsByStatus: map[TurnStatus]int64{TurnCompleted: 7, TurnFailed: 3, TurnCancelled: 5, TurnRunning: 2}}
	if measure.Turns() != 17 || measure.Judged() != 10 {
		t.Fatalf("turns = %d, judged = %d", measure.Turns(), measure.Judged())
	}
	if rate, judged := measure.FailureRateBP(); !judged || rate != 3000 {
		t.Fatalf("rate = %d, %v", rate, judged)
	}
	if _, judged := (AgentHealthMeasure{TurnsByStatus: map[TurnStatus]int64{TurnCancelled: 4}}).FailureRateBP(); judged {
		t.Fatal("cancelled turns alone are no sample")
	}
	if (HealthAlertPolicy{}).SampleFloor() != 1 || (HealthAlertPolicy{MinTurns: 20}).SampleFloor() != 20 {
		t.Fatal("the sample floor is at least one turn")
	}
	if (HealthAlertPolicy{WindowMinutes: 60, MinTurns: 5}).Evaluates() || !(HealthAlertPolicy{MaxSilenceMinutes: 30}).Evaluates() {
		t.Fatal("only a threshold makes a policy evaluate")
	}
}
