package controlplane

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

// layered builds one section from its layers, widest first.
func layered(sectionID, order int64, layers ...domain.PromptLayer) domain.PromptSectionSource {
	return domain.PromptSectionSource{PromptSectionID: sectionID, Caption: "task", Description: "Task instructions", DisplayOrder: order, Layers: layers}
}

func baseLayer(instruction, output string) domain.PromptLayer {
	return domain.PromptLayer{ScopeID: "global", ScopeKind: domain.ScopeKindPlatform, MergeMode: domain.MergeReplace, Instruction: instruction, Output: output}
}

func typeLayer(mode domain.MergeMode, instruction, output string) domain.PromptLayer {
	return domain.PromptLayer{ScopeID: "t:writer", ScopeKind: "agent_type", MergeMode: mode, Editable: true, Instruction: instruction, Output: output}
}

func agentLayer(mode domain.MergeMode, instruction, output string) domain.PromptLayer {
	return domain.PromptLayer{ScopeID: "a:writer-a", ScopeKind: "agent", MergeMode: mode, Editable: true, Instruction: instruction, Output: output}
}

func modelReference(providerID, modelID string) *domain.ModelReference {
	return &domain.ModelReference{ProviderID: providerID, ModelID: modelID}
}

func TestPromptCompilerFoldsLayersWithScopeEngineSemantics(t *testing.T) {
	const add, swap = domain.MergeAppend, domain.MergeReplace
	tests := []struct {
		name        string
		layers      []domain.PromptLayer
		instruction string
		output      string
	}{
		{"baseline only", []domain.PromptLayer{baseLayer("base", "o1")}, "base", "o1"},
		{"type only", []domain.PromptLayer{typeLayer(add, "tenant", "")}, "tenant", ""},
		{"baseline and type append", []domain.PromptLayer{baseLayer("base", ""), typeLayer(add, "tenant", "")}, "base\n\ntenant", ""},
		{"agent append", []domain.PromptLayer{baseLayer("base", ""), agentLayer(add, "agent", "")}, "base\n\nagent", ""},
		{"replace drops everything inherited", []domain.PromptLayer{baseLayer("base", "o1"), typeLayer(add, "tenant", ""), agentLayer(swap, "agent", "")}, "agent", ""},
		{"all append", []domain.PromptLayer{baseLayer("base", ""), typeLayer(add, "tenant", ""), agentLayer(add, "agent", "")}, "base\n\ntenant\n\nagent", ""},
		{"appended outputs join", []domain.PromptLayer{baseLayer("base", "o1"), typeLayer(add, "tenant", "o2")}, "base\n\ntenant", "o1\n\no2"},
		{"empty output inherits", []domain.PromptLayer{baseLayer("base", "o1"), agentLayer(add, "agent", "")}, "base\n\nagent", "o1"},
		{"a layer may add an output contract alone", []domain.PromptLayer{baseLayer("base", ""), agentLayer(add, "", "object")}, "base", "object"},
	}
	compiler := &PromptCompiler{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, tc.layers...)})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			section := compiled.Sections[0]
			if section.Instruction != tc.instruction || section.Output != tc.output {
				t.Fatalf("section = %q / %q, want %q / %q", section.Instruction, section.Output, tc.instruction, tc.output)
			}
			last := tc.layers[len(tc.layers)-1]
			if section.Source.ScopeID != last.ScopeID || section.Source.ScopeKind != last.ScopeKind || section.Source.ResourceID != "1/en-US" {
				t.Fatalf("source = %+v, want the deciding layer %q", section.Source, last.ScopeID)
			}
		})
	}
}

// Sealing is set by the wider scope, so the narrower one cannot opt out of it.
func TestPromptCompilerRefusesALayerUnderASealedOne(t *testing.T) {
	sealed := typeLayer(domain.MergeAppend, "never promise a refund", "")
	sealed.Sealed = true
	compiler := &PromptCompiler{}
	if _, err := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""), sealed, agentLayer(domain.MergeReplace, "agent", ""))}); !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("want ErrSealed, got %v", err)
	}
	compiled, err := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""), sealed)})
	if err != nil || !compiled.Sections[0].Source.Sealed {
		t.Fatalf("a sealed last layer compiles and says so: %+v, %v", compiled.Sections, err)
	}
}

func TestPromptCompilerOrdersSectionsByDisplayOrderThenID(t *testing.T) {
	sections := []domain.PromptSectionSource{
		layered(9, 9, baseLayer("location", "")),
		layered(2, 2, baseLayer("base tone", ""), agentLayer(domain.MergeReplace, "tone", "")),
		layered(1, 1, baseLayer("task", "")),
		layered(7, 2, typeLayer(domain.MergeAppend, "structure", "")),
	}
	got, err := (&PromptCompiler{}).Compile("en-US", sections)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []int64{1, 2, 7, 9}
	for i, sectionID := range want {
		if got.Sections[i].PromptSectionID != sectionID || got.Sections[i].Sequence != int64(i+1) {
			t.Fatalf("section %d = %+v", i, got.Sections[i])
		}
	}
}

func TestPromptCompilerRejectsInvalidSources(t *testing.T) {
	compiler := &PromptCompiler{}
	one := []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""))}
	tests := []struct {
		name     string
		language string
		sections []domain.PromptSectionSource
		want     error
	}{
		{"missing language", " ", one, domain.ErrValidation},
		{"missing prompts", "en-US", nil, domain.ErrNoPrompts},
		{"missing section id", "en-US", []domain.PromptSectionSource{layered(0, 1, baseLayer("base", ""))}, domain.ErrValidation},
		{"repeated section", "en-US", append(one, one...), domain.ErrValidation},
		{"section without a layer", "en-US", []domain.PromptSectionSource{layered(1, 1)}, domain.ErrValidation},
		{"replace without an instruction", "en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""), agentLayer(domain.MergeReplace, "", "object"))}, domain.ErrValidation},
		{"unsupported merge mode", "en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""), agentLayer(domain.MergeIntersect, "agent", ""))}, domain.ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compiler.Compile(tc.language, tc.sections)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPromptCompilerDigestIsStable(t *testing.T) {
	compiler := &PromptCompiler{}
	// Provenance is not part of the digest, so a prompt whose text did not change keeps it.
	language, err := compiler.Compile("en-US", []domain.PromptSectionSource{
		layered(1, 1, baseLayer("base", "object")),
		layered(2, 2, typeLayer(domain.MergeAppend, "tone", "")),
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
	english, _ := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""))})
	german, _ := compiler.Compile("de-DE", []domain.PromptSectionSource{layered(1, 1, baseLayer("basis", ""))})
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
	language, _ := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("base", ""))})
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
