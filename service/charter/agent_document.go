package charter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/validate"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

const (
	// AgentExtensionKey holds the agent's draft parts, bindings, and frozen settings in an export.
	AgentExtensionKey = "scout:agent"
	// DigestExtensionKey holds "sha256:<hex>" of the canonical document without it.
	DigestExtensionKey = "scout:digest"

	maxDocumentID = 120
	// maxVersion and maxNamespace are Charter's definitionVersion and namespace limits.
	maxVersion   = 60
	maxNamespace = 200
)

var charterIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// governanceMembers are the Charter governance members of an AgentDefinition in schema order.
var governanceMembers = []string{
	"purpose", "accountable", "responsibilityIds", "triggers", "requiredInputs", "expectedOutcomes",
	"capabilityIds", "policies", "confidenceBoundaries", "escalationConditions",
}

// AgentDocuments renders an exported agent as a Charter AgentDefinition: the Charter members derive
// from the active release, Scout's definition rides in "scout:definition", and the agent's draft
// parts in "scout:agent". The document is fully conformant exactly when "charter_missing" is empty.
type AgentDocuments struct {
	// Namespace maps a tenant to the Charter namespace its agents belong to; required.
	Namespace func(tenantID int64) string
	// Enterprise names the tenant's Charter Enterprise; nil or empty leaves enterpriseId out.
	Enterprise func(tenantID int64) string

	once       sync.Once
	structural *validate.Structural
	err        error
}

var _ contract.AgentDocumentCodec = (*AgentDocuments)(nil)

// agentExtension is the "scout:agent" extension.
type agentExtension struct {
	Format         string                     `json:"format"`
	AgentType      string                     `json:"agent_type"`
	TypeVersion    string                     `json:"type_version,omitempty"`
	TypeDigest     string                     `json:"type_digest,omitempty"`
	DisplayName    string                     `json:"display_name"`
	Models         domain.AgentModelSelection `json:"models"`
	Extension      json.RawMessage            `json:"extension,omitempty"`
	Bindings       []domain.PortableBinding   `json:"bindings"`
	Guardrails     []domain.GuardrailRule     `json:"guardrails,omitempty"`
	DraftOrigin    *domain.DerivedFrom        `json:"draft_origin,omitempty"`
	Settings       []domain.PortableSetting   `json:"settings"`
	CharterMissing []string                   `json:"charter_missing,omitempty"`
}

// Encode renders the agent in the tenant's namespace and returns the canonical document and its digest.
func (d *AgentDocuments) Encode(_ context.Context, tenantID int64, agent domain.PortableAgent) (json.RawMessage, string, error) {
	if d.Namespace == nil {
		return nil, "", fmt.Errorf("%w: agent documents need a namespace", domain.ErrNotReady)
	}
	namespace := d.Namespace(tenantID)
	if strings.TrimSpace(namespace) == "" {
		return nil, "", fmt.Errorf("%w: tenant %d has no charter namespace", domain.ErrNotReady, tenantID)
	}
	spec, err := validate.Catalog{}.SpecVersion()
	if err != nil {
		return nil, "", fmt.Errorf("read charter specification version: %w", err)
	}
	enterprise := ""
	if d.Enterprise != nil {
		enterprise = d.Enterprise(tenantID)
	}
	body, err := d.render(namespace, enterprise, spec, agent)
	if err != nil {
		return nil, "", err
	}
	digest := documentDigest(body)
	signed, err := withDigest(body, digest)
	if err != nil {
		return nil, "", err
	}
	return signed, digest, nil
}

// Decode reads a document Encode wrote, refusing any other content, and returns the agent it carries.
func (d *AgentDocuments) Decode(_ context.Context, document json.RawMessage) (domain.PortableAgent, error) {
	canonical, err := canonicalDocument(document)
	if err != nil {
		return domain.PortableAgent{}, documentError("document", err.Error())
	}
	parsed, err := corpus.Parser{}.Parse(canonical)
	if err != nil {
		return domain.PortableAgent{}, documentError("document", "is not a Charter document: "+err.Error())
	}
	if parsed.Kind != model.KindAgentDefinition {
		return domain.PortableAgent{}, documentError("document.kind", "must be AgentDefinition")
	}
	spec, err := validate.Catalog{}.SpecVersion()
	if err != nil {
		return domain.PortableAgent{}, fmt.Errorf("read charter specification version: %w", err)
	}
	documents := corpus.New()
	if err = documents.Add(parsed); err != nil {
		return domain.PortableAgent{}, documentError("document", err.Error())
	}
	if findings := (validate.SpecVersion{Version: spec}).Validate(documents); len(findings) > 0 {
		return domain.PortableAgent{}, documentError("document.charterSpecVersion", findings[0].Message)
	}
	var definition model.AgentDefinition
	if err = strictJSON(canonical, &definition); err != nil {
		return domain.PortableAgent{}, documentError("document", err.Error())
	}
	if len(definition.Extensions) != 3 || definition.Extensions[ExtensionKey] == nil || definition.Extensions[AgentExtensionKey] == nil || definition.Extensions[DigestExtensionKey] == nil {
		return domain.PortableAgent{}, documentError("document.extensions", "must be exactly "+ExtensionKey+", "+AgentExtensionKey+", and "+DigestExtensionKey)
	}
	var claimed string
	if err = json.Unmarshal(definition.Extensions[DigestExtensionKey], &claimed); err != nil {
		return domain.PortableAgent{}, documentError("document.extensions."+DigestExtensionKey, "must be a string")
	}
	body, err := withoutDigest(canonical)
	if err != nil {
		return domain.PortableAgent{}, documentError("document", err.Error())
	}
	digest := documentDigest(body)
	if claimed != digest {
		return domain.PortableAgent{}, documentError("document.extensions."+DigestExtensionKey, "does not match the document")
	}
	var release domain.AgentDefinition
	if err = strictJSON(definition.Extensions[ExtensionKey], &release); err != nil {
		return domain.PortableAgent{}, documentError("document.extensions."+ExtensionKey, err.Error())
	}
	var extension agentExtension
	if err = strictJSON(definition.Extensions[AgentExtensionKey], &extension); err != nil {
		return domain.PortableAgent{}, documentError("document.extensions."+AgentExtensionKey, err.Error())
	}
	if extension.Format != domain.PortableAgentFormat {
		return domain.PortableAgent{}, documentError("document.extensions."+AgentExtensionKey+".format", "must be "+domain.PortableAgentFormat)
	}
	if count := utf8.RuneCountInString(definition.Namespace); count == 0 || count > maxNamespace {
		return domain.PortableAgent{}, documentError("document.namespace", fmt.Sprintf("must contain 1 to %d characters", maxNamespace))
	}
	if count := utf8.RuneCountInString(release.Version); count == 0 || count > maxVersion {
		return domain.PortableAgent{}, documentError("document.extensions."+ExtensionKey+".version", fmt.Sprintf("must contain 1 to %d characters", maxVersion))
	}
	enterprise := ""
	if definition.EnterpriseID != nil {
		enterprise = definition.EnterpriseID.ID
	}
	agent := domain.PortableAgent{
		AgentTypeID: extension.AgentType, TypeVersion: extension.TypeVersion, TypeDigest: extension.TypeDigest,
		DisplayName: extension.DisplayName, Models: extension.Models, Extension: extension.Extension,
		Bindings: extension.Bindings, Guardrails: extension.Guardrails, DraftOrigin: extension.DraftOrigin,
		Release: release, Settings: extension.Settings,
	}
	rendered, err := d.render(definition.Namespace, enterprise, definition.CharterSpecVersion, agent)
	if err != nil {
		return domain.PortableAgent{}, documentError("document", err.Error())
	}
	if !bytes.Equal(rendered, body) {
		return domain.PortableAgent{}, documentError(firstDifference(body, rendered), "does not match what the embedded definition and agent render")
	}
	agent.Namespace, agent.EnterpriseID, agent.SpecVersion = definition.Namespace, enterprise, definition.CharterSpecVersion
	agent.Document = domain.CharterObjectRef{Kind: string(model.KindAgentDefinition), ID: definition.ID, Namespace: definition.Namespace, External: true}
	agent.Digest = digest
	return agent, nil
}

// render builds the canonical document without its digest; Encode and Decode share it, so a decoded
// document whose Charter members were not derived from its embedded release is refused.
func (d *AgentDocuments) render(namespace, enterprise, spec string, agent domain.PortableAgent) ([]byte, error) {
	release := agent.Release
	identity := charterAgentID(release.AgentID)
	id := identity + "-v" + release.Version
	if release.AgentID == "" || release.Version == "" || len(id) > maxDocumentID || !charterIdentifier.MatchString(id) {
		return nil, fmt.Errorf("%w: agent %q version %q does not form a Charter identifier", domain.ErrConflict, release.AgentID, release.Version)
	}
	definition := model.AgentDefinition{
		CharterSpecVersion: spec, Namespace: namespace, ID: id, Kind: model.KindAgentDefinition, Name: agent.DisplayName,
		AgentIdentityID: model.Ref{ID: identity}, DefinitionVersion: release.Version,
	}
	if enterprise != "" {
		definition.EnterpriseID = &model.Ref{ID: enterprise}
	}
	if raw, governed := release.Sections[domain.GovernanceSection]; governed {
		var governance domain.AgentGovernance
		if err := strictJSON(raw, &governance); err != nil {
			return nil, fmt.Errorf("%w: the frozen governance section does not decode: %v", domain.ErrConflict, err)
		}
		applyGovernance(&definition, governance)
	}
	if release.DerivedFrom != nil {
		definition.DerivedFrom = &model.Provenance{Source: objectRef(release.DerivedFrom.Source), Version: release.DerivedFrom.Version, Digest: release.DerivedFrom.Digest}
	}
	scout, err := json.Marshal(release)
	if err != nil {
		return nil, err
	}
	extension, err := json.Marshal(agentExtension{
		Format: domain.PortableAgentFormat, AgentType: agent.AgentTypeID, TypeVersion: agent.TypeVersion, TypeDigest: agent.TypeDigest,
		DisplayName: agent.DisplayName, Models: agent.Models, Extension: agent.Extension, Bindings: nonNil(agent.Bindings),
		Guardrails: agent.Guardrails, DraftOrigin: agent.DraftOrigin, Settings: nonNil(agent.Settings),
		CharterMissing: charterMissing(definition),
	})
	if err != nil {
		return nil, err
	}
	definition.Extensions = map[string]json.RawMessage{ExtensionKey: scout, AgentExtensionKey: extension}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return nil, err
	}
	var members map[string]any
	if err = useNumber(encoded, &members); err != nil {
		return nil, err
	}
	for _, member := range governanceMembers {
		if emptyMember(members[member]) {
			delete(members, member)
		}
	}
	body, err := canonicalValue(members)
	if err != nil {
		return nil, err
	}
	if len(charterMissing(definition)) == 0 {
		if err = d.conformant(body); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// conformant runs Charter's structural schema over a document that claims full conformance.
func (d *AgentDocuments) conformant(body []byte) error {
	d.once.Do(func() { d.structural, d.err = validate.NewStructural() })
	if d.err != nil {
		return fmt.Errorf("load charter schemas: %w", d.err)
	}
	parsed, err := corpus.Parser{}.Parse(body)
	if err != nil {
		return err
	}
	if findings := d.structural.ValidateDocument(parsed); len(findings) > 0 {
		return fmt.Errorf("%w: the document breaks the Charter schema at %s: %s", domain.ErrConflict, findings[0].Path, findings[0].Message)
	}
	return nil
}

// charterAgentID keeps an agent id that is a Charter identifier; any other id has each disallowed
// character replaced by '-' and a digest of the original appended, so distinct ids stay distinct.
func charterAgentID(agentID string) string {
	if agentID == "" || charterIdentifier.MatchString(agentID) {
		return agentID
	}
	safe := []byte(strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && (r == '.' || r == '_' || r == ':' || r == '-' || alphanumeric(byte(r))) {
			return r
		}
		return '-'
	}, agentID))
	if !alphanumeric(safe[0]) {
		safe = append([]byte("a"), safe...)
	}
	sum := sha256.Sum256([]byte(agentID))
	return string(safe) + "." + hex.EncodeToString(sum[:4])
}

func alphanumeric(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func applyGovernance(definition *model.AgentDefinition, governance domain.AgentGovernance) {
	definition.Purpose = governance.Purpose
	if governance.Accountable != nil {
		definition.Accountable = objectRef(*governance.Accountable)
	}
	definition.ResponsibilityIDs = refs(governance.ResponsibilityIDs)
	definition.Triggers = governance.Triggers
	if len(definition.Triggers) == 0 {
		seen := map[string]bool{}
		for _, channel := range governance.TriggerChannels {
			if channel.Trigger != "" && !seen[channel.Trigger] {
				seen[channel.Trigger] = true
				definition.Triggers = append(definition.Triggers, channel.Trigger)
			}
		}
	}
	definition.RequiredInputs = governance.RequiredInputs
	definition.ExpectedOutcomes = governance.ExpectedOutcomes
	definition.CapabilityIDs = refs(governance.CapabilityIDs)
	definition.Policies = governance.Policies
	definition.ConfidenceBoundaries = governance.ConfidenceBoundaries
	definition.EscalationConditions = governance.EscalationConditions
}

// charterMissing lists, in schema order, the members Charter requires that the definition leaves empty.
func charterMissing(definition model.AgentDefinition) []string {
	var missing []string
	if definition.EnterpriseID == nil {
		missing = append(missing, "enterpriseId")
	}
	present := []bool{
		definition.Purpose != "", definition.Accountable.ID != "", len(definition.ResponsibilityIDs) > 0, len(definition.Triggers) > 0,
		len(definition.RequiredInputs) > 0, len(definition.ExpectedOutcomes) > 0, len(definition.CapabilityIDs) > 0,
		len(definition.Policies) > 0, len(definition.ConfidenceBoundaries) > 0, len(definition.EscalationConditions) > 0,
	}
	for index, member := range governanceMembers {
		if !present[index] {
			missing = append(missing, member)
		}
	}
	return missing
}

func emptyMember(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		id, _ := typed["id"].(string)
		return id == ""
	}
	return false
}

func objectRef(ref domain.CharterObjectRef) model.ObjectRef {
	return model.ObjectRef{Kind: model.Kind(ref.Kind), ID: ref.ID, Namespace: ref.Namespace, External: ref.External}
}

func refs(values []domain.CharterRef) []model.Ref {
	if len(values) == 0 {
		return nil
	}
	out := make([]model.Ref, 0, len(values))
	for _, value := range values {
		out = append(out, model.Ref{Namespace: value.Namespace, ID: value.ID})
	}
	return out
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

// canonicalDocument checks raw holds exactly one JSON object and returns it in canonical form:
// sorted object keys, arrays in order, numbers as written, no HTML escaping, no insignificant space.
func canonicalDocument(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("is required")
	}
	var value map[string]any
	if err := useNumber(raw, &value); err != nil {
		return nil, fmt.Errorf("must be one JSON object: %v", err)
	}
	return canonicalValue(value)
}

func useNumber(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing content after the JSON value")
	}
	return nil
}

func canonicalValue(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func strictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing content after the JSON value")
	}
	return nil
}

func documentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func withDigest(body []byte, digest string) ([]byte, error) {
	var members map[string]any
	if err := useNumber(body, &members); err != nil {
		return nil, err
	}
	extensions, _ := members["extensions"].(map[string]any)
	extensions[DigestExtensionKey] = digest
	return canonicalValue(members)
}

func withoutDigest(canonical []byte) ([]byte, error) {
	var members map[string]any
	if err := useNumber(canonical, &members); err != nil {
		return nil, err
	}
	if extensions, ok := members["extensions"].(map[string]any); ok {
		delete(extensions, DigestExtensionKey)
	}
	return canonicalValue(members)
}

// firstDifference names the first member path, in key order, where two canonical documents differ.
func firstDifference(left, right []byte) string {
	var a, b any
	if useNumber(left, &a) != nil || useNumber(right, &b) != nil {
		return "document"
	}
	return difference("document", a, b)
}

func difference(path string, left, right any) string {
	leftMap, leftIsMap := left.(map[string]any)
	rightMap, rightIsMap := right.(map[string]any)
	if !leftIsMap || !rightIsMap {
		return path
	}
	keys := make([]string, 0, len(leftMap)+len(rightMap))
	for key := range leftMap {
		keys = append(keys, key)
	}
	for key := range rightMap {
		if _, shared := leftMap[key]; !shared {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !reflect.DeepEqual(leftMap[key], rightMap[key]) {
			return difference(path+"."+key, leftMap[key], rightMap[key])
		}
	}
	return path
}

func documentError(field, message string) error {
	return &controlplane.StudioValidationError{Fields: []domain.AgentFieldError{{Field: field, Message: message}}}
}
