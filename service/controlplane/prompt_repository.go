package controlplane

import (
	"context"
	"encoding/json"
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
	qPromptAgent            = "scout_prompt_agent"
	qPromptBaselines        = "scout_prompt_baselines"
	qPromptBindings         = "scout_prompt_bindings"
	qPromptBaseLanguages    = "scout_prompt_base_languages"
	qPromptBindingLanguages = "scout_prompt_binding_languages"

	scopeListSeparator = "\x1f"
)

// A prompt_section binding's resource id is "<section id>/<language>"; see PromptResourceID.
var promptSourceQueries = map[string]string{
	qPromptAgent: `SELECT agent_type_id FROM agent_profile WHERE tenant_id = ? AND agent_id = ?`,
	qPromptBaselines: `
SELECT b.baseline_key, s.id, s.caption, s.description, s.display_order, b.instruction, b.output
  FROM prompt_baseline b
  JOIN prompt_section s ON s.id = b.prompt_section_id
 WHERE b.agent_type_id = ? AND b.language_code = ?`,
	qPromptBindings: `
SELECT b.scope_id, s.id, s.caption, s.description, s.display_order,
       b.merge_mode_code, b.sealed, b.resource_version, b.bound_by, b.resource_value
  FROM config_scope_binding b
  JOIN prompt_section s ON s.id = CAST(split_part(b.resource_id, '/', 1) AS BIGINT)
 WHERE b.tenant_id = ? AND b.resource_kind_code = 'prompt_section'
   AND b.scope_id = ANY(string_to_array(?, chr(31)))
   AND split_part(b.resource_id, '/', 2) = ?
   AND b.begda <= CURRENT_TIMESTAMP AND (b.endda IS NULL OR b.endda > CURRENT_TIMESTAMP)`,
	qPromptBaseLanguages: `SELECT baseline_key, language_code FROM prompt_baseline WHERE agent_type_id = ?`,
	qPromptBindingLanguages: `
SELECT DISTINCT split_part(resource_id, '/', 2)
  FROM config_scope_binding
 WHERE tenant_id = ? AND resource_kind_code = 'prompt_section'
   AND scope_id = ANY(string_to_array(?, chr(31)))
   AND begda <= CURRENT_TIMESTAMP AND (endda IS NULL OR endda > CURRENT_TIMESTAMP)`,
}

// BasePromptScopeLayout is the two-level convention AgentProvisioner creates under the
// tenant root: one scope per agent type, one per agent beneath it.
type BasePromptScopeLayout struct{}

// RootPromptScopeID is the tenant root scope of the base layout.
const RootPromptScopeID = "tenant"

func (BasePromptScopeLayout) Scopes(_ context.Context, _ int64, agentID, agentTypeID string) (domain.PromptScopes, error) {
	scopes := domain.PromptScopes{AgentScopeID: "a:" + agentID, TypeScopeID: "t:" + agentTypeID}
	if len(scopes.AgentScopeID) > 80 || len(scopes.TypeScopeID) > 80 {
		return domain.PromptScopes{}, fmt.Errorf("%w: agent and agent type ids are limited to 78 characters by their scope ids", domain.ErrValidation)
	}
	return scopes, nil
}

var _ contract.PromptScopeLayout = BasePromptScopeLayout{}

// PromptRepository resolves an agent's prompt layers: the product baseline, then the
// prompt_section binding of every scope on the agent's chain, widest first.
type PromptRepository struct {
	DB       port.DatabaseRepository
	Selector contract.PromptBaselineSelector
	Scopes   contract.ScopeRepository
	// Layout is optional; nil uses BasePromptScopeLayout.
	Layout contract.PromptScopeLayout

	once sync.Once
	qs   port.QueryService
}

func (r *PromptRepository) init(ctx context.Context) error {
	if r.Selector == nil || r.Scopes == nil {
		return fmt.Errorf("prompt source repository: baseline selector and scope repository are required")
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

// promptPlacement is what every read needs first: the agent's type, its baselines, and its chain.
type promptPlacement struct {
	agentTypeID string
	rank        map[string]int
	scopes      domain.PromptScopes
	chain       domain.ScopeChain
}

func (r *PromptRepository) place(ctx context.Context, tenantID int64, agentID string) (promptPlacement, error) {
	if err := r.init(ctx); err != nil {
		return promptPlacement{}, err
	}
	res, err := r.qs.Query(ctx, qPromptAgent, tenantID, agentID)
	if err != nil {
		return promptPlacement{}, fmt.Errorf("load prompt agent: %w", err)
	}
	if len(res.Rows) == 0 {
		return promptPlacement{}, domain.ErrNotFound
	}
	placement := promptPlacement{agentTypeID: strings.TrimSpace(common.AsString(res.Rows[0][0]))}
	selection, err := r.Selector.Select(ctx, tenantID, agentID, placement.agentTypeID)
	if err != nil {
		return promptPlacement{}, fmt.Errorf("select prompt baseline: %w", err)
	}
	placement.rank = baselineRank(selection.Keys)
	layout := r.Layout
	if layout == nil {
		layout = BasePromptScopeLayout{}
	}
	if placement.scopes, err = layout.Scopes(ctx, tenantID, agentID, placement.agentTypeID); err != nil {
		return promptPlacement{}, err
	}
	if placement.chain, err = r.Scopes.Chain(ctx, tenantID, placement.scopes.AgentScopeID); err != nil {
		return promptPlacement{}, fmt.Errorf("prompt scope chain of agent %q: %w", agentID, err)
	}
	return placement, nil
}

func (placement promptPlacement) scopeList() string {
	ids := make([]string, 0, len(placement.chain))
	for _, scope := range placement.chain {
		ids = append(ids, scope.ScopeID)
	}
	return strings.Join(ids, scopeListSeparator)
}

// Resolve returns each section's layers, widest first.
func (r *PromptRepository) Resolve(ctx context.Context, tenantID int64, agentID, languageCode string) (domain.ResolvedPrompts, error) {
	placement, err := r.place(ctx, tenantID, agentID)
	if err != nil {
		return domain.ResolvedPrompts{}, err
	}
	base, err := r.qs.Query(ctx, qPromptBaselines, placement.agentTypeID, languageCode)
	if err != nil {
		return domain.ResolvedPrompts{}, fmt.Errorf("resolve baselines: %w", err)
	}
	bound, err := r.qs.Query(ctx, qPromptBindings, tenantID, placement.scopeList(), languageCode)
	if err != nil {
		return domain.ResolvedPrompts{}, fmt.Errorf("resolve prompt bindings: %w", err)
	}
	sections := map[int64]*domain.PromptSectionSource{}
	section := func(row []any) *domain.PromptSectionSource {
		id := common.AsInt64(row[0])
		if sections[id] == nil {
			sections[id] = &domain.PromptSectionSource{
				PromptSectionID: id, Caption: common.AsString(row[1]), Description: common.AsString(row[2]), DisplayOrder: common.AsInt64(row[3]),
			}
		}
		return sections[id]
	}

	// One baseline per section: the best-ranked key the product selected; unranked keys never apply.
	selectedKey, selectedRank := "", len(placement.rank)+1
	best := map[int64]int{}
	for _, row := range base.Rows {
		key := strings.TrimSpace(common.AsString(row[0]))
		keyRank, ranked := placement.rank[key]
		if !ranked {
			continue
		}
		if keyRank < selectedRank {
			selectedKey, selectedRank = key, keyRank
		}
		target := section(row[1:])
		if current, chosen := best[target.PromptSectionID]; chosen && current <= keyRank {
			continue
		}
		best[target.PromptSectionID] = keyRank
		target.Layers = []domain.PromptLayer{{
			ScopeID: key, ScopeKind: domain.ScopeKindPlatform, MergeMode: domain.MergeReplace,
			Instruction: common.AsString(row[5]), Output: common.AsString(row[6]),
		}}
	}

	byScope := map[string][][]any{}
	for _, row := range bound.Rows {
		scopeID := common.AsString(row[0])
		byScope[scopeID] = append(byScope[scopeID], row)
	}
	for _, scope := range placement.chain {
		for _, row := range byScope[scope.ScopeID] {
			var value promptBindingValue
			if err = json.Unmarshal([]byte(common.AsString(row[9])), &value); err != nil {
				return domain.ResolvedPrompts{}, fmt.Errorf("decode prompt binding of scope %q: %w", scope.ScopeID, err)
			}
			layer := domain.PromptLayer{
				ScopeID: scope.ScopeID, ScopeKind: scope.ScopeKind, MergeMode: domain.MergeMode(common.AsString(row[5])),
				Sealed: common.AsBool(row[6]), Version: common.AsString(row[7]),
				Editable:    scope.ScopeID == placement.scopes.AgentScopeID || scope.ScopeID == placement.scopes.TypeScopeID,
				Instruction: value.Instruction, Output: value.Output,
			}
			if boundBy, ok := common.AsInt64OK(row[8]); ok {
				layer.BoundBy = &boundBy
			}
			target := section(row[1:])
			target.Layers = append(target.Layers, layer)
		}
	}
	if len(sections) == 0 {
		return domain.ResolvedPrompts{}, domain.ErrNoPrompts
	}
	resolved := domain.ResolvedPrompts{
		AgentID: agentID, AgentTypeID: placement.agentTypeID, BaselineKey: selectedKey, LanguageCode: languageCode, Scopes: placement.scopes,
	}
	for _, source := range sections {
		resolved.Sections = append(resolved.Sections, *source)
	}
	sort.SliceStable(resolved.Sections, func(i, j int) bool {
		if resolved.Sections[i].DisplayOrder != resolved.Sections[j].DisplayOrder {
			return resolved.Sections[i].DisplayOrder < resolved.Sections[j].DisplayOrder
		}
		return resolved.Sections[i].PromptSectionID < resolved.Sections[j].PromptSectionID
	})
	return resolved, nil
}

// Languages lists every language present in the selected baselines or bound on the agent's chain.
func (r *PromptRepository) Languages(ctx context.Context, tenantID int64, agentID string) ([]string, error) {
	placement, err := r.place(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	base, err := r.qs.Query(ctx, qPromptBaseLanguages, placement.agentTypeID)
	if err != nil {
		return nil, fmt.Errorf("list baseline languages: %w", err)
	}
	bound, err := r.qs.Query(ctx, qPromptBindingLanguages, tenantID, placement.scopeList())
	if err != nil {
		return nil, fmt.Errorf("list bound languages: %w", err)
	}
	set := map[string]struct{}{}
	for _, row := range base.Rows {
		if _, ok := placement.rank[strings.TrimSpace(common.AsString(row[0]))]; ok {
			set[common.AsString(row[1])] = struct{}{}
		}
	}
	for _, row := range bound.Rows {
		set[common.AsString(row[0])] = struct{}{}
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
