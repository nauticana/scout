package domain

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// studioCodeDomain ties one keel constant domain to its Go constants, its seed file, and the
// CHECK that constrains the referencing column.
type studioCodeDomain struct {
	name   string
	codes  []string
	width  int
	seed   string
	schema string
	check  string
	column string
}

func TestStudioCodesAgreeWithSeedsAndChecks(t *testing.T) {
	domains := []studioCodeDomain{
		{
			name: "publish_review_status", width: 1,
			codes: studioCodesOf(PublishReviewRequested, PublishReviewApproved, PublishReviewRejected, PublishReviewSuperseded),
			seed:  "agent/agent.yml", schema: "agent/agent_publish_review.yml",
			check: "agent_publish_review_status_ck", column: "status_code",
		},
		{
			name: "draft_comment_anchor", width: 1,
			codes: studioCodesOf(CommentOnDraft, CommentOnPromptSection, CommentOnSetting),
			seed:  "agent/agent.yml", schema: "agent/agent_draft_comment.yml",
			check: "agent_draft_comment_anchor_ck", column: "anchor_code",
		},
		{
			name: "draft_comment_status", width: 1,
			codes: studioCodesOf(DraftCommentOpen, DraftCommentResolved),
			seed:  "agent/agent.yml", schema: "agent/agent_draft_comment.yml",
			check: "agent_draft_comment_status_ck", column: "status_code",
		},
		{
			name: "release_activation_reason", width: 2,
			codes: studioCodesOf(ActivationVersionMissing, ActivationDigestMismatch, ActivationNotAuthorized, ActivationAgentArchived,
				ActivationGuardRefused, ActivationDeploymentChanged, ActivationAttemptsExhausted),
			seed: "agent/agent.yml", schema: "agent/agent_release_activation.yml",
			check: "agent_release_activation_reason_code_ck", column: "reason_code",
		},
		{
			name: "guardrail_layer", width: 1,
			codes: studioCodesOf(GuardrailLayerBaseline, GuardrailLayerPlatform, GuardrailLayerTenant, GuardrailLayerType, GuardrailLayerRelease),
			seed:  "agent/agent.yml", schema: "agent/safety_event.yml",
			check: "safety_event_layer_ck", column: "layer_code",
		},
		{
			name: "config_merge_mode", width: 1,
			codes: studioCodesOf(MergeReplace, MergeAppend, MergeIntersect),
			seed:  "configuration/configuration.yml", schema: "configuration/config_scope_binding.yml",
			check: "config_scope_binding_merge_mode_ck", column: "merge_mode_code",
		},
	}
	for _, d := range domains {
		t.Run(d.name, func(t *testing.T) {
			for _, code := range d.codes {
				if len(code) != d.width {
					t.Errorf("code %q is not %d characters", code, d.width)
				}
			}
			if seeded := studioSeededCodes(t, d.seed, d.name); !studioSameCodes(seeded, d.codes) {
				t.Errorf("seed values %v, Go constants %v", seeded, d.codes)
			}
			if checked := studioCheckedCodes(t, d.schema, d.check, d.column); !studioSameCodes(checked, d.codes) {
				t.Errorf("%s lists %v, Go constants %v", d.check, checked, d.codes)
			}
			if column := studioColumnType(t, d.schema, d.column); column != "CHAR("+string(rune('0'+d.width))+")" {
				t.Errorf("%s is %s", d.column, column)
			}
		})
	}
}

func studioCodesOf[T ~string](values ...T) []string {
	codes := make([]string, len(values))
	for i, value := range values {
		codes[i] = string(value)
	}
	return codes
}

func studioSameCodes(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func studioReadSchema(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile("../schema/" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// studioSeededCodes reads the constant_value rows of one domain: "- [domain, CODE, Caption]".
func studioSeededCodes(t *testing.T, seed, name string) []string {
	t.Helper()
	values := studioReadSchema(t, "seed/"+seed)
	if start := strings.Index(values, "- table: constant_value"); start >= 0 {
		values = values[start+1:]
		if end := strings.Index(values, "- table:"); end >= 0 {
			values = values[:end]
		}
	}
	row := regexp.MustCompile(`(?m)^\s*- \[` + regexp.QuoteMeta(name) + `, "?([^",\]]+)"?, [^\]]+\]$`)
	var codes []string
	for _, match := range row.FindAllStringSubmatch(values, -1) {
		codes = append(codes, match[1])
	}
	return codes
}

// studioCheckedCodes reads the first "column IN (...)" list of the named check.
func studioCheckedCodes(t *testing.T, schema, check, column string) []string {
	t.Helper()
	block := regexp.MustCompile(`(?m)^\s*- name: ` + regexp.QuoteMeta(check) + `\n\s*expression: (.*)$`).FindStringSubmatch(studioReadSchema(t, schema))
	if block == nil {
		t.Fatalf("%s has no check %s", schema, check)
	}
	list := regexp.MustCompile(`\b` + regexp.QuoteMeta(column) + ` IN \(([^)]*)\)`).FindStringSubmatch(block[1])
	if list == nil {
		return studioCheckedEqualities(block[1], column)
	}
	var codes []string
	for _, item := range strings.Split(list[1], ",") {
		codes = append(codes, strings.Trim(strings.TrimSpace(item), "'"))
	}
	return codes
}

// studioCheckedEqualities reads a check written as one "column = 'X'" branch per code.
func studioCheckedEqualities(expression, column string) []string {
	var codes []string
	for _, match := range regexp.MustCompile(`\b`+regexp.QuoteMeta(column)+` = '([^']*)'`).FindAllStringSubmatch(expression, -1) {
		codes = append(codes, match[1])
	}
	return codes
}

func studioColumnType(t *testing.T, schema, column string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^\s*- name: ` + regexp.QuoteMeta(column) + `\n\s*type: (\S+)$`).FindStringSubmatch(studioReadSchema(t, schema))
	if match == nil {
		t.Fatalf("%s has no column %s", schema, column)
	}
	return match[1]
}
