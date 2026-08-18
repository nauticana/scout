package controlplane

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func promptRow(sectionID, order int64, level domain.PromptSourceLevel, overwrite bool, instruction, output string) domain.PromptSourceRow {
	return domain.PromptSourceRow{
		PromptSectionID: sectionID,
		Caption:         "task",
		Description:     "Task instructions",
		DisplayOrder:    order,
		SourceLevel:     level,
		Overwrite:       overwrite,
		Instruction:     instruction,
		Output:          output,
	}
}

func modelReference(providerID, modelID string) *domain.ModelReference {
	return &domain.ModelReference{ProviderID: providerID, ModelID: modelID}
}

func TestPromptCompilerMergeTruthTable(t *testing.T) {
	cases := []struct {
		name       string
		rows       []domain.PromptSourceRow
		wantText   string
		wantOutput string
	}{
		{"baseline only", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "o1")}, "base", "o1"},
		{"tenant only", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", "")}, "tenant", ""},
		{"agent only", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceAgentOverride, true, "agent", "")}, "agent", ""},
		{"baseline and tenant", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", ""), promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", "")}, "base\n\ntenant", ""},
		{"baseline and agent overwrite", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, true, "agent", "")}, "base\n\nagent", ""},
		{"baseline and agent append", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, false, "agent", "")}, "base\n\nagent", ""},
		{"tenant and agent overwrite", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, true, "agent", "")}, "agent", ""},
		{"tenant and agent append", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, false, "agent", "")}, "tenant\n\nagent", ""},
		{"all overwrite", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", ""), promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, true, "agent", "")}, "base\n\nagent", ""},
		{"all append", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", ""), promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, false, "agent", "")}, "base\n\ntenant\n\nagent", ""},
		{"specific output wins", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "o1"), promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", "o2"), promptRow(1, 1, domain.PromptSourceAgentOverride, false, "agent", "o3")}, "base\n\ntenant\n\nagent", "o3"},
		{"empty output inherits", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "o1"), promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant", ""), promptRow(1, 1, domain.PromptSourceAgentOverride, true, "agent", "")}, "base\n\nagent", "o1"},
	}
	compiler := &PromptCompiler{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compiler.Compile("en-US", tc.rows)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if len(got.Sections) != 1 {
				t.Fatalf("sections = %d, want 1", len(got.Sections))
			}
			if got.Sections[0].Instruction != tc.wantText {
				t.Errorf("instruction = %q, want %q", got.Sections[0].Instruction, tc.wantText)
			}
			if got.Sections[0].Output != tc.wantOutput {
				t.Errorf("output = %q, want %q", got.Sections[0].Output, tc.wantOutput)
			}
		})
	}
}

func TestPromptCompilerOrdersSectionsAndUsesSpecificMetadata(t *testing.T) {
	rows := []domain.PromptSourceRow{
		promptRow(9, 9, domain.PromptSourceBaseline, false, "location", ""),
		promptRow(2, 2, domain.PromptSourceAgentOverride, true, "tone", ""),
		promptRow(1, 1, domain.PromptSourceBaseline, false, "task", ""),
		promptRow(7, 2, domain.PromptSourceTenantDefault, false, "structure", ""),
		promptRow(2, 2, domain.PromptSourceBaseline, false, "base tone", ""),
	}
	rows[1].Caption = "specific tone"
	got, err := (&PromptCompiler{}).Compile("en-US", rows)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []int64{1, 2, 7, 9}
	for i, sectionID := range want {
		if got.Sections[i].PromptSectionID != sectionID || got.Sections[i].Sequence != int64(i+1) {
			t.Fatalf("section %d = %+v", i, got.Sections[i])
		}
	}
	if got.Sections[1].Caption != "specific tone" {
		t.Fatalf("caption = %q", got.Sections[1].Caption)
	}
}

func TestPromptCompilerRejectsInvalidSources(t *testing.T) {
	compiler := &PromptCompiler{}
	tests := []struct {
		name     string
		language string
		rows     []domain.PromptSourceRow
		want     error
	}{
		{"missing language", " ", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "")}, domain.ErrValidation},
		{"missing prompts", "en-US", nil, domain.ErrNoPrompts},
		{"missing section id", "en-US", []domain.PromptSourceRow{promptRow(0, 1, domain.PromptSourceBaseline, false, "base", "")}, domain.ErrValidation},
		{"invalid level", "en-US", []domain.PromptSourceRow{promptRow(1, 1, 0, false, "base", "")}, domain.ErrValidation},
		{"duplicate level", "en-US", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "a", ""), promptRow(1, 1, domain.PromptSourceBaseline, false, "b", "")}, domain.ErrValidation},
		{"inconsistent order", "en-US", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "a", ""), promptRow(1, 2, domain.PromptSourceTenantDefault, false, "b", "")}, domain.ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compiler.Compile(tc.language, tc.rows)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPromptCompilerDigestIsStable(t *testing.T) {
	compiler := &PromptCompiler{}
	language, err := compiler.Compile("en-US", []domain.PromptSourceRow{
		promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "object"),
		promptRow(2, 2, domain.PromptSourceTenantDefault, false, "tone", ""),
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = "670dec4e0b18e91e950551ed6bcb4d04c409ed9dce5d0923d11c334d1d921310"
	if language.Digest != want {
		t.Fatalf("digest = %q, want %q", language.Digest, want)
	}
}

func TestPromptCompilerDefinitionDigestIsCanonical(t *testing.T) {
	compiler := &PromptCompiler{}
	english, _ := compiler.Compile("en-US", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "")})
	german, _ := compiler.Compile("de-DE", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "basis", "")})
	definition := domain.AgentDefinition{
		AgentTypeID:    "assistant",
		Enabled:        true,
		Models:         domain.AgentModelSelection{Text: modelReference("provider", "model")},
		ApprovalPolicy: domain.AgentApprovalPolicy{RequireApproval: true},
		Languages:      []domain.CompiledPrompt{english, german},
		Extension:      []byte(`{"b":2,"a":1}`),
	}
	a, err := compiler.DefinitionDigest(definition)
	if err != nil {
		t.Fatalf("DefinitionDigest: %v", err)
	}
	definition.Languages = []domain.CompiledPrompt{german, english}
	definition.Extension = []byte("{\n  \"a\": 1, \"b\": 2\n}")
	b, err := compiler.DefinitionDigest(definition)
	if err != nil {
		t.Fatalf("DefinitionDigest reordered: %v", err)
	}
	if a != b {
		t.Fatalf("canonical digests differ: %q != %q", a, b)
	}
	const want = "1237ed589c0ecccc268aa016ea1ec73e05f494185ddb1d0a4ccd19002846ef25"
	if a != want {
		t.Fatalf("digest = %q, want %q", a, want)
	}

	definition.Enabled = false
	changed, err := compiler.DefinitionDigest(definition)
	if err != nil {
		t.Fatalf("DefinitionDigest changed: %v", err)
	}
	if changed == a {
		t.Fatal("runtime field change did not change digest")
	}
}

func TestPromptCompilerDefinitionDigestRejectsInvalidInput(t *testing.T) {
	compiler := &PromptCompiler{}
	language, _ := compiler.Compile("en-US", []domain.PromptSourceRow{promptRow(1, 1, domain.PromptSourceBaseline, false, "base", "")})
	tests := []struct {
		name       string
		definition domain.AgentDefinition
	}{
		{"missing kind", domain.AgentDefinition{Languages: []domain.CompiledPrompt{language}}},
		{"invalid extension", domain.AgentDefinition{AgentTypeID: "assistant", Languages: []domain.CompiledPrompt{language}, Extension: []byte("{")}},
		{"duplicate language", domain.AgentDefinition{AgentTypeID: "assistant", Languages: []domain.CompiledPrompt{language, language}}},
		{"stale language digest", domain.AgentDefinition{AgentTypeID: "assistant", Languages: []domain.CompiledPrompt{{LanguageCode: language.LanguageCode, Sections: language.Sections, Digest: "stale"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compiler.DefinitionDigest(tc.definition)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want %v", err, domain.ErrValidation)
			}
		})
	}
}
