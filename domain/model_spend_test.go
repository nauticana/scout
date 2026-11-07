package domain

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestUsageAttributedToReplacesTheBreakdownAndKeepsToolCallsUnattributed(t *testing.T) {
	model := ModelReference{ProviderID: "anthropic", ModelID: "claude"}
	usage := Usage{InputTokens: 3, OutputTokens: 4, ToolCalls: 2, SearchQueries: 1, CostMinorUnits: 5, Currency: "USD",
		Models: []ModelSpend{{Model: ModelReference{ProviderID: "other", ModelID: "x"}, InputTokens: 3}}}
	attributed := usage.AttributedTo(model)
	want := []ModelSpend{{Model: model, InputTokens: 3, OutputTokens: 4, SearchQueries: 1, CostMinorUnits: 5}}
	if !reflect.DeepEqual(attributed.Models, want) || attributed.ToolCalls != 2 || attributed.CostMinorUnits != 5 {
		t.Fatalf("attributed = %+v", attributed)
	}
	if usage.Models[0].Model.ProviderID != "other" {
		t.Fatal("attribution changed the original usage")
	}
	for name, got := range map[string]Usage{
		"blank model":     usage.AttributedTo(ModelReference{ProviderID: "anthropic"}),
		"tool calls only": Usage{ToolCalls: 1}.AttributedTo(model),
	} {
		if got.Models != nil {
			t.Fatalf("%s: models = %+v, want none", name, got.Models)
		}
	}
}

func TestUsageAddChecksPartsAndMergesTheBreakdown(t *testing.T) {
	gpt := ModelReference{ProviderID: "openai", ModelID: "gpt"}
	total, err := Usage{InputTokens: 1, ToolCalls: 1, Currency: "USD", CostMinorUnits: 2}.Add(Usage{InputTokens: 2, CostMinorUnits: 3, Currency: "USD"}.AttributedTo(gpt))
	if err != nil || total.InputTokens != 3 || total.CostMinorUnits != 5 || total.ToolCalls != 1 || len(total.Models) != 1 || total.Models[0].InputTokens != 2 {
		t.Fatalf("total = %+v, %v", total, err)
	}
	if _, err := (Usage{InputTokens: -1}).Add(Usage{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative total: error = %v, want validation", err)
	}
	if _, err := (Usage{CostMinorUnits: 1}).Add(Usage{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("priced total without currency: error = %v, want validation", err)
	}
	for name, add := range map[string]Usage{
		"negative":         {OutputTokens: -1},
		"cost no currency": {CostMinorUnits: 1},
		"other currency":   {CostMinorUnits: 1, Currency: "EUR"},
		"overflow":         {CostMinorUnits: math.MaxInt64},
		"unnamed model":    {InputTokens: 1, Models: []ModelSpend{{InputTokens: 1}}},
	} {
		if _, err := total.Add(add); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
	}
}
