package domain

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// ModelSpend is the part of a Usage one model performed, costed in the Usage currency.
type ModelSpend struct {
	Model            ModelReference `json:"model"`
	InputTokens      int64          `json:"input_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64          `json:"cache_write_tokens,omitempty"`
	SearchQueries    int64          `json:"search_queries"`
	CostMinorUnits   int64          `json:"cost_minor_units"`
}

// AttributedTo returns the usage with its tokens, searches, and cost attributed to
// model, replacing any earlier breakdown; tool calls stay unattributed. Usage with
// nothing a model performed, or a blank model, carries no breakdown.
func (usage Usage) AttributedTo(model ModelReference) Usage {
	usage.Models = nil
	if model.ProviderID == "" || model.ModelID == "" ||
		usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 &&
			usage.SearchQueries == 0 && usage.CostMinorUnits == 0 {
		return usage
	}
	usage.Models = []ModelSpend{{
		Model: model, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens,
		SearchQueries: usage.SearchQueries, CostMinorUnits: usage.CostMinorUnits,
	}}
	return usage
}

// Validate reports a spend that names no model or holds a negative part.
func (spend ModelSpend) Validate() error {
	if strings.TrimSpace(spend.Model.ProviderID) == "" || strings.TrimSpace(spend.Model.ModelID) == "" {
		return fmt.Errorf("%w: model usage names a provider and a model", ErrValidation)
	}
	if spend.InputTokens < 0 || spend.OutputTokens < 0 || spend.CacheReadTokens < 0 || spend.CacheWriteTokens < 0 ||
		spend.SearchQueries < 0 || spend.CostMinorUnits < 0 {
		return fmt.Errorf("%w: model usage cannot be negative", ErrValidation)
	}
	return nil
}

// CompareModelSpend orders spends by provider, then model.
func CompareModelSpend(a, b ModelSpend) int {
	return cmp.Or(cmp.Compare(a.Model.ProviderID, b.Model.ProviderID), cmp.Compare(a.Model.ModelID, b.Model.ModelID))
}

func (spend ModelSpend) add(other ModelSpend) (ModelSpend, error) {
	if spend.InputTokens > math.MaxInt64-other.InputTokens || spend.OutputTokens > math.MaxInt64-other.OutputTokens ||
		spend.CacheReadTokens > math.MaxInt64-other.CacheReadTokens || spend.CacheWriteTokens > math.MaxInt64-other.CacheWriteTokens ||
		spend.SearchQueries > math.MaxInt64-other.SearchQueries || spend.CostMinorUnits > math.MaxInt64-other.CostMinorUnits {
		return ModelSpend{}, fmt.Errorf("%w: model usage overflow", ErrValidation)
	}
	spend.InputTokens += other.InputTokens
	spend.OutputTokens += other.OutputTokens
	spend.CacheReadTokens += other.CacheReadTokens
	spend.CacheWriteTokens += other.CacheWriteTokens
	spend.SearchQueries += other.SearchQueries
	spend.CostMinorUnits += other.CostMinorUnits
	return spend, nil
}

// MergeModelSpend adds one breakdown to another without aliasing either, ordered by model; nil when both are empty.
func MergeModelSpend(total, add []ModelSpend) ([]ModelSpend, error) {
	var merged []ModelSpend
	for _, spend := range slices.Concat(total, add) {
		if err := spend.Validate(); err != nil {
			return nil, err
		}
		index := slices.IndexFunc(merged, func(existing ModelSpend) bool { return existing.Model == spend.Model })
		if index < 0 {
			merged = append(merged, spend)
			continue
		}
		sum, err := merged[index].add(spend)
		if err != nil {
			return nil, err
		}
		merged[index] = sum
	}
	slices.SortFunc(merged, CompareModelSpend)
	return merged, nil
}

// Add returns the checked sum: no negative part, no overflow, a currency wherever there is cost,
// one currency across both, and the per-model breakdown merged by model. Neither operand is changed.
func (usage Usage) Add(add Usage) (Usage, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0 ||
		usage.ToolCalls < 0 || usage.SearchQueries < 0 || usage.CostMinorUnits < 0 {
		return Usage{}, fmt.Errorf("%w: usage cannot be negative", ErrValidation)
	}
	if add.InputTokens < 0 || add.OutputTokens < 0 || add.CacheReadTokens < 0 || add.CacheWriteTokens < 0 ||
		add.ToolCalls < 0 || add.SearchQueries < 0 || add.CostMinorUnits < 0 {
		return Usage{}, fmt.Errorf("%w: usage cannot be negative", ErrValidation)
	}
	if usage.InputTokens > math.MaxInt64-add.InputTokens || usage.OutputTokens > math.MaxInt64-add.OutputTokens ||
		usage.CacheReadTokens > math.MaxInt64-add.CacheReadTokens || usage.CacheWriteTokens > math.MaxInt64-add.CacheWriteTokens ||
		usage.ToolCalls > math.MaxInt-add.ToolCalls || usage.SearchQueries > math.MaxInt64-add.SearchQueries ||
		usage.CostMinorUnits > math.MaxInt64-add.CostMinorUnits {
		return Usage{}, fmt.Errorf("%w: usage overflow", ErrValidation)
	}
	if add.CostMinorUnits > 0 && len(add.Currency) != 3 {
		return Usage{}, fmt.Errorf("%w: cost requires a currency", ErrValidation)
	}
	if usage.CostMinorUnits > 0 && len(usage.Currency) != 3 {
		return Usage{}, fmt.Errorf("%w: cost requires a currency", ErrValidation)
	}
	if usage.Currency != "" && add.Currency != "" && usage.Currency != add.Currency {
		return Usage{}, fmt.Errorf("%w: usage currencies %s and %s differ", ErrValidation, usage.Currency, add.Currency)
	}
	models, err := MergeModelSpend(usage.Models, add.Models)
	if err != nil {
		return Usage{}, err
	}
	usage.InputTokens += add.InputTokens
	usage.OutputTokens += add.OutputTokens
	usage.CacheReadTokens += add.CacheReadTokens
	usage.CacheWriteTokens += add.CacheWriteTokens
	usage.ToolCalls += add.ToolCalls
	usage.SearchQueries += add.SearchQueries
	usage.CostMinorUnits += add.CostMinorUnits
	usage.Models = models
	if usage.Currency == "" {
		usage.Currency = add.Currency
	}
	return usage, nil
}
