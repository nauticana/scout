package controlplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	promptDigestVersion     = "scout.prompt.v1"
	definitionDigestVersion = "scout.agent_definition.v1"
)

// PromptCompiler merges prompt inheritance levels and creates canonical digests.
type PromptCompiler struct{}

// Compile merges prompt rows into an ordered immutable language snapshot.
func (*PromptCompiler) Compile(languageCode string, rows []domain.PromptSourceRow) (domain.CompiledPrompt, error) {
	if strings.TrimSpace(languageCode) == "" {
		return domain.CompiledPrompt{}, fmt.Errorf("%w: language code is required", domain.ErrValidation)
	}
	type section struct {
		id           int64
		displayOrder int64
		levels       [4]*domain.PromptSourceRow
	}
	byID := make(map[int64]*section)
	ordered := make([]*section, 0)
	for i := range rows {
		row := &rows[i]
		if row.PromptSectionID <= 0 {
			return domain.CompiledPrompt{}, fmt.Errorf("%w: prompt section id must be positive", domain.ErrValidation)
		}
		if row.SourceLevel < domain.PromptSourceBaseline || row.SourceLevel > domain.PromptSourceAgentOverride {
			return domain.CompiledPrompt{}, fmt.Errorf("%w: prompt section %d has invalid source level %d", domain.ErrValidation, row.PromptSectionID, row.SourceLevel)
		}
		item := byID[row.PromptSectionID]
		if item == nil {
			item = &section{id: row.PromptSectionID, displayOrder: row.DisplayOrder}
			byID[row.PromptSectionID] = item
			ordered = append(ordered, item)
		} else if item.displayOrder != row.DisplayOrder {
			return domain.CompiledPrompt{}, fmt.Errorf("%w: prompt section %d has inconsistent display order", domain.ErrValidation, row.PromptSectionID)
		}
		if item.levels[row.SourceLevel] != nil {
			return domain.CompiledPrompt{}, fmt.Errorf("%w: prompt section %d has duplicate source level %d", domain.ErrValidation, row.PromptSectionID, row.SourceLevel)
		}
		item.levels[row.SourceLevel] = row
	}
	if len(ordered) == 0 {
		return domain.CompiledPrompt{}, fmt.Errorf("language %q: %w", languageCode, domain.ErrNoPrompts)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].displayOrder != ordered[j].displayOrder {
			return ordered[i].displayOrder < ordered[j].displayOrder
		}
		return ordered[i].id < ordered[j].id
	})

	compiled := domain.CompiledPrompt{LanguageCode: languageCode}
	for i, item := range ordered {
		baseline := item.levels[domain.PromptSourceBaseline]
		tenantDefault := item.levels[domain.PromptSourceTenantDefault]
		agentOverride := item.levels[domain.PromptSourceAgentOverride]
		parts := make([]string, 0, 3)
		output := ""
		if baseline != nil {
			parts = append(parts, baseline.Instruction)
			output = baseline.Output
		}
		if tenantDefault != nil && (agentOverride == nil || !agentOverride.Overwrite) {
			parts = append(parts, tenantDefault.Instruction)
		}
		if tenantDefault != nil && tenantDefault.Output != "" {
			output = tenantDefault.Output
		}
		if agentOverride != nil {
			parts = append(parts, agentOverride.Instruction)
			if agentOverride.Output != "" {
				output = agentOverride.Output
			}
		}
		source := firstPromptRow(agentOverride, tenantDefault, baseline)
		compiled.Sections = append(compiled.Sections, domain.CompiledPromptSection{
			Sequence:        int64(i + 1),
			PromptSectionID: item.id,
			Caption:         source.Caption,
			Description:     source.Description,
			Instruction:     strings.Join(parts, "\n\n"),
			Output:          output,
		})
	}
	compiled.Digest = compiledPromptDigest(compiled)
	return compiled, nil
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
	return sha256Hex(payload.String()), nil
}

func firstPromptRow(rows ...*domain.PromptSourceRow) *domain.PromptSourceRow {
	for _, row := range rows {
		if row != nil {
			return row
		}
	}
	return nil
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
