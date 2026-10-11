package domain

// ModelSpend is the part of a Usage one model performed, costed in the Usage currency.
type ModelSpend struct {
	Model          ModelReference `json:"model"`
	InputTokens    int64          `json:"input_tokens"`
	OutputTokens   int64          `json:"output_tokens"`
	SearchQueries  int64          `json:"search_queries"`
	CostMinorUnits int64          `json:"cost_minor_units"`
}

// AttributedTo returns the usage with its tokens, searches, and cost attributed to
// model, replacing any earlier breakdown; tool calls stay unattributed. Usage with
// nothing a model performed, or a blank model, carries no breakdown.
func (usage Usage) AttributedTo(model ModelReference) Usage {
	usage.Models = nil
	if model.ProviderID == "" || model.ModelID == "" ||
		usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.SearchQueries == 0 && usage.CostMinorUnits == 0 {
		return usage
	}
	usage.Models = []ModelSpend{{
		Model: model, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		SearchQueries: usage.SearchQueries, CostMinorUnits: usage.CostMinorUnits,
	}}
	return usage
}
