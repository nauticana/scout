package domain

import (
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
