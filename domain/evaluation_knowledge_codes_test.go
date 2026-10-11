package domain

import (
	"slices"
	"strings"
	"testing"
)

// Evaluation run, role, and golden scope codes and knowledge binding and source codes agree
// with their seeded constant_value rows and with every CHECK and column that stores them.
func TestEvaluationAndKnowledgeCodesAgreeWithSeedAndSchema(t *testing.T) {
	lists := []codeList{
		{
			constantID: "evaluation_run_status",
			codes:      codes(EvaluationQueued, EvaluationRunning, EvaluationCompleted, EvaluationStoppedEarly, EvaluationFailed),
			valid:      func(code string) bool { return EvaluationRunStatus(code).Valid() },
			seed:       "evaluation/evaluation.yml", schema: "evaluation/evaluation_run.yml",
			column: "status_code", columnType: "CHAR(1)", check: "evaluation_run_status_ck",
		},
		{
			constantID: "evaluation_role",
			codes:      codes(RoleBaseline, RoleCandidate),
			valid:      func(code string) bool { return EvaluationRole(code).Valid() },
			seed:       "evaluation/evaluation.yml", schema: "evaluation/evaluation_run_subject.yml",
			column: "role_code", columnType: "CHAR(1)", check: "evaluation_run_subject_role_ck",
		},
		{
			constantID: "evaluation_role",
			codes:      codes(RoleBaseline, RoleCandidate),
			valid:      func(code string) bool { return EvaluationRole(code).Valid() },
			seed:       "evaluation/evaluation.yml", schema: "evaluation/evaluation_result.yml",
			column: "role_code", columnType: "CHAR(1)", check: "evaluation_result_role_ck",
		},
		{
			constantID: "knowledge_binding_mode",
			codes:      codes(KnowledgeModeRetrieval, KnowledgeModeWhole),
			valid:      func(code string) bool { return KnowledgeBindingMode(code).Valid() },
			seed:       "knowledge/knowledge.yml", schema: "knowledge/agent_knowledge_binding.yml",
			column: "mode_code", columnType: "CHAR(1)", check: "agent_knowledge_binding_mode_ck",
		},
		{
			constantID: "knowledge_source_op",
			codes:      codes(SourceUpserted, SourceDeleted),
			valid:      func(code string) bool { return SourceChangeOp(code).Valid() },
			seed:       "knowledge/knowledge.yml", schema: "knowledge/knowledge_source_event.yml",
			column: "op_code", columnType: "CHAR(1)", check: "knowledge_source_event_op_ck",
		},
	}
	// Golden scope codes stay words: authored evaluation_gate policies name them.
	for _, table := range []string{"evaluation_run", "evaluation_baseline", "golden_case", "golden_example", "golden_query"} {
		lists = append(lists, codeList{
			constantID: "golden_scope",
			codes:      codes(GoldenScopeDev, GoldenScopeGate),
			valid:      func(code string) bool { return GoldenScope(code).Valid() },
			seed:       "evaluation/evaluation.yml", schema: "evaluation/" + table + ".yml",
			column: "scope_code", columnType: "VARCHAR(10)", check: table + "_scope_ck",
		})
	}
	for _, list := range lists {
		t.Run(list.constantID+"/"+list.schema, func(t *testing.T) {
			for _, code := range list.codes {
				if !list.valid(code) {
					t.Errorf("code %q is not valid", code)
				}
			}
			for _, word := range []string{"", strings.ToLower(list.codes[0]) + "x", list.codes[0] + " ", "queued", "baseline", "whole", "upsert"} {
				if list.valid(word) {
					t.Errorf("%q must not be valid", word)
				}
			}
			want := sortedCopy(list.codes)
			if seeded := seededCodes(t, list.seed, list.constantID); !slices.Equal(seeded, want) {
				t.Errorf("seed codes = %v, want %v", seeded, want)
			}
			if checked := checkedCodes(t, list.schema, list.check); !slices.Equal(checked, want) {
				t.Errorf("%s codes = %v, want %v", list.check, checked, want)
			}
			if got := yamlFieldAfter(t, list.schema, "- name: "+list.column, "type:"); got != list.columnType {
				t.Errorf("%s type = %q, want %q", list.column, got, list.columnType)
			}
			table := strings.TrimSuffix(list.schema[strings.LastIndex(list.schema, "/")+1:], ".yml")
			lookup := "- [" + list.constantID + ", " + table + ", " + list.column + "]"
			if !slices.ContainsFunc(readSchemaFile(t, "seed/"+list.seed), func(line string) bool { return strings.TrimSpace(line) == lookup }) {
				t.Errorf("seed has no constant_lookup row %q", lookup)
			}
		})
	}
}
