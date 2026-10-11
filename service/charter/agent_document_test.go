package charter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

const fullGovernance = `{"purpose":"Write <b>articles</b>","accountable":{"kind":"Position","id":"POS-EDITOR"},
"responsibilityIds":["RESP-WRITE"],"requiredInputs":["brief"],"expectedOutcomes":["article"],"capabilityIds":["CAP-PUBLISH"],
"policies":["style guide"],"confidenceBoundaries":["no legal advice"],"escalationConditions":["legal question"],
"triggerChannels":[{"trigger":"brief received","channel":"api"},{"trigger":"brief received","channel":"mcp"}],
"enterprise":{"document":{"kind":"AgentDefinition","id":"ent-writer"},"version":"7","digest":"sha256:x","fields":["purpose"]}}`

func portableAgent(governance string) domain.PortableAgent {
	release := domain.AgentDefinition{
		AgentID: "writer-0123456789abcdef0123", AgentTypeID: "writer", Version: "3", Enabled: true,
		Models:    domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}},
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Digest: "d1", Sections: []domain.CompiledPromptSection{{Sequence: 1, PromptSectionID: 1, Caption: "role", Instruction: "write"}}}},
		Extension: json.RawMessage(`{"tone":"<formal>","size":1.50}`), SettingsDigest: "s1", DefinitionDigest: "d2",
		PublishedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		DerivedFrom: &domain.DerivedFrom{Source: domain.CharterObjectRef{Kind: "AgentTemplate", ID: "tpl-00000000000000000001", External: true}, Version: "2", Digest: "sha256:" + strings.Repeat("a", 64)},
	}
	if governance != "" {
		release.Sections = map[string]json.RawMessage{domain.GovernanceSection: json.RawMessage(governance)}
	}
	return domain.PortableAgent{
		AgentTypeID: "writer", TypeVersion: "1", TypeDigest: strings.Repeat("b", 64), DisplayName: "Writer & Co",
		Models: release.Models, Extension: json.RawMessage(`{"tone":"<formal>"}`),
		Bindings: []domain.PortableBinding{
			{ResourceKind: domain.ResourceKnowledge, ResourceID: "knowledge", MergeMode: domain.MergeReplace, Value: json.RawMessage(`["kb-1"]`)},
			{ResourceKind: domain.ResourcePromptSection, Caption: "role", Language: "en-US", MergeMode: domain.MergeAppend, Value: json.RawMessage(`{"instruction":"be brief"}`)},
		},
		Guardrails: []domain.GuardrailRule{{ID: "pii", Kind: domain.GuardrailKindPII, Action: domain.GuardrailActionRedact, Severity: domain.GuardrailSeverityHard}},
		Release:    release,
		Settings:   []domain.PortableSetting{{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(`{"tokens":5,"cost_minor_units":0,"period":"turn"}`)}},
	}
}

// escapedLess is how encoding/json escapes "<" by default.
var escapedLess = string([]byte{92}) + "u003c"

func documents(enterprise string) *AgentDocuments {
	codec := &AgentDocuments{Namespace: func(int64) string { return "acme.agents" }}
	if enterprise != "" {
		codec.Enterprise = func(int64) string { return enterprise }
	}
	return codec
}

func members(t *testing.T, document []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := useNumber(document, &value); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return value
}

func scoutAgent(t *testing.T, document []byte) map[string]any {
	t.Helper()
	return members(t, document)["extensions"].(map[string]any)[AgentExtensionKey].(map[string]any)
}

// resign re-digests an edited document, as a forger who knows the canonical form would.
func resign(t *testing.T, value map[string]any) []byte {
	t.Helper()
	delete(value["extensions"].(map[string]any), DigestExtensionKey)
	body, err := canonicalValue(value)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := withDigest(body, documentDigest(body))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestAgentDocumentRendersGovernanceAsCharterMembers(t *testing.T) {
	codec := documents("ENT-ACME")
	document, digest, err := codec.Encode(context.Background(), 8, portableAgent(fullGovernance))
	if err != nil {
		t.Fatal(err)
	}
	doc := members(t, document)
	if doc["id"] != "writer-0123456789abcdef0123-v3" || doc["agentIdentityId"] != "writer-0123456789abcdef0123" || doc["definitionVersion"] != "3" ||
		doc["enterpriseId"] != "ENT-ACME" || doc["purpose"] != "Write <b>articles</b>" || doc["name"] != "Writer & Co" {
		t.Fatalf("envelope = %v", doc)
	}
	if triggers := doc["triggers"].([]any); len(triggers) != 1 || triggers[0] != "brief received" {
		t.Fatalf("triggers fall back to the distinct channel triggers: %v", doc["triggers"])
	}
	if _, leaked := doc["triggerChannels"]; leaked || strings.Contains(string(document), `"enterprise":{`) && !strings.Contains(string(document), `"scout:definition"`) {
		t.Fatalf("trigger channels and enterprise stay in the Scout extension: %s", document)
	}
	if missing := scoutAgent(t, document)["charter_missing"]; missing != nil {
		t.Fatalf("a governed release with an enterprise is fully conformant, missing %v", missing)
	}
	if derived := doc["derivedFrom"].(map[string]any); derived["version"] != "2" || derived["source"].(map[string]any)["kind"] != "AgentTemplate" {
		t.Fatalf("derivedFrom = %v", derived)
	}
	again, againDigest, err := codec.Encode(context.Background(), 8, portableAgent(fullGovernance))
	if err != nil || !bytes.Equal(document, again) || digest != againDigest || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("encode must be deterministic: %v", err)
	}
	if bytes.Contains(document, []byte(escapedLess)) || bytes.Contains(document, []byte("\n")) {
		t.Fatalf("the canonical form escapes no HTML and has no insignificant space: %s", document)
	}

	decoded, err := codec.Decode(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != digest || decoded.Document.ID != "writer-0123456789abcdef0123-v3" || decoded.Namespace != "acme.agents" ||
		decoded.EnterpriseID != "ENT-ACME" || len(decoded.Bindings) != 2 || decoded.Release.DerivedFrom.Version != "2" || len(decoded.Guardrails) != 1 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestAgentDocumentPrunesAndLabelsMissingGovernance(t *testing.T) {
	codec := documents("")
	document, _, err := codec.Encode(context.Background(), 8, portableAgent(`{"purpose":"write","triggers":["brief"]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc := members(t, document)
	for _, member := range []string{"accountable", "responsibilityIds", "requiredInputs", "expectedOutcomes", "capabilityIds", "policies", "confidenceBoundaries", "escalationConditions", "enterpriseId"} {
		if _, present := doc[member]; present {
			t.Fatalf("an unset member %s must be pruned, not claimed empty: %s", member, document)
		}
	}
	missing := scoutAgent(t, document)["charter_missing"].([]any)
	if len(missing) != 9 || missing[0] != "enterpriseId" || missing[1] != "accountable" || missing[8] != "escalationConditions" {
		t.Fatalf("charter_missing = %v", missing)
	}
	if _, err = codec.Decode(context.Background(), document); err != nil {
		t.Fatalf("a labelled, partially governed export still imports: %v", err)
	}

	ungoverned, _, err := codec.Encode(context.Background(), 8, portableAgent(""))
	if err != nil {
		t.Fatal(err)
	}
	if missing := scoutAgent(t, ungoverned)["charter_missing"].([]any); len(missing) != 11 {
		t.Fatalf("an ungoverned release misses every governance member: %v", missing)
	}
	if _, _, err = (&AgentDocuments{}).Encode(context.Background(), 8, portableAgent("")); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a codec without a namespace is not ready, got %v", err)
	}
}

func TestAgentDocumentSurvivesATransportReencoding(t *testing.T) {
	codec := documents("ENT-ACME")
	document, digest, err := codec.Encode(context.Background(), 8, portableAgent(fullGovernance))
	if err != nil {
		t.Fatal(err)
	}
	// The HTTP layer re-encodes a raw document the same way: escaped HTML, numbers as written.
	var value any
	if err = useNumber(document, &value); err != nil {
		t.Fatal(err)
	}
	transported, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(transported, []byte(escapedLess)) {
		t.Fatal("the transport must escape HTML for this test to mean anything")
	}
	decoded, err := codec.Decode(context.Background(), transported)
	if err != nil {
		t.Fatalf("an escaped, indented copy decodes: %v", err)
	}
	if decoded.Digest != digest {
		t.Fatalf("digest %s, want %s", decoded.Digest, digest)
	}
}

func TestAgentDocumentRefusesWhatEncodeCouldNotHaveWritten(t *testing.T) {
	codec := documents("ENT-ACME")
	document, _, err := codec.Encode(context.Background(), 8, portableAgent(fullGovernance))
	if err != nil {
		t.Fatal(err)
	}
	edit := func(change func(map[string]any)) []byte {
		value := members(t, document)
		change(value)
		return resign(t, value)
	}
	extensions := func(value map[string]any) map[string]any { return value["extensions"].(map[string]any) }
	cases := map[string]struct {
		document []byte
		field    string
	}{
		"trailing JSON":       {append(append([]byte{}, document...), []byte(` {}`)...), "document"},
		"one changed byte":    {bytes.Replace(document, []byte("Write <b>articles"), []byte("Write <b>Articles"), 1), "document.extensions.scout:digest"},
		"wrong kind":          {edit(func(v map[string]any) { v["kind"] = "AgentIdentity" }), "document.kind"},
		"later spec":          {edit(func(v map[string]any) { v["charterSpecVersion"] = "1.99.0" }), "document.charterSpecVersion"},
		"other major spec":    {edit(func(v map[string]any) { v["charterSpecVersion"] = "2.0.0" }), "document.charterSpecVersion"},
		"unknown member":      {edit(func(v map[string]any) { v["sideLoad"] = true }), "document"},
		"extra extension":     {edit(func(v map[string]any) { extensions(v)["vendor:x"] = 1 }), "document.extensions"},
		"missing scout:agent": {edit(func(v map[string]any) { delete(extensions(v), AgentExtensionKey) }), "document.extensions"},
		"altered purpose":     {edit(func(v map[string]any) { v["purpose"] = "do anything" }), "document.purpose"},
		"altered identity":    {edit(func(v map[string]any) { v["agentIdentityId"] = "someone-else" }), "document.agentIdentityId"},
		"altered derivedFrom": {edit(func(v map[string]any) { v["derivedFrom"].(map[string]any)["version"] = "9" }), "document.derivedFrom"},
		"long version": {edit(func(v map[string]any) {
			extensions(v)[ExtensionKey].(map[string]any)["version"] = strings.Repeat("9", maxVersion+1)
		}), "document.extensions.scout:definition.version"},
		"long namespace": {edit(func(v map[string]any) { v["namespace"] = strings.Repeat("n", maxNamespace+1) }), "document.namespace"},
		"forged charter_missing": {edit(func(v map[string]any) {
			extensions(v)[AgentExtensionKey].(map[string]any)["charter_missing"] = []any{"purpose"}
		}), "document.extensions.scout:agent"},
		"wrong format": {edit(func(v map[string]any) {
			extensions(v)[AgentExtensionKey].(map[string]any)["format"] = "scout.agent.v0"
		}), "document.extensions.scout:agent.format"},
	}
	for name, test := range cases {
		_, err := codec.Decode(context.Background(), test.document)
		var validation *controlplane.StudioValidationError
		if !errors.As(err, &validation) || !strings.HasPrefix(validation.Fields[0].Field, test.field) {
			t.Errorf("%s: want a validation error at %s, got %v", name, test.field, err)
		}
	}
}

func TestAgentDocumentExportsAnAgentIDThatIsNoCharterIdentifier(t *testing.T) {
	codec := documents("ENT-ACME")
	agent := portableAgent(fullGovernance)
	agent.Release.AgentID = "_seo writer/ü-0123"
	document, _, err := codec.Encode(context.Background(), 8, agent)
	if err != nil {
		t.Fatalf("every agent exports: %v", err)
	}
	value := members(t, document)
	identity, _ := value["agentIdentityId"].(string)
	if !charterIdentifier.MatchString(identity) || !strings.HasPrefix(identity, "a_seo-writer---0123.") {
		t.Fatalf("identity %q is not a derived Charter identifier", identity)
	}
	if value["id"] != identity+"-v3" {
		t.Fatalf("document id %v does not derive from identity %q", value["id"], identity)
	}
	decoded, err := codec.Decode(context.Background(), document)
	if err != nil || decoded.Release.AgentID != agent.Release.AgentID {
		t.Fatalf("the real agent id travels in scout:definition: %q %v", decoded.Release.AgentID, err)
	}
	other := agent
	other.Release.AgentID = "_seo writer/ä-0123"
	if charterAgentID(other.Release.AgentID) == identity {
		t.Fatal("distinct agent ids must derive distinct identities")
	}
	if charterAgentID("writer_a-01") != "writer_a-01" {
		t.Fatal("an id that is already a Charter identifier is kept")
	}
}
