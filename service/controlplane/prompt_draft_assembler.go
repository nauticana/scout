package controlplane

import (
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PromptDraftAssembler builds an editable prompt view from resolved source rows.
type PromptDraftAssembler struct {
	Compiler contract.PromptCompiler
}

// Assemble combines source-level values with compiled effective content.
func (assembler *PromptDraftAssembler) Assemble(resolved domain.ResolvedPrompts) (domain.AgentLanguageDraft, error) {
	if assembler.Compiler == nil {
		return domain.AgentLanguageDraft{}, fmt.Errorf("prompt draft assembler: compiler is required")
	}
	compiled, err := assembler.Compiler.Compile(resolved.LanguageCode, resolved.Rows)
	if err != nil {
		return domain.AgentLanguageDraft{}, err
	}
	bySection := make(map[int64]*domain.AgentPromptSection, len(compiled.Sections))
	for i := range resolved.Rows {
		row := resolved.Rows[i]
		section := bySection[row.PromptSectionID]
		if section == nil {
			section = &domain.AgentPromptSection{PromptSectionID: row.PromptSectionID}
			bySection[row.PromptSectionID] = section
		}
		switch row.SourceLevel {
		case domain.PromptSourceBaseline:
			section.Baseline = domain.PromptValue{Instruction: row.Instruction, Output: row.Output}
		case domain.PromptSourceTenantDefault:
			section.TenantDefault = &domain.PromptValue{Instruction: row.Instruction, Output: row.Output}
		case domain.PromptSourceAgentOverride:
			section.AgentOverride = &domain.PromptOverride{
				PromptValue: domain.PromptValue{Instruction: row.Instruction, Output: row.Output},
				Overwrite:   row.Overwrite,
			}
		}
	}

	draft := domain.AgentLanguageDraft{LanguageCode: resolved.LanguageCode}
	for _, effective := range compiled.Sections {
		section := bySection[effective.PromptSectionID]
		if section == nil {
			return domain.AgentLanguageDraft{}, fmt.Errorf("%w: compiled prompt section %d has no source row", domain.ErrValidation, effective.PromptSectionID)
		}
		section.Caption = effective.Caption
		section.Description = effective.Description
		section.Effective = domain.PromptValue{Instruction: effective.Instruction, Output: effective.Output}
		draft.Sections = append(draft.Sections, *section)
	}
	return draft, nil
}

var _ contract.PromptDraftAssembler = (*PromptDraftAssembler)(nil)
