package controlplane

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nauticana/scout/domain"
)

// DefaultMaxGovernanceItems is Scout's cap on one Studio-authored governance list when
// StudioGovernance.MaxItems is zero.
const DefaultMaxGovernanceItems = 32

// Charter's bounds on AgentDefinition values; the binding and link bounds are Scout's.
const (
	maxGovernanceText   = 500
	maxCharterID        = 120
	maxCharterNamespace = 200
	maxCharterKind      = 60
	maxCharterVersion   = 60
	maxTriggerBinding   = 200
	maxGovernanceLink   = 2000
)

var charterIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// valueBounds selects the bounds a governance value is held to. Studio holds what it authors to its
// own; a value the enterprise supplies meets Charter's schema alone, since nobody in Studio can edit it.
type valueBounds int

const (
	// studioBounds adds Scout's item cap, distinct statements, and no surrounding spaces to Charter's.
	studioBounds valueBounds = iota
	charterBounds
)

// boundsOf holds each field the enterprise owns to Charter's bounds and every other to Studio's.
func boundsOf(owned []domain.GovernanceField) func(domain.GovernanceField) valueBounds {
	return func(field domain.GovernanceField) valueBounds {
		if slices.Contains(owned, field) {
			return charterBounds
		}
		return studioBounds
	}
}

// effectiveGovernance is what a release freezes: each field the snapshot supplies, Studio's draft
// for the rest, and the Charter triggers of Studio's trigger channels unless the enterprise owns them.
func effectiveGovernance(draft domain.AgentGovernance, snapshot *domain.GovernanceSnapshot) domain.AgentGovernance {
	effective := draft
	effective.Triggers, effective.Enterprise = triggerTexts(draft.TriggerChannels), nil
	if snapshot == nil {
		return effective
	}
	owned := suppliedFields(snapshot.Governance)
	for _, field := range owned {
		takeField(&effective, snapshot.Governance, field)
	}
	effective.Enterprise = &domain.GovernanceSnapshotRef{
		Document: snapshot.Document, Version: snapshot.Version, Digest: snapshot.Digest, Fields: owned,
	}
	return effective
}

// studioDraft is the part of a stored draft Studio still owns.
func studioDraft(stored domain.AgentGovernance, snapshot *domain.GovernanceSnapshot) domain.AgentGovernance {
	if snapshot != nil {
		for _, field := range suppliedFields(snapshot.Governance) {
			takeField(&stored, domain.AgentGovernance{}, field)
		}
	}
	return stored
}

// suppliedFields lists, in Charter's order, the governance fields g sets.
func suppliedFields(g domain.AgentGovernance) []domain.GovernanceField {
	fields := []domain.GovernanceField{}
	for _, field := range domain.CharterGovernanceFields() {
		if hasField(g, field) {
			fields = append(fields, field)
		}
	}
	return fields
}

func hasField(g domain.AgentGovernance, field domain.GovernanceField) bool {
	switch field {
	case domain.GovernancePurpose:
		return g.Purpose != ""
	case domain.GovernanceAccountable:
		return g.Accountable != nil
	case domain.GovernanceResponsibilities:
		return len(g.ResponsibilityIDs) > 0
	case domain.GovernanceTriggers:
		return len(g.Triggers) > 0
	case domain.GovernanceRequiredInputs:
		return len(g.RequiredInputs) > 0
	case domain.GovernanceExpectedOutcomes:
		return len(g.ExpectedOutcomes) > 0
	case domain.GovernanceCapabilities:
		return len(g.CapabilityIDs) > 0
	case domain.GovernancePolicies:
		return len(g.Policies) > 0
	case domain.GovernanceConfidenceBoundaries:
		return len(g.ConfidenceBoundaries) > 0
	case domain.GovernanceEscalationConditions:
		return len(g.EscalationConditions) > 0
	}
	return false
}

// takeField sets one field of dst to src's; triggers carry their channels.
func takeField(dst *domain.AgentGovernance, src domain.AgentGovernance, field domain.GovernanceField) {
	switch field {
	case domain.GovernancePurpose:
		dst.Purpose = src.Purpose
	case domain.GovernanceAccountable:
		dst.Accountable = src.Accountable
	case domain.GovernanceResponsibilities:
		dst.ResponsibilityIDs = src.ResponsibilityIDs
	case domain.GovernanceTriggers:
		dst.Triggers, dst.TriggerChannels = src.Triggers, src.TriggerChannels
	case domain.GovernanceRequiredInputs:
		dst.RequiredInputs = src.RequiredInputs
	case domain.GovernanceExpectedOutcomes:
		dst.ExpectedOutcomes = src.ExpectedOutcomes
	case domain.GovernanceCapabilities:
		dst.CapabilityIDs = src.CapabilityIDs
	case domain.GovernancePolicies:
		dst.Policies = src.Policies
	case domain.GovernanceConfidenceBoundaries:
		dst.ConfidenceBoundaries = src.ConfidenceBoundaries
	case domain.GovernanceEscalationConditions:
		dst.EscalationConditions = src.EscalationConditions
	}
}

// triggerTexts lists the distinct Charter triggers of channels in the order they first appear.
func triggerTexts(channels []domain.AgentTrigger) []string {
	var texts []string
	for _, channel := range channels {
		if !slices.Contains(texts, channel.Trigger) {
			texts = append(texts, channel.Trigger)
		}
	}
	return texts
}

// changedFields names the governance fields a save changes, triggers including their channels.
func changedFields(before, after domain.AgentGovernance) ([]string, error) {
	var changed []string
	for _, field := range domain.CharterGovernanceFields() {
		var left, right domain.AgentGovernance
		takeField(&left, before, field)
		takeField(&right, after, field)
		leftJSON, err := json.Marshal(left)
		if err != nil {
			return nil, err
		}
		rightJSON, err := json.Marshal(right)
		if err != nil {
			return nil, err
		}
		if string(leftJSON) != string(rightJSON) {
			changed = append(changed, string(field))
		}
	}
	return changed, nil
}

// fieldProblems collects the field errors of one governance check.
type fieldProblems []domain.AgentFieldError

func (p *fieldProblems) add(field, message string) {
	*p = append(*p, domain.AgentFieldError{Field: field, Message: message})
}

// text refuses an empty or overlong value, and under Studio's bounds a padded one.
func (p *fieldProblems) text(field, value string, limit int, bounds valueBounds) {
	if value == "" || utf8.RuneCountInString(value) > limit {
		p.add(field, fmt.Sprintf("must contain 1 to %d characters", limit))
	} else if bounds == studioBounds && strings.TrimSpace(value) != value {
		p.add(field, "must not have surrounding spaces")
	}
}

func (p *fieldProblems) identifier(field, value string) {
	if len(value) > maxCharterID || !charterIdentifier.MatchString(value) {
		p.add(field, fmt.Sprintf("must be a Charter identifier of at most %d letters, digits, and . _ : -", maxCharterID))
	}
}

func (p *fieldProblems) namespace(field, value string, bounds valueBounds) {
	if value != "" {
		p.text(field, value, maxCharterNamespace, bounds)
	}
}

// count holds a Studio list to Scout's item cap of maxItems; Charter caps no list.
func (p *fieldProblems) count(field string, items int, bounds valueBounds, maxItems int) bool {
	if bounds == studioBounds && items > maxItems {
		p.add(field, fmt.Sprintf("must not list more than %d items", maxItems))
		return false
	}
	return true
}

// texts refuses a repeated statement only under Studio's bounds; Charter's statement lists allow one.
func (p *fieldProblems) texts(field string, values []string, bounds valueBounds, maxItems int) {
	if !p.count(field, len(values), bounds, maxItems) {
		return
	}
	for index, value := range values {
		item := fmt.Sprintf("%s[%d]", field, index)
		p.text(item, value, maxGovernanceText, bounds)
		if bounds == studioBounds && slices.Contains(values[:index], value) {
			p.add(item, "duplicate")
		}
	}
}

// refs refuses a repeated reference under both bounds; Charter's reference lists are unique.
func (p *fieldProblems) refs(field string, refs []domain.CharterRef, bounds valueBounds, maxItems int) {
	if !p.count(field, len(refs), bounds, maxItems) {
		return
	}
	for index, ref := range refs {
		item := fmt.Sprintf("%s[%d]", field, index)
		p.identifier(item+".id", ref.ID)
		p.namespace(item+".namespace", ref.Namespace, bounds)
		if slices.Contains(refs[:index], ref) {
			p.add(item, "duplicate")
		}
	}
}

// objectRef checks a Charter objectRef; kinds, when given, are the only kinds it may name.
func (p *fieldProblems) objectRef(field string, ref domain.CharterObjectRef, bounds valueBounds, kinds ...string) {
	switch {
	case len(kinds) > 0 && !slices.Contains(kinds, ref.Kind):
		p.add(field+".kind", "must be "+strings.Join(kinds, " or "))
	case len(kinds) == 0:
		p.text(field+".kind", ref.Kind, maxCharterKind, bounds)
	}
	p.identifier(field+".id", ref.ID)
	p.namespace(field+".namespace", ref.Namespace, bounds)
}

// channels refuses an unknown channel, a binding on a channel that has none or none where one is
// needed, a repeated declaration, and a schedule or topic that starts two triggers.
func (p *fieldProblems) channels(channels []domain.AgentTrigger, maxItems int) {
	if !p.count("triggerChannels", len(channels), studioBounds, maxItems) {
		return
	}
	for index, channel := range channels {
		item := fmt.Sprintf("triggerChannels[%d]", index)
		p.text(item+".trigger", channel.Trigger, maxGovernanceText, studioBounds)
		switch channel.Channel {
		case domain.TriggerConversation, domain.TriggerMCP, domain.TriggerAPI:
			if channel.Binding != "" {
				p.add(item+".binding", "only a schedule or event trigger names a binding")
			}
		case domain.TriggerSchedule, domain.TriggerEvent:
			if channel.Binding == "" || strings.TrimSpace(channel.Binding) != channel.Binding ||
				utf8.RuneCountInString(channel.Binding) > maxTriggerBinding || strings.ContainsFunc(channel.Binding, unicode.IsControl) {
				p.add(item+".binding", fmt.Sprintf("a schedule or event trigger names its schedule or topic in 1 to %d characters", maxTriggerBinding))
			}
		default:
			p.add(item+".channel", "must be conversation, mcp, api, schedule, or event")
		}
		for _, earlier := range channels[:index] {
			if earlier == channel {
				p.add(item, "duplicate")
				break
			}
			if channel.Binding != "" && earlier.Channel == channel.Channel && earlier.Binding == channel.Binding {
				p.add(item+".binding", "a schedule or topic starts one trigger")
				break
			}
		}
	}
}

// governanceProblems reports each malformed value or reference of g; owned are the fields the
// enterprise supplied, which only Charter's bounds apply to, and maxItems caps every other list.
func governanceProblems(g domain.AgentGovernance, owned []domain.GovernanceField, maxItems int) []domain.AgentFieldError {
	var problems fieldProblems
	bounds := boundsOf(owned)
	if g.Purpose != "" {
		problems.text(string(domain.GovernancePurpose), g.Purpose, maxGovernanceText, bounds(domain.GovernancePurpose))
	}
	if g.Accountable != nil {
		problems.objectRef(string(domain.GovernanceAccountable), *g.Accountable, bounds(domain.GovernanceAccountable), "Position", "OrganizationUnit")
	}
	for _, list := range []struct {
		field  domain.GovernanceField
		values []string
	}{
		{domain.GovernanceTriggers, g.Triggers},
		{domain.GovernanceRequiredInputs, g.RequiredInputs},
		{domain.GovernanceExpectedOutcomes, g.ExpectedOutcomes},
		{domain.GovernancePolicies, g.Policies},
		{domain.GovernanceConfidenceBoundaries, g.ConfidenceBoundaries},
		{domain.GovernanceEscalationConditions, g.EscalationConditions},
	} {
		problems.texts(string(list.field), list.values, bounds(list.field), maxItems)
	}
	problems.refs(string(domain.GovernanceResponsibilities), g.ResponsibilityIDs, bounds(domain.GovernanceResponsibilities), maxItems)
	problems.refs(string(domain.GovernanceCapabilities), g.CapabilityIDs, bounds(domain.GovernanceCapabilities), maxItems)
	problems.channels(g.TriggerChannels, maxItems)
	return problems
}

// draftProblems reports what keeps a save from replacing Studio's governance: a malformed value,
// a field the enterprise snapshot owns, or a field only publication derives.
func draftProblems(draft domain.AgentGovernance, snapshot *domain.GovernanceSnapshot, maxItems int) []domain.AgentFieldError {
	problems := fieldProblems(governanceProblems(draft, nil, maxItems))
	if len(draft.Triggers) > 0 {
		problems.add("triggers", "Studio declares its triggers in triggerChannels")
	}
	if draft.Enterprise != nil {
		problems.add("enterprise", "only the enterprise source names its snapshot")
	}
	if snapshot == nil {
		return problems
	}
	for _, field := range suppliedFields(snapshot.Governance) {
		switch {
		case field == domain.GovernanceTriggers && len(draft.TriggerChannels) > 0:
			problems.add("triggerChannels", "the enterprise source owns the triggers")
		case field != domain.GovernanceTriggers && hasField(draft, field):
			problems.add(string(field), "owned by the enterprise source")
		}
	}
	return problems
}

// frozenProblems reports what keeps a governance from being frozen into a release: a malformed
// value, triggers that are not those of the trigger channels, or an incomplete enterprise reference.
func frozenProblems(g domain.AgentGovernance, maxItems int) []domain.AgentFieldError {
	var owned []domain.GovernanceField
	if g.Enterprise != nil {
		owned = g.Enterprise.Fields
	}
	problems := fieldProblems(governanceProblems(g, owned, maxItems))
	triggersOwned := false
	if g.Enterprise != nil {
		problems = append(problems, snapshotRefProblems(*g.Enterprise)...)
		triggersOwned = slices.Contains(g.Enterprise.Fields, domain.GovernanceTriggers)
	}
	switch {
	case triggersOwned && len(g.TriggerChannels) > 0:
		problems.add("triggerChannels", "the enterprise source owns the triggers")
	case !triggersOwned && !slices.Equal(g.Triggers, triggerTexts(g.TriggerChannels)):
		problems.add("triggers", "must be the triggers of triggerChannels")
	}
	return problems
}

// missingFields names each required field g lacks.
func missingFields(g domain.AgentGovernance, required []domain.GovernanceField) []domain.AgentFieldError {
	var problems fieldProblems
	for _, field := range required {
		if !hasField(g, field) {
			problems.add(string(field), "required by the active Charter profile")
		}
	}
	return problems
}

func snapshotRefProblems(ref domain.GovernanceSnapshotRef) []domain.AgentFieldError {
	var problems fieldProblems
	problems.objectRef("enterprise.document", ref.Document, studioBounds)
	problems.text("enterprise.version", ref.Version, maxCharterVersion, studioBounds)
	problems.text("enterprise.digest", ref.Digest, maxGovernanceText, studioBounds)
	for index, field := range ref.Fields {
		if !slices.Contains(domain.CharterGovernanceFields(), field) || slices.Contains(ref.Fields[:index], field) {
			problems.add(fmt.Sprintf("enterprise.fields[%d]", index), "must be a distinct Charter governance field")
		}
	}
	return problems
}

// snapshotProblems reports what makes a source's snapshot unusable: an incomplete reference, a link
// a browser should not follow, Scout's own fields, which no enterprise document carries, or a value
// Charter's schema refuses.
func snapshotProblems(snapshot domain.GovernanceSnapshot, maxItems int) []domain.AgentFieldError {
	problems := fieldProblems(snapshotRefProblems(domain.GovernanceSnapshotRef{
		Document: snapshot.Document, Version: snapshot.Version, Digest: snapshot.Digest,
	}))
	problems = append(problems, governanceProblems(snapshot.Governance, domain.CharterGovernanceFields(), maxItems)...)
	if snapshot.Link != "" {
		link, err := url.Parse(snapshot.Link)
		if err != nil || len(snapshot.Link) > maxGovernanceLink || link.Host == "" || link.Scheme != "https" && link.Scheme != "http" {
			problems.add("link", "must be an absolute http or https URL")
		}
	}
	if len(snapshot.Governance.TriggerChannels) > 0 || snapshot.Governance.Enterprise != nil {
		problems.add("governance", "a snapshot carries Charter fields only")
	}
	return problems
}
