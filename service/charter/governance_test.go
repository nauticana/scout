package charter

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/validate"

	"github.com/nauticana/scout/domain"
)

// The governance Studio freezes is a fragment of a Charter AgentDefinition: decoded into Charter's
// own model and completed with an envelope, it is a schema-valid document.
func TestStudioGovernanceIsACharterAgentDefinitionFragment(t *testing.T) {
	governance := domain.AgentGovernance{
		Purpose:              "Answer billing questions",
		Accountable:          &domain.CharterObjectRef{Kind: "Position", ID: "POS-SALES-OPERATIONS-MANAGER"},
		ResponsibilityIDs:    []domain.CharterRef{{ID: "RESP-COORDINATE-ORDER-EXCEPTION"}},
		Triggers:             []string{"customer asks in the app", "nightly invoice review"},
		RequiredInputs:       []string{"customer account"},
		ExpectedOutcomes:     []string{"answer with sources"},
		CapabilityIDs:        []domain.CharterRef{{ID: "CAP-READ-ORDER-EXCEPTION"}, {Namespace: "billing.example", ID: "CAP-READ-INVOICE"}},
		Policies:             []string{"refund policy"},
		ConfidenceBoundaries: []string{"amount above the refund limit"},
		EscalationConditions: []string{"customer disputes a charge"},
		TriggerChannels: []domain.AgentTrigger{
			{Trigger: "customer asks in the app", Channel: domain.TriggerConversation},
			{Trigger: "nightly invoice review", Channel: domain.TriggerSchedule, Binding: "sched-17"},
		},
		Enterprise: &domain.GovernanceSnapshotRef{
			Document: domain.CharterObjectRef{Kind: "AgentDefinition", ID: "AGENTDEF-BILLING"}, Version: "4", Digest: "sha256:4f1c",
			Fields: []domain.GovernanceField{domain.GovernancePurpose},
		},
	}
	section, err := json.Marshal(governance)
	if err != nil {
		t.Fatal(err)
	}
	var def model.AgentDefinition
	if err = json.Unmarshal(section, &def); err != nil {
		t.Fatalf("decode the section as a Charter definition: %v", err)
	}
	def.CharterSpecVersion, def.Namespace, def.ID, def.Kind = "1.3.0", harborNS, "AGENTDEF-BILLING-1", model.KindAgentDefinition
	def.EnterpriseID = &model.Ref{ID: "ENT-HARBOR"}
	def.AgentIdentityID, def.DefinitionVersion = model.Ref{ID: "AGENT-BILLING"}, "1"
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := corpus.Parser{}.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := validate.NewStructural()
	if err != nil {
		t.Fatal(err)
	}
	if findings := structural.ValidateDocument(doc); len(findings) > 0 {
		t.Fatalf("the completed definition is not schema-valid: %+v", findings)
	}
	if def.CapabilityIDs[1] != (model.Ref{Namespace: "billing.example", ID: "CAP-READ-INVOICE"}) || def.Accountable.Kind != model.KindPosition ||
		!reflect.DeepEqual(def.Triggers, governance.Triggers) || !reflect.DeepEqual(def.EscalationConditions, governance.EscalationConditions) {
		t.Fatalf("Charter read other values: %+v", def)
	}
}

// A product's governance source can map a Charter AgentDefinition into a snapshot by decoding it,
// and the snapshot encodes back to the same Charter values.
func TestCharterAgentDefinitionDecodesIntoStudioGovernance(t *testing.T) {
	doc, ok := harbor(t).Get(harborNS, "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-1")
	if !ok {
		t.Fatal("harbor agent definition missing")
	}
	var governance domain.AgentGovernance
	if err := json.Unmarshal(doc.Raw, &governance); err != nil {
		t.Fatalf("decode a Charter definition as governance: %v", err)
	}
	if len(governance.TriggerChannels) != 0 || governance.Enterprise != nil {
		t.Fatalf("a Charter document carries no Scout members: %+v", governance)
	}
	original, err := corpus.Decode[model.AgentDefinition](doc)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(governance)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip model.AgentDefinition
	if err = json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Purpose != original.Purpose || roundTrip.Accountable != original.Accountable ||
		!reflect.DeepEqual(roundTrip.ResponsibilityIDs, original.ResponsibilityIDs) || !reflect.DeepEqual(roundTrip.Triggers, original.Triggers) ||
		!reflect.DeepEqual(roundTrip.RequiredInputs, original.RequiredInputs) || !reflect.DeepEqual(roundTrip.ExpectedOutcomes, original.ExpectedOutcomes) ||
		!reflect.DeepEqual(roundTrip.CapabilityIDs, original.CapabilityIDs) || !reflect.DeepEqual(roundTrip.Policies, original.Policies) ||
		!reflect.DeepEqual(roundTrip.ConfidenceBoundaries, original.ConfidenceBoundaries) || !reflect.DeepEqual(roundTrip.EscalationConditions, original.EscalationConditions) {
		t.Fatalf("governance lost Charter values:\n got %+v\nwant %+v", roundTrip, original)
	}
}
