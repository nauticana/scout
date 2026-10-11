package dataplane

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/domain"
)

// usageLine is one usage_event row: the part of a turn's usage one model performed,
// or, with a zero model, the part no model performed.
type usageLine struct {
	model domain.ModelReference
	usage domain.Usage
}

// mergeModelSpend adds one breakdown to another without aliasing either, ordered by model.
func mergeModelSpend(total, add []domain.ModelSpend) ([]domain.ModelSpend, error) {
	if len(add) == 0 {
		return total, nil
	}
	merged := slices.Clone(total)
	for _, spend := range add {
		if err := validModelSpend(spend); err != nil {
			return nil, err
		}
		index := slices.IndexFunc(merged, func(existing domain.ModelSpend) bool { return existing.Model == spend.Model })
		if index < 0 {
			merged = append(merged, spend)
			continue
		}
		sum, err := addModelSpend(merged[index], spend)
		if err != nil {
			return nil, err
		}
		merged[index] = sum
	}
	slices.SortFunc(merged, compareModelSpend)
	return merged, nil
}

// usageLines splits settled usage into one line per model plus the part no model
// performed; usage without a breakdown is one unattributed line. The split is
// deterministic, so a replayed settlement writes the same rows.
func usageLines(usage domain.Usage) ([]usageLine, error) {
	remainder := domain.Usage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, ToolCalls: usage.ToolCalls,
		SearchQueries: usage.SearchQueries, CostMinorUnits: usage.CostMinorUnits, Currency: usage.Currency,
	}
	spends := slices.Clone(usage.Models)
	slices.SortFunc(spends, compareModelSpend)
	lines := make([]usageLine, 0, len(spends)+1)
	for i, spend := range spends {
		if err := validModelSpend(spend); err != nil {
			return nil, err
		}
		if i > 0 && spends[i-1].Model == spend.Model {
			return nil, fmt.Errorf("%w: model %s/%s appears twice in the usage breakdown", domain.ErrValidation, spend.Model.ProviderID, spend.Model.ModelID)
		}
		remainder.InputTokens -= spend.InputTokens
		remainder.OutputTokens -= spend.OutputTokens
		remainder.SearchQueries -= spend.SearchQueries
		remainder.CostMinorUnits -= spend.CostMinorUnits
		if remainder.InputTokens < 0 || remainder.OutputTokens < 0 || remainder.SearchQueries < 0 || remainder.CostMinorUnits < 0 {
			return nil, fmt.Errorf("%w: the per-model usage exceeds the turn's totals", domain.ErrValidation)
		}
		lines = append(lines, usageLine{model: spend.Model, usage: domain.Usage{
			InputTokens: spend.InputTokens, OutputTokens: spend.OutputTokens, SearchQueries: spend.SearchQueries,
			CostMinorUnits: spend.CostMinorUnits, Currency: usage.Currency,
		}})
	}
	if len(lines) == 0 || remainder.InputTokens > 0 || remainder.OutputTokens > 0 || remainder.ToolCalls > 0 ||
		remainder.SearchQueries > 0 || remainder.CostMinorUnits > 0 {
		lines = append(lines, usageLine{usage: remainder})
	}
	return lines, nil
}

func validModelSpend(spend domain.ModelSpend) error {
	if strings.TrimSpace(spend.Model.ProviderID) == "" || strings.TrimSpace(spend.Model.ModelID) == "" {
		return fmt.Errorf("%w: model usage names a provider and a model", domain.ErrValidation)
	}
	if spend.InputTokens < 0 || spend.OutputTokens < 0 || spend.SearchQueries < 0 || spend.CostMinorUnits < 0 {
		return fmt.Errorf("%w: model usage cannot be negative", domain.ErrValidation)
	}
	return nil
}

func addModelSpend(total, add domain.ModelSpend) (domain.ModelSpend, error) {
	if total.InputTokens > math.MaxInt64-add.InputTokens || total.OutputTokens > math.MaxInt64-add.OutputTokens ||
		total.SearchQueries > math.MaxInt64-add.SearchQueries || total.CostMinorUnits > math.MaxInt64-add.CostMinorUnits {
		return domain.ModelSpend{}, fmt.Errorf("%w: model usage overflow", domain.ErrValidation)
	}
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.SearchQueries += add.SearchQueries
	total.CostMinorUnits += add.CostMinorUnits
	return total, nil
}

func compareModelSpend(a, b domain.ModelSpend) int {
	return cmp.Or(cmp.Compare(a.Model.ProviderID, b.Model.ProviderID), cmp.Compare(a.Model.ModelID, b.Model.ModelID))
}

// stagedModelUsage validates usage's breakdown and encodes it for the staged_model_usage
// column; usage without a breakdown stages NULL.
func stagedModelUsage(usage domain.Usage) (any, error) {
	if _, err := usageLines(usage); err != nil {
		return nil, err
	}
	if len(usage.Models) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(usage.Models)
	if err != nil {
		return nil, fmt.Errorf("encode staged model usage: %w", err)
	}
	return string(raw), nil
}

func decodeStagedModels(value any) ([]domain.ModelSpend, error) {
	if value == nil {
		return nil, nil
	}
	raw := []byte(common.AsString(value))
	if len(raw) == 0 {
		return nil, nil
	}
	var models []domain.ModelSpend
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, fmt.Errorf("decode staged model usage: %w", err)
	}
	return models, nil
}

// stagedUsage rebuilds a turn's staged usage before settlement.
func stagedUsage(state TurnState) (domain.Usage, error) {
	usage := domain.Usage{
		InputTokens: state.InputTokens, OutputTokens: state.OutputTokens,
		ToolCalls: state.ToolCalls, SearchQueries: state.SearchQueries,
		CostMinorUnits: state.CostMinorUnits, Currency: state.Currency, Models: state.Models,
	}
	if _, err := usageLines(usage); err != nil {
		return domain.Usage{}, err
	}
	return usage, nil
}
