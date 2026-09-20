package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type promptBindingKey struct {
	sectionID int64
	language  string
}

// promptBinding is what Studio may set on a prompt_section binding of a scope it edits.
type promptBinding struct {
	mode   domain.MergeMode
	sealed bool
	value  promptBindingValue
}

func (s *StudioService) promptScopes(ctx context.Context, tenantID int64, agentID, agentTypeID string) (domain.PromptScopes, error) {
	layout := s.Layout
	if layout == nil {
		layout = contract.PromptScopeLayout(BasePromptScopeLayout{})
	}
	return layout.Scopes(ctx, tenantID, agentID, agentTypeID)
}

// desiredPromptBindings reads the draft's layers at one scope; a section without one ends its binding.
func desiredPromptBindings(draft domain.AgentDraft, scopeID string) map[promptBindingKey]promptBinding {
	desired := map[promptBindingKey]promptBinding{}
	for _, language := range draft.Languages {
		for _, section := range language.Sections {
			for _, layer := range section.Layers {
				if layer.ScopeID == scopeID {
					desired[promptBindingKey{section.PromptSectionID, language.LanguageCode}] = promptBinding{
						mode: mergeModeOrAppend(layer.MergeMode), sealed: layer.Sealed,
						value: promptBindingValue{Instruction: layer.Instruction, Output: layer.Output},
					}
				}
			}
		}
	}
	return desired
}

// A Studio layer adds to what it inherits unless it says otherwise.
func mergeModeOrAppend(mode domain.MergeMode) domain.MergeMode {
	if mode == "" {
		return domain.MergeAppend
	}
	return mode
}

func persistedPromptBindings(ctx context.Context, tx keelport.TxQueryService, tenantID int64, scopeID string) (map[promptBindingKey]promptBinding, error) {
	result, err := tx.Query(ctx, qStudioListBindings, tenantID, scopeID)
	if err != nil {
		return nil, fmt.Errorf("list prompt bindings of scope %q: %w", scopeID, err)
	}
	persisted := make(map[promptBindingKey]promptBinding, len(result.Rows))
	for _, row := range result.Rows {
		section, language, found := strings.Cut(common.AsString(row[0]), "/")
		sectionID, err := strconv.ParseInt(section, 10, 64)
		if !found || err != nil {
			return nil, fmt.Errorf("%w: prompt binding %q of scope %q is not <section>/<language>", domain.ErrConflict, common.AsString(row[0]), scopeID)
		}
		binding := promptBinding{mode: domain.MergeMode(common.AsString(row[1])), sealed: common.AsBool(row[2])}
		if err = json.Unmarshal([]byte(common.AsString(row[3])), &binding.value); err != nil {
			return nil, fmt.Errorf("decode prompt binding %q of scope %q: %w", common.AsString(row[0]), scopeID, err)
		}
		persisted[promptBindingKey{sectionID, language}] = binding
	}
	return persisted, nil
}

// syncPromptBindings ends every binding that changed or disappeared and binds the new values.
// Bindings are temporal, so an edit is a new row, never an update of the old one.
func syncPromptBindings(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, scopeID string, desired, persisted map[promptBindingKey]promptBinding) error {
	for key, current := range persisted {
		if next, kept := desired[key]; kept && next == current {
			continue
		}
		if err := endPromptBinding(ctx, tx, actor.TenantID, scopeID, PromptResourceID(key.sectionID, key.language)); err != nil {
			return err
		}
	}
	for key, next := range desired {
		if current, exists := persisted[key]; exists && next == current {
			continue
		}
		value, err := json.Marshal(next.value)
		if err != nil {
			return err
		}
		digest := valueDigest(value)
		if _, err = tx.Query(ctx, qStudioInsertBinding, actor.TenantID, scopeID, PromptResourceID(key.sectionID, key.language),
			digest[:12], string(next.mode), next.sealed, string(value), digest, actor.ActorID); err != nil {
			return fmt.Errorf("bind prompt section %d at scope %q: %w", key.sectionID, scopeID, err)
		}
	}
	return nil
}

// endPromptBinding closes the open binding; one bound in this same instant was never in force and is removed.
func endPromptBinding(ctx context.Context, tx keelport.TxQueryService, tenantID int64, scopeID, resourceID string) error {
	if _, err := tx.Query(ctx, qStudioDropBinding, tenantID, scopeID, resourceID); err != nil {
		return fmt.Errorf("drop prompt binding %q of scope %q: %w", resourceID, scopeID, err)
	}
	if _, err := tx.Query(ctx, qStudioEndBinding, tenantID, scopeID, resourceID); err != nil {
		return fmt.Errorf("end prompt binding %q of scope %q: %w", resourceID, scopeID, err)
	}
	return nil
}

func promptBindingsEqual(left, right map[promptBindingKey]promptBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// resetPromptBindings ends the bindings of one scope, optionally narrowed to a section, a language, or both.
func resetPromptBindings(ctx context.Context, tx keelport.TxQueryService, tenantID int64, scopeID string, request domain.AgentResetRequest) error {
	section := ""
	if request.PromptSectionID > 0 {
		section = strconv.FormatInt(request.PromptSectionID, 10)
	}
	for _, query := range []string{qStudioResetDropBindings, qStudioResetEndBindings} {
		if _, err := tx.Query(ctx, query, tenantID, scopeID, section, section, request.LanguageCode, request.LanguageCode); err != nil {
			return fmt.Errorf("reset prompt bindings of scope %q: %w", scopeID, err)
		}
	}
	return nil
}

// checkSealedLayers refuses a save that would bind under a sealed layer, which no later
// compile could accept. The stored layers are the authority for every scope Studio does not edit.
func (s *StudioService) checkSealedLayers(ctx context.Context, tenantID int64, draft domain.AgentDraft, scopes domain.PromptScopes) error {
	for _, language := range draft.Languages {
		resolved, err := s.Sources.Resolve(ctx, tenantID, draft.AgentID, language.LanguageCode)
		if err != nil && err != domain.ErrNoPrompts {
			return err
		}
		stored := map[int64][]domain.PromptLayer{}
		for _, section := range resolved.Sections {
			stored[section.PromptSectionID] = section.Layers
		}
		for _, section := range language.Sections {
			var writesType, writesAgent, typeSealed bool
			for _, layer := range section.Layers {
				writesType = writesType || layer.ScopeID == scopes.TypeScopeID
				writesAgent = writesAgent || layer.ScopeID == scopes.AgentScopeID
				typeSealed = typeSealed || layer.ScopeID == scopes.TypeScopeID && layer.Sealed
			}
			for _, layer := range stored[section.PromptSectionID] {
				if layer.Sealed && !layer.Editable && (writesType || writesAgent) {
					return fmt.Errorf("%w: prompt section %d is sealed at scope %q", domain.ErrSealed, section.PromptSectionID, layer.ScopeID)
				}
			}
			if typeSealed && writesAgent {
				return fmt.Errorf("%w: prompt section %d is sealed at scope %q", domain.ErrSealed, section.PromptSectionID, scopes.TypeScopeID)
			}
		}
	}
	return nil
}
