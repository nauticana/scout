package controlplane

import (
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PromptDraftAssembler pairs each section's layers with the text the compiler makes of them.
type PromptDraftAssembler struct {
	Compiler contract.PromptCompiler
}

// Assemble returns sections in compiled order; the effective values are always the compiler's.
func (a *PromptDraftAssembler) Assemble(resolved domain.ResolvedPrompts) (domain.AgentLanguageDraft, error) {
	if a.Compiler == nil {
		return domain.AgentLanguageDraft{}, domain.ErrNotReady
	}
	compiled, err := a.Compiler.Compile(resolved.LanguageCode, resolved.Sections)
	if err != nil {
		return domain.AgentLanguageDraft{}, err
	}
	layers := make(map[int64][]domain.PromptLayer, len(resolved.Sections))
	for _, section := range resolved.Sections {
		layers[section.PromptSectionID] = section.Layers
	}
	draft := domain.AgentLanguageDraft{LanguageCode: resolved.LanguageCode, Sections: make([]domain.AgentPromptSection, 0, len(compiled.Sections))}
	for _, section := range compiled.Sections {
		sources, ok := layers[section.PromptSectionID]
		if !ok {
			return domain.AgentLanguageDraft{}, fmt.Errorf("%w: compiled section %d has no source", domain.ErrValidation, section.PromptSectionID)
		}
		draft.Sections = append(draft.Sections, domain.AgentPromptSection{
			PromptSectionID: section.PromptSectionID, Caption: section.Caption, Description: section.Description,
			Layers: sources, Effective: domain.PromptValue{Instruction: section.Instruction, Output: section.Output},
		})
	}
	return draft, nil
}

var _ contract.PromptDraftAssembler = (*PromptDraftAssembler)(nil)
