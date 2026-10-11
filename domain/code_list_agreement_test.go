package domain

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// codeList is one closed constant domain: its Go codes, the seed that captions them,
// and the column and CHECK that store them.
type codeList struct {
	constantID string
	codes      []string
	valid      func(string) bool
	seed       string
	schema     string
	column     string
	columnType string
	check      string
}

func TestCodeListsAgreeWithSeedAndSchema(t *testing.T) {
	lists := []codeList{
		{
			constantID: "agent_output_class",
			codes:      codes(OutputAdvice, OutputDraft, OutputApprovalRequest, OutputCompletedAction),
			valid:      func(code string) bool { return OutputClass(code).Valid() },
			seed:       "approval/approval.yml", schema: "approval/turn_approval_request.yml",
			column: "output_class_code", columnType: "CHAR(1)", check: "turn_approval_request_output_class_ck",
		},
		{
			constantID: "mcp_confirmation_channel",
			codes:      codes(MCPConfirmationElicitation, MCPConfirmationInbox),
			valid:      func(code string) bool { return MCPConfirmationChannel(code).Valid() },
			seed:       "mcp_confirmation/mcp_confirmation.yml", schema: "mcp_confirmation/mcp_confirmation.yml",
			column: "channel_code", columnType: "CHAR(1)", check: "mcp_confirmation_channel_ck",
		},
		{
			constantID: "execution_step_kind",
			codes:      []string{StepKindModel, StepKindTool, StepKindKnowledge, StepKindAgent, StepKindWait, StepKindToolLoop},
			valid:      ValidStepKind,
			seed:       "execution_graph/execution_graph.yml", schema: "execution_graph/execution_step.yml",
			column: "step_kind_code", columnType: "CHAR(1)", check: "execution_step_kind_ck",
		},
		{
			constantID: "model_capability",
			codes: codes(CapabilityText, CapabilityImage, CapabilityVideo, CapabilityTools, CapabilityStructuredOutput,
				CapabilitySampling, CapabilityWebSearch, CapabilityEmbeddings),
			valid: func(code string) bool { return ModelCapability(code).Valid() },
			seed:  "model/model.yml", schema: "model/model_capability.yml",
			column: "capability_code", columnType: "CHAR(2)", check: "model_capability_code_ck",
		},
		{
			constantID: "capacity_class",
			codes:      codes(CapacityShared, CapacityDedicated),
			valid:      func(code string) bool { return CapacityClass(code).Valid() },
			seed:       "tenancy/tenancy.yml", schema: "tenancy/tenant_runtime_policy.yml",
			column: "capacity_class_code", columnType: "CHAR(1)", check: "tenant_runtime_policy_capacity_ck",
		},
	}
	for _, list := range lists {
		t.Run(list.constantID, func(t *testing.T) {
			width := len(list.codes[0])
			for _, code := range list.codes {
				if len(code) != width || !list.valid(code) {
					t.Errorf("code %q is not a valid %d-character code", code, width)
				}
			}
			for _, word := range []string{"", strings.ToLower(list.codes[0]), list.codes[0] + " "} {
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
		})
	}
}

func codes[T ~string](values ...T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}

func sortedCopy(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func readSchemaFile(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile("../schema/" + path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(content), "\n")
}

// seededCodes returns the constant_value codes the seed file lists for one constant.
func seededCodes(t *testing.T, seed, constantID string) []string {
	t.Helper()
	var out []string
	inValues := false
	for _, line := range readSchemaFile(t, "seed/"+seed) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- table:") {
			inValues = trimmed == "- table: constant_value"
			continue
		}
		prefix := "- [" + constantID + ", "
		if inValues && strings.HasPrefix(trimmed, prefix) {
			out = append(out, strings.TrimSpace(strings.SplitN(strings.TrimPrefix(trimmed, prefix), ",", 2)[0]))
		}
	}
	slices.Sort(out)
	return out
}

var quotedCode = regexp.MustCompile(`'([^']*)'`)

// checkedCodes returns the quoted literals of a named CHECK expression.
func checkedCodes(t *testing.T, schema, check string) []string {
	t.Helper()
	var out []string
	for _, match := range quotedCode.FindAllStringSubmatch(yamlFieldAfter(t, schema, "- name: "+check, "expression:"), -1) {
		out = append(out, match[1])
	}
	slices.Sort(out)
	return out
}

// yamlFieldAfter returns the value of the first field line after the anchor line.
func yamlFieldAfter(t *testing.T, path, anchor, field string) string {
	t.Helper()
	lines := readSchemaFile(t, path)
	for i, line := range lines {
		if strings.TrimSpace(line) != anchor {
			continue
		}
		for _, next := range lines[i+1:] {
			if value, ok := strings.CutPrefix(strings.TrimSpace(next), field); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	t.Fatalf("%s has no %q followed by %q", path, anchor, field)
	return ""
}
