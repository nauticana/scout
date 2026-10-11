package domain

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// evaluationCodeDomain ties a code list's Go constants to its seeded constant_value rows and
// to the CHECK and column type of the table that stores it.
type evaluationCodeDomain struct {
	constantID string
	codes      []string
	table      string
	column     string
	check      string
}

func evaluationCodeDomains() []evaluationCodeDomain {
	return []evaluationCodeDomain{
		{"gate_decision_verdict", []string{string(GateVerdictPass), string(GateVerdictFail), string(GateVerdictOverride)},
			"agent_gate_decision", "verdict_code", "agent_gate_decision_verdict_ck"},
		{"gate_decision_run_role", []string{string(GateRunCandidate), string(GateRunBaseline)},
			"agent_gate_decision_run", "role_code", "agent_gate_decision_run_role_ck"},
		{"review_source", []string{string(ReviewSourceSampled), string(ReviewSourceFeedback)},
			"human_review_item", "source_code", "human_review_item_source_ck"},
		{"review_status", []string{string(ReviewPending), string(ReviewGraded), string(ReviewDismissed), string(ReviewPromoted)},
			"human_review_item", "status_code", "human_review_item_status_ck"},
		{"review_verdict", []string{string(ReviewPass), string(ReviewFail)},
			"human_review_item", "verdict_code", "human_review_item_verdict_ck"},
		{"evaluation_comparison_verdict", []string{string(ComparisonPrefersA), string(ComparisonPrefersB), string(ComparisonTie)},
			"evaluation_comparison", "verdict_code", "evaluation_comparison_verdict_ck"},
	}
}

func TestEvaluationCodesMatchTheSeedAndTheChecks(t *testing.T) {
	seed := readEvaluationSchema(t, "../schema/seed/evaluation/evaluation.yml")
	_, values, found := strings.Cut(seed, "- table: constant_value\n")
	if !found {
		t.Fatal("seed has no constant_value rows")
	}
	values, _, _ = strings.Cut(values, "- table:")
	for _, code := range evaluationCodeDomains() {
		t.Run(code.constantID, func(t *testing.T) {
			for _, value := range code.codes {
				if len(value) != 1 {
					t.Errorf("code %q is not one character", value)
				}
			}
			valueRow := regexp.MustCompile(`(?m)^\s*- \[` + code.constantID + `, ([^,\]]+), ([^\]]+)\]`)
			var seeded []string
			for _, match := range valueRow.FindAllStringSubmatch(values, -1) {
				seeded = append(seeded, match[1])
				if caption := strings.TrimSpace(match[2]); caption == "" || len(caption) > 30 {
					t.Errorf("caption %q of %s is empty or longer than 30 characters", caption, match[1])
				}
			}
			if !sameCodes(seeded, code.codes) {
				t.Errorf("seeded values %v, constants %v", seeded, code.codes)
			}
			if lookup := "- [" + code.constantID + ", " + code.table + ", " + code.column + "]"; !strings.Contains(seed, lookup) {
				t.Errorf("seed has no constant_lookup row %q", lookup)
			}

			table := readEvaluationSchema(t, "../schema/evaluation/"+code.table+".yml")
			columnType := regexp.MustCompile(`(?m)^  - name: ` + code.column + `\n    type: (\S+)`).FindStringSubmatch(table)
			if columnType == nil || columnType[1] != "CHAR(1)" {
				t.Errorf("column %s.%s type = %v, want CHAR(1)", code.table, code.column, columnType)
			}
			check := regexp.MustCompile(`(?m)^  - name: ` + code.check + `\n    expression: .*` + code.column + ` IN \(([^)]*)\)`).FindStringSubmatch(table)
			if check == nil {
				t.Fatalf("table %s has no check %s listing %s", code.table, code.check, code.column)
			}
			var checked []string
			for _, literal := range strings.Split(check[1], ",") {
				checked = append(checked, strings.Trim(strings.TrimSpace(literal), "'"))
			}
			if !sameCodes(checked, code.codes) {
				t.Errorf("check %s lists %v, constants %v", code.check, checked, code.codes)
			}
		})
	}
}

func readEvaluationSchema(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func sameCodes(got, want []string) bool {
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}
