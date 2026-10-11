package domain

import (
	"encoding/json"
	"testing"
)

func TestCharterRefEncodesAsCharterDoes(t *testing.T) {
	refs := []CharterRef{{ID: "RESP-1"}, {Namespace: "acme.example", ID: "CAP-1"}}
	encoded, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `["RESP-1",{"id":"CAP-1","namespace":"acme.example"}]` {
		t.Fatalf("encoded = %s", encoded)
	}
	var decoded []CharterRef
	if err = json.Unmarshal(encoded, &decoded); err != nil || len(decoded) != 2 || decoded[0] != refs[0] || decoded[1] != refs[1] {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
	for _, malformed := range []string{`[7]`, `[{"namespace":"acme.example","id":"CAP-1","kind":"CapabilityContract"}]`, `[["CAP-1"]]`} {
		if err = json.Unmarshal([]byte(malformed), &decoded); err == nil {
			t.Fatalf("%s decoded without error", malformed)
		}
	}
}

func TestAgentGovernanceUsesCharterFieldNames(t *testing.T) {
	encoded, err := json.Marshal(AgentGovernance{
		Purpose: "p", Accountable: &CharterObjectRef{Kind: "Position", ID: "POS-1"}, ResponsibilityIDs: []CharterRef{{ID: "R"}},
		Triggers: []string{"t"}, RequiredInputs: []string{"i"}, ExpectedOutcomes: []string{"o"}, CapabilityIDs: []CharterRef{{ID: "C"}},
		Policies: []string{"x"}, ConfidenceBoundaries: []string{"b"}, EscalationConditions: []string{"e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	for _, field := range CharterGovernanceFields() {
		if _, ok := keys[string(field)]; !ok {
			t.Fatalf("%s is missing from %s", field, encoded)
		}
	}
	if len(keys) != len(CharterGovernanceFields()) {
		t.Fatalf("unexpected members in %s", encoded)
	}
	if empty, _ := json.Marshal(AgentGovernance{}); string(empty) != "{}" {
		t.Fatalf("an empty governance encodes as %s", empty)
	}
}
