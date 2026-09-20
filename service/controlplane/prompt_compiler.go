package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const (
	promptDigestVersion     = "scout.prompt.v1"
	definitionDigestVersion = "scout.agent_definition.v1"
)

// PromptCompiler folds each section's layers, widest first, through the scope
// engine's prompt merger and creates canonical digests. A layer under a sealed one is refused.
type PromptCompiler struct {
	// Merger is optional; nil uses scope.PromptMerger.
	Merger contract.ResourceMerger
}

// Compile merges layered sections into an ordered immutable language snapshot.
func (compiler *PromptCompiler) Compile(languageCode string, sections []domain.PromptSectionSource) (domain.CompiledPrompt, error) {
	if strings.TrimSpace(languageCode) == "" {
		return domain.CompiledPrompt{}, fmt.Errorf("%w: language code is required", domain.ErrValidation)
	}
	if len(sections) == 0 {
		return domain.CompiledPrompt{}, fmt.Errorf("language %q: %w", languageCode, domain.ErrNoPrompts)
	}
	ordered := slices.Clone(sections)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].DisplayOrder != ordered[j].DisplayOrder {
			return ordered[i].DisplayOrder < ordered[j].DisplayOrder
		}
		return ordered[i].PromptSectionID < ordered[j].PromptSectionID
	})
	compiled := domain.CompiledPrompt{LanguageCode: languageCode, Sections: make([]domain.CompiledPromptSection, 0, len(ordered))}
	seen := make(map[int64]struct{}, len(ordered))
	for _, section := range ordered {
		if _, duplicate := seen[section.PromptSectionID]; duplicate || section.PromptSectionID <= 0 {
			return domain.CompiledPrompt{}, fmt.Errorf("%w: prompt section %d is invalid or repeated", domain.ErrValidation, section.PromptSectionID)
		}
		seen[section.PromptSectionID] = struct{}{}
		value, source, err := compiler.fold(languageCode, section)
		if err != nil {
			return domain.CompiledPrompt{}, err
		}
		compiled.Sections = append(compiled.Sections, domain.CompiledPromptSection{
			Sequence: int64(len(compiled.Sections) + 1), PromptSectionID: section.PromptSectionID,
			Caption: section.Caption, Description: section.Description,
			Instruction: value.Instruction, Output: value.Output, Source: source,
		})
	}
	compiled.Digest = compiledPromptDigest(compiled)
	return compiled, nil
}

// fold applies one section's layers in order and reports the layer that decided it.
func (compiler *PromptCompiler) fold(languageCode string, section domain.PromptSectionSource) (domain.PromptValue, domain.Provenance, error) {
	merger := compiler.Merger
	if merger == nil {
		merger = scope.PromptMerger{}
	}
	if len(section.Layers) == 0 {
		return domain.PromptValue{}, domain.Provenance{}, fmt.Errorf("%w: prompt section %d has no layer", domain.ErrValidation, section.PromptSectionID)
	}
	resourceID := PromptResourceID(section.PromptSectionID, languageCode)
	var merged []byte
	var source domain.Provenance
	var sealedBy *domain.PromptLayer
	for index, layer := range section.Layers {
		if sealedBy != nil {
			return domain.PromptValue{}, domain.Provenance{}, fmt.Errorf("%w: scope %q cannot override prompt section %d sealed at scope %q",
				domain.ErrSealed, layer.ScopeID, section.PromptSectionID, sealedBy.ScopeID)
		}
		value, err := json.Marshal(promptBindingValue{Instruction: layer.Instruction, Output: layer.Output})
		if err != nil {
			return domain.PromptValue{}, domain.Provenance{}, err
		}
		binding := domain.ScopedBinding{
			ScopeID: layer.ScopeID, ResourceKind: domain.ResourcePromptSection, ResourceID: resourceID,
			MergeMode: layer.MergeMode, Sealed: layer.Sealed, Value: value,
		}
		if merged, err = merger.Merge(context.Background(), merged, binding); err != nil {
			return domain.PromptValue{}, domain.Provenance{}, fmt.Errorf("prompt section %d at scope %q: %w", section.PromptSectionID, layer.ScopeID, err)
		}
		source = domain.Provenance{
			ScopeID: layer.ScopeID, ScopeKind: layer.ScopeKind, ResourceKind: domain.ResourcePromptSection, ResourceID: resourceID,
			ResourceVersion: layer.Version, MergeMode: mergeModeOrReplace(layer.MergeMode), Sealed: layer.Sealed, Approver: layer.BoundBy,
		}
		if layer.Sealed {
			sealedBy = &section.Layers[index]
		}
	}
	var effective promptBindingValue
	if err := json.Unmarshal(merged, &effective); err != nil {
		return domain.PromptValue{}, domain.Provenance{}, err
	}
	return domain.PromptValue{Instruction: effective.Instruction, Output: effective.Output}, source, nil
}

// promptBindingValue is the canonical prompt_section binding value.
type promptBindingValue struct {
	Instruction string `json:"instruction"`
	Output      string `json:"output,omitempty"`
}

// PromptResourceID is the config_scope_binding resource id of one section in one language.
func PromptResourceID(promptSectionID int64, languageCode string) string {
	return fmt.Sprintf("%d/%s", promptSectionID, languageCode)
}

// DefinitionDigest returns a canonical digest of runtime-relevant definition fields.
func (*PromptCompiler) DefinitionDigest(definition domain.AgentDefinition) (string, error) {
	if strings.TrimSpace(definition.AgentTypeID) == "" {
		return "", fmt.Errorf("%w: agent kind is required", domain.ErrValidation)
	}
	extension, err := canonicalJSON(definition.Extension)
	if err != nil {
		return "", fmt.Errorf("%w: invalid definition extension: %v", domain.ErrValidation, err)
	}
	languages := append([]domain.CompiledPrompt(nil), definition.Languages...)
	sort.Slice(languages, func(i, j int) bool { return languages[i].LanguageCode < languages[j].LanguageCode })
	seen := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		if strings.TrimSpace(language.LanguageCode) == "" {
			return "", fmt.Errorf("%w: compiled language code is required", domain.ErrValidation)
		}
		if _, exists := seen[language.LanguageCode]; exists {
			return "", fmt.Errorf("%w: duplicate compiled language %q", domain.ErrValidation, language.LanguageCode)
		}
		seen[language.LanguageCode] = struct{}{}
		if expected := compiledPromptDigest(language); language.Digest != expected {
			return "", fmt.Errorf("%w: compiled language %q digest does not match its content", domain.ErrValidation, language.LanguageCode)
		}
	}

	var payload strings.Builder
	payload.WriteString(definitionDigestVersion)
	payload.WriteByte('\n')
	writeDigestField(&payload, definition.AgentTypeID)
	writeModelReference(&payload, definition.Models.Text)
	writeModelReference(&payload, definition.Models.Image)
	writeModelReference(&payload, definition.Models.Video)
	writeDigestField(&payload, boolDigestField(definition.Enabled))
	writeDigestField(&payload, boolDigestField(definition.ApprovalPolicy.RequireApproval))
	writeDigestField(&payload, string(extension))
	for _, language := range languages {
		writeDigestField(&payload, language.LanguageCode)
		writeDigestField(&payload, language.Digest)
		payload.WriteByte(0x1e)
	}
	// Written only when present, so a definition without tools keeps its v1 digest.
	if len(definition.Tools) > 0 || definition.ToolLoop != nil {
		tools := append([]domain.ToolReference(nil), definition.Tools...)
		sort.Slice(tools, func(i, j int) bool { return tools[i].ToolID < tools[j].ToolID })
		for _, tool := range tools {
			writeDigestField(&payload, tool.ToolID)
			writeDigestField(&payload, tool.Version)
		}
		payload.WriteByte(0x1e)
		loop, err := json.Marshal(definition.ToolLoop)
		if err != nil {
			return "", fmt.Errorf("%w: invalid tool loop configuration: %v", domain.ErrValidation, err)
		}
		writeDigestField(&payload, string(loop))
	}
	return sha256Hex(payload.String()), nil
}

func compiledPromptDigest(prompt domain.CompiledPrompt) string {
	var payload strings.Builder
	payload.WriteString(promptDigestVersion)
	payload.WriteByte('\n')
	writeDigestField(&payload, prompt.LanguageCode)
	for _, section := range prompt.Sections {
		writeDigestField(&payload, fmt.Sprintf("%d", section.Sequence))
		writeDigestField(&payload, fmt.Sprintf("%d", section.PromptSectionID))
		writeDigestField(&payload, section.Caption)
		writeDigestField(&payload, section.Description)
		writeDigestField(&payload, section.Instruction)
		writeDigestField(&payload, section.Output)
		payload.WriteByte(0x1e)
	}
	return sha256Hex(payload.String())
}

func writeModelReference(payload *strings.Builder, reference *domain.ModelReference) {
	if reference == nil {
		writeDigestField(payload, "0")
		return
	}
	writeDigestField(payload, "1")
	writeDigestField(payload, reference.ProviderID)
	writeDigestField(payload, reference.ModelID)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("multiple JSON values")
	}
	return json.Marshal(value)
}

func writeDigestField(payload *strings.Builder, value string) {
	fmt.Fprintf(payload, "%d:%s", len(value), value)
	payload.WriteByte(0x1f)
}

func boolDigestField(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

var _ contract.PromptCompiler = (*PromptCompiler)(nil)
