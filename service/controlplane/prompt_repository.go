package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qPromptAgent           = "scout_prompt_agent"
	qPromptBaselines       = "scout_prompt_baselines"
	qPromptTenantDefaults  = "scout_prompt_tenant_defaults"
	qPromptAgentOverrides  = "scout_prompt_agent_overrides"
	qPromptBaseLanguages   = "scout_prompt_base_languages"
	qPromptTenantLanguages = "scout_prompt_tenant_languages"
	qPromptAgentLanguages  = "scout_prompt_agent_languages"
)

var promptSourceQueries = map[string]string{
	qPromptAgent: `SELECT agent_type_id FROM agent_profile WHERE tenant_id = ? AND agent_id = ?`,
	qPromptBaselines: `
SELECT b.baseline_key, s.id, s.caption, s.description, s.display_order, b.instruction, b.output
  FROM prompt_baseline b
  JOIN prompt_section s ON s.id = b.prompt_section_id
 WHERE b.agent_type_id = ? AND b.language_code = ?`,
	qPromptTenantDefaults: `
SELECT s.id, s.caption, s.description, s.display_order, d.instruction, d.output
  FROM tenant_prompt_default d
  JOIN prompt_section s ON s.id = d.prompt_section_id
 WHERE d.tenant_id = ? AND d.agent_type_id = ? AND d.language_code = ?`,
	qPromptAgentOverrides: `
SELECT s.id, s.caption, s.description, s.display_order, o.overwrite, o.instruction, o.output
  FROM agent_prompt_override o
  JOIN prompt_section s ON s.id = o.prompt_section_id
 WHERE o.tenant_id = ? AND o.agent_id = ? AND o.language_code = ?`,
	qPromptBaseLanguages:   `SELECT baseline_key, language_code FROM prompt_baseline WHERE agent_type_id = ?`,
	qPromptTenantLanguages: `SELECT language_code FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ?`,
	qPromptAgentLanguages:  `SELECT language_code FROM agent_prompt_override WHERE tenant_id = ? AND agent_id = ?`,
}

// PromptRepository resolves Scout prompt tables through Keel named SQL.
type PromptRepository struct {
	DB       port.DatabaseRepository
	Selector contract.PromptBaselineSelector

	once sync.Once
	qs   port.QueryService
}

func (r *PromptRepository) init(ctx context.Context) error {
	if r.Selector == nil {
		return fmt.Errorf("prompt source repository: selector is required")
	}
	if r.qs != nil {
		return nil
	}
	if r.DB == nil {
		return fmt.Errorf("prompt source repository: database is required")
	}
	r.once.Do(func() { r.qs = r.DB.GetQueryService(ctx, promptSourceQueries) })
	return nil
}

func (r *PromptRepository) agentTypeID(ctx context.Context, tenantID int64, agentID string) (string, error) {
	if err := r.init(ctx); err != nil {
		return "", err
	}
	res, err := r.qs.Query(ctx, qPromptAgent, tenantID, agentID)
	if err != nil {
		return "", fmt.Errorf("load prompt agent: %w", err)
	}
	if len(res.Rows) == 0 {
		return "", domain.ErrNotFound
	}
	return strings.TrimSpace(common.AsString(res.Rows[0][0])), nil
}

// Resolve returns ordered baseline, tenant-default, and agent-override rows.
func (r *PromptRepository) Resolve(ctx context.Context, tenantID int64, agentID, languageCode string) (domain.ResolvedPrompts, error) {
	agentTypeID, err := r.agentTypeID(ctx, tenantID, agentID)
	if err != nil {
		return domain.ResolvedPrompts{}, err
	}
	selection, err := r.Selector.Select(ctx, tenantID, agentID, agentTypeID)
	if err != nil {
		return domain.ResolvedPrompts{}, fmt.Errorf("select prompt baseline: %w", err)
	}
	rank := baselineRank(selection.Keys)
	rows, baselineKey, err := r.resolveRows(ctx, tenantID, agentID, agentTypeID, languageCode, rank)
	if err != nil {
		return domain.ResolvedPrompts{}, err
	}
	if len(rows) == 0 {
		return domain.ResolvedPrompts{}, domain.ErrNoPrompts
	}
	return domain.ResolvedPrompts{
		AgentID: agentID, AgentTypeID: agentTypeID, BaselineKey: baselineKey,
		LanguageCode: languageCode, Rows: rows,
	}, nil
}

func (r *PromptRepository) resolveRows(ctx context.Context, tenantID int64, agentID, agentTypeID, languageCode string, rank map[string]int) ([]domain.PromptSourceRow, string, error) {
	base, err := r.qs.Query(ctx, qPromptBaselines, agentTypeID, languageCode)
	if err != nil {
		return nil, "", fmt.Errorf("load prompt baselines: %w", err)
	}
	tenant, err := r.qs.Query(ctx, qPromptTenantDefaults, tenantID, agentTypeID, languageCode)
	if err != nil {
		return nil, "", fmt.Errorf("load tenant prompt defaults: %w", err)
	}
	agent, err := r.qs.Query(ctx, qPromptAgentOverrides, tenantID, agentID, languageCode)
	if err != nil {
		return nil, "", fmt.Errorf("load agent prompt overrides: %w", err)
	}

	type baselineChoice struct {
		rank int
		row  domain.PromptSourceRow
	}
	choices := map[int64]baselineChoice{}
	selectedKey := ""
	for _, row := range base.Rows {
		key := strings.TrimSpace(common.AsString(row[0]))
		position, ok := rank[key]
		if !ok {
			continue
		}
		sectionID := common.AsInt64(row[1])
		choice, exists := choices[sectionID]
		if exists && choice.rank <= position {
			continue
		}
		choices[sectionID] = baselineChoice{rank: position, row: sourceRow(row[1:], domain.PromptSourceBaseline, key, false, 4, 5)}
		if selectedKey == "" || position < rank[selectedKey] {
			selectedKey = key
		}
	}

	rows := make([]domain.PromptSourceRow, 0, len(choices)+len(tenant.Rows)+len(agent.Rows))
	for _, choice := range choices {
		rows = append(rows, choice.row)
	}
	for _, row := range tenant.Rows {
		rows = append(rows, sourceRow(row, domain.PromptSourceTenantDefault, agentTypeID, false, 4, 5))
	}
	for _, row := range agent.Rows {
		rows = append(rows, sourceRow(row, domain.PromptSourceAgentOverride, agentID, common.AsBool(row[4]), 5, 6))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].DisplayOrder != rows[j].DisplayOrder {
			return rows[i].DisplayOrder < rows[j].DisplayOrder
		}
		if rows[i].PromptSectionID != rows[j].PromptSectionID {
			return rows[i].PromptSectionID < rows[j].PromptSectionID
		}
		return rows[i].SourceLevel < rows[j].SourceLevel
	})
	return rows, selectedKey, nil
}

func sourceRow(row []any, level domain.PromptSourceLevel, key string, overwrite bool, instructionIndex, outputIndex int) domain.PromptSourceRow {
	return domain.PromptSourceRow{
		PromptSectionID: common.AsInt64(row[0]), Caption: common.AsString(row[1]),
		Description: common.AsString(row[2]), DisplayOrder: common.AsInt64(row[3]),
		SourceLevel: level, SourceKey: key, Overwrite: overwrite,
		Instruction: common.AsString(row[instructionIndex]), Output: common.AsString(row[outputIndex]),
	}
}

// Languages lists every language present in the selected inheritance chain.
func (r *PromptRepository) Languages(ctx context.Context, tenantID int64, agentID string) ([]string, error) {
	agentTypeID, err := r.agentTypeID(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	selection, err := r.Selector.Select(ctx, tenantID, agentID, agentTypeID)
	if err != nil {
		return nil, fmt.Errorf("select prompt baseline: %w", err)
	}
	rank := baselineRank(selection.Keys)
	base, err := r.qs.Query(ctx, qPromptBaseLanguages, agentTypeID)
	if err != nil {
		return nil, fmt.Errorf("list baseline languages: %w", err)
	}
	tenant, err := r.qs.Query(ctx, qPromptTenantLanguages, tenantID, agentTypeID)
	if err != nil {
		return nil, fmt.Errorf("list tenant languages: %w", err)
	}
	agent, err := r.qs.Query(ctx, qPromptAgentLanguages, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list agent languages: %w", err)
	}
	set := map[string]struct{}{}
	for _, row := range base.Rows {
		if _, ok := rank[strings.TrimSpace(common.AsString(row[0]))]; ok {
			set[common.AsString(row[1])] = struct{}{}
		}
	}
	for _, result := range [][][]any{tenant.Rows, agent.Rows} {
		for _, row := range result {
			set[common.AsString(row[0])] = struct{}{}
		}
	}
	languages := make([]string, 0, len(set))
	for language := range set {
		if strings.TrimSpace(language) != "" {
			languages = append(languages, language)
		}
	}
	sort.Strings(languages)
	return languages, nil
}

func baselineRank(keys []string) map[string]int {
	rank := make(map[string]int, len(keys))
	for i, key := range keys {
		key = strings.TrimSpace(key)
		if key != "" {
			if _, exists := rank[key]; !exists {
				rank[key] = i
			}
		}
	}
	return rank
}

var _ contract.PromptSourceRepository = (*PromptRepository)(nil)
