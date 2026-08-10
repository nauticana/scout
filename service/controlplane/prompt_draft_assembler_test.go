package controlplane

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestPromptDraftAssemblerPreservesSourcesAndEffectiveValues(t *testing.T) {
	rows := []domain.PromptSourceRow{
		promptRow(2, 2, domain.PromptSourceBaseline, false, "base two", "base output"),
		promptRow(1, 1, domain.PromptSourceTenantDefault, false, "tenant one", "tenant output"),
		promptRow(1, 1, domain.PromptSourceBaseline, false, "base one", "base output"),
		promptRow(1, 1, domain.PromptSourceAgentOverride, true, "agent one", ""),
	}
	rows[3].Caption = "specific caption"
	assembler := &PromptDraftAssembler{Compiler: &PromptCompiler{}}
	draft, err := assembler.Assemble(domain.ResolvedPrompts{LanguageCode: "en-US", Rows: rows})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if draft.LanguageCode != "en-US" || len(draft.Sections) != 2 {
		t.Fatalf("draft = %+v", draft)
	}
	first := draft.Sections[0]
	if first.PromptSectionID != 1 || first.Caption != "specific caption" {
		t.Fatalf("first section = %+v", first)
	}
	if first.Baseline.Instruction != "base one" || first.TenantDefault == nil || first.AgentOverride == nil {
		t.Fatalf("source values = %+v", first)
	}
	if !first.AgentOverride.Overwrite || first.Effective.Instruction != "base one\n\nagent one" || first.Effective.Output != "tenant output" {
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
