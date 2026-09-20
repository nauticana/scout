package controlplane

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestPromptDraftAssemblerKeepsLayersAndTheCompilersEffectiveValues(t *testing.T) {
	assembler := &PromptDraftAssembler{Compiler: &PromptCompiler{}}
	draft, err := assembler.Assemble(domain.ResolvedPrompts{LanguageCode: "en-US", Sections: []domain.PromptSectionSource{
		layered(2, 2, baseLayer("base two", "base output")),
		layered(1, 1, baseLayer("base one", "base output"), typeLayer(domain.MergeAppend, "tenant one", ""), agentLayer(domain.MergeAppend, "agent one", "")),
	}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if draft.LanguageCode != "en-US" || len(draft.Sections) != 2 {
		t.Fatalf("draft = %+v", draft)
	}
	first := draft.Sections[0]
	if first.PromptSectionID != 1 || len(first.Layers) != 3 || first.Layers[1].ScopeID != "t:writer" || !first.Layers[2].Editable {
		t.Fatalf("first section = %+v", first)
	}
	if first.Effective.Instruction != "base one\n\ntenant one\n\nagent one" || first.Effective.Output != "base output" {
		t.Fatalf("effective value = %+v", first.Effective)
	}
}

func TestPromptDraftAssemblerRequiresCompiler(t *testing.T) {
	_, err := (&PromptDraftAssembler{}).Assemble(domain.ResolvedPrompts{LanguageCode: "en-US"})
	if err == nil {
		t.Fatal("expected missing compiler error")
	}
}

func TestPromptDraftAssemblerReturnsCompilerErrors(t *testing.T) {
	assembler := &PromptDraftAssembler{Compiler: &PromptCompiler{}}
	_, err := assembler.Assemble(domain.ResolvedPrompts{LanguageCode: "en-US"})
	if !errors.Is(err, domain.ErrNoPrompts) {
		t.Fatalf("error = %v, want %v", err, domain.ErrNoPrompts)
	}
}
