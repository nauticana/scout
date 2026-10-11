package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
	"github.com/nauticana/scout/service/guardrail"
)

// GuardrailSectionName is the definition section a release's own guardrail rules freeze under.
const GuardrailSectionName = "guardrails"

const (
	qGuardrailDraftGet  = "scout_studio_guardrail_draft_get"
	qGuardrailDraftPut  = "scout_studio_guardrail_draft_put"
	qGuardrailDraftDrop = "scout_studio_guardrail_draft_drop"
	qGuardrailPin       = "scout_studio_guardrail_pin"

	studioGuardrailCatalogID = "scout.controlplane.studio_guardrails"
	maxGuardrailRuleID       = 80
)

var studioGuardrailQueries = map[string]string{
	qGuardrailDraftGet: `SELECT rules, rules_digest FROM agent_draft_guardrail WHERE partner_id = ? AND agent_id = ?`,
	qGuardrailDraftPut: `
INSERT INTO agent_draft_guardrail (partner_id, agent_id, rules, rules_digest, modified_by)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (partner_id, agent_id) DO UPDATE
   SET rules = EXCLUDED.rules, rules_digest = EXCLUDED.rules_digest,
       modified_by = EXCLUDED.modified_by, modified_at = CURRENT_TIMESTAMP`,
	qGuardrailDraftDrop: `DELETE FROM agent_draft_guardrail WHERE partner_id = ? AND agent_id = ?`,
	// A release names its guardrail version once, in the transaction that writes the release.
	qGuardrailPin: `
UPDATE agent_version SET guardrail_version = ?
 WHERE partner_id = ? AND agent_id = ? AND agent_version = ? AND guardrail_version IS NULL
RETURNING agent_version`,
	qStudioBumpDraft:  studioQueries[qStudioBumpDraft],
	qStudioAudit:      studioQueries[qStudioAudit],
	qGuardrailPublish: runtimePolicyQueries[qGuardrailPublish],
	qGuardrailGet:     runtimePolicyQueries[qGuardrailGet],
}

// StudioGuardrails edits an agent's enforced guardrail rules. Its own rules sit behind the draft
// revision and may only add to, or tighten by id, what it inherits: the baseline, the platform and
// tenant restriction layers, and its type's rules. Compose it as a DefinitionSection, which freezes
// the type's and agent's rules, and as a ReleaseWriter, which stores them as the release's
// guardrail_config version and names it on agent_version in the publish transaction.
type StudioGuardrails struct {
	// Studio authorizes every call, reads the agent's draft and type, and owns its revision and audit.
	Studio *StudioService
	// Rules validates every rule set; required.
	Rules contract.GuardrailRuleCompiler
	// Restrictions reads the platform and tenant layers; required, since no inherited rule is assumed absent.
	Restrictions contract.RestrictionLayerReader
	// Baseline is the enforcer's release-independent rules; set, they are shown and checked too.
	Baseline []domain.GuardrailRule
	// Events pages safety events; only SafetyEvents needs it.
	Events contract.SafetyEventQuery

	once sync.Once
	qs   keelport.QueryService
}

var (
	_ contract.StudioGuardrailBackend = (*StudioGuardrails)(nil)
	_ contract.DefinitionSection      = (*StudioGuardrails)(nil)
	_ ReleaseWriter                   = (*StudioGuardrails)(nil)
)

// guardrailSection is the frozen section: the canonical rule envelope and its digest, which is
// also the release's guardrail_config version.
type guardrailSection struct {
	Rules       json.RawMessage `json:"rules"`
	RulesDigest string          `json:"rules_digest"`
}

func (g *StudioGuardrails) ready() error {
	if g.Studio == nil || g.Rules == nil || g.Restrictions == nil {
		return fmt.Errorf("studio guardrails: studio service, rule compiler, and restriction layers are required")
	}
	return nil
}

func (g *StudioGuardrails) queries(ctx context.Context) (keelport.QueryService, error) {
	if g.Studio == nil || g.Studio.DB == nil {
		return nil, fmt.Errorf("studio guardrails: a studio service with a database is required")
	}
	g.once.Do(func() { g.qs = g.Studio.DB.GetQueryService(ctx, studioGuardrailQueries) })
	return g.qs, nil
}

// Guardrails returns the layers the agent inherits and its own rules; own rules that no longer
// fit what it inherits are reported in Problem.
func (g *StudioGuardrails) Guardrails(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentGuardrails, error) {
	if err := g.ready(); err != nil {
		return domain.AgentGuardrails{}, err
	}
	if err := g.Studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentGuardrails{}, err
	}
	draft, err := g.Studio.getDraft(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	qs, err := g.queries(ctx)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	own, err := ownRules(ctx, qs, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	inherited, err := g.inherited(ctx, actor.TenantID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	result := domain.AgentGuardrails{AgentID: agentID, Inherited: inherited, Rules: own, DraftRevision: draft.ExpectedDraftRevision}
	if err = g.check(ctx, inherited, own); guardrailProblem(err) {
		result.Problem = err.Error()
	} else if err != nil {
		return domain.AgentGuardrails{}, err
	}
	return result, nil
}

// SaveGuardrails replaces the agent's own rules behind its draft revision. They are compiled,
// alone and with the type's, and checked against every inherited layer first, so a rule set
// that loosens an inherited rule is refused unwritten.
func (g *StudioGuardrails) SaveGuardrails(ctx context.Context, actor domain.StudioActor, change domain.AgentGuardrailChange) (domain.AgentGuardrails, error) {
	if err := g.ready(); err != nil {
		return domain.AgentGuardrails{}, err
	}
	draft, err := g.Studio.editable(ctx, actor, domain.StudioEdit, change.AgentID)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	if err = validateGuardrailChange(change); err != nil {
		return domain.AgentGuardrails{}, err
	}
	envelope, digest, own, err := canonicalRules(change.Rules)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	inherited, err := g.inherited(ctx, actor.TenantID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentGuardrails{}, err
	}
	if err = g.check(ctx, inherited, own); err != nil {
		return domain.AgentGuardrails{}, err
	}

	tx, err := g.Studio.DB.BeginTx(ctx, studioGuardrailQueries)
	if err != nil {
		return domain.AgentGuardrails{}, fmt.Errorf("begin guardrail transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	bumped, err := tx.Query(ctx, qStudioBumpDraft, actorUser(actor), actor.TenantID, change.AgentID, change.ExpectedDraftRevision)
	if err != nil {
		return domain.AgentGuardrails{}, fmt.Errorf("bump draft revision: %w", err)
	}
	if len(bumped.Rows) == 0 {
		return domain.AgentGuardrails{}, domain.ErrRevisionConflict
	}
	detail := "removed"
	if len(own) == 0 {
		_, err = tx.Query(ctx, qGuardrailDraftDrop, actor.TenantID, change.AgentID)
	} else {
		detail = fmt.Sprintf("%d rules, digest %s", len(own), digest[:12])
		_, err = tx.Query(ctx, qGuardrailDraftPut, actor.TenantID, change.AgentID, string(envelope), digest, actorUser(actor))
	}
	if err != nil {
		return domain.AgentGuardrails{}, fmt.Errorf("store agent guardrails: %w", err)
	}
	if err = auditStudio(ctx, tx, actor, change.AgentID, "GUARDRAILS", detail); err != nil {
		return domain.AgentGuardrails{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentGuardrails{}, fmt.Errorf("commit agent guardrails: %w", err)
	}
	committed = true
	return g.Guardrails(ctx, actor, change.AgentID)
}

// SafetyEvents pages the agent's safety events newest first, exactly as the sink stored them.
func (g *StudioGuardrails) SafetyEvents(ctx context.Context, actor domain.StudioActor, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error) {
	if g.Studio == nil || g.Events == nil {
		return nil, fmt.Errorf("studio guardrails: studio service and safety event query are required")
	}
	if err := g.Studio.Authorize(ctx, actor, domain.StudioView, filter.AgentID); err != nil {
		return nil, err
	}
	return g.Events.SafetyEvents(ctx, actor.TenantID, filter)
}

// Name is the definition section the release's rules freeze under.
func (g *StudioGuardrails) Name() string { return GuardrailSectionName }

// Section freezes what a release of the draft enforces in its own layer: the type's rules, each
// replaced in place by the agent's rule of the same id, then the agent's others. A draft with
// none contributes no section. It never refuses a rule; WriteRelease checks the frozen set.
func (g *StudioGuardrails) Section(ctx context.Context, tenantID int64, draft domain.AgentDraft) (json.RawMessage, error) {
	qs, err := g.queries(ctx)
	if err != nil {
		return nil, err
	}
	typeRules, err := g.typeRules(ctx, draft.AgentTypeID)
	if err != nil {
		return nil, err
	}
	own, err := ownRules(ctx, qs, tenantID, draft.AgentID)
	if err != nil {
		return nil, err
	}
	rules := releaseRules(typeRules, own)
	if len(rules) == 0 {
		return nil, nil
	}
	envelope, digest, _, err := canonicalRules(rules)
	if err != nil {
		return nil, err
	}
	return json.Marshal(guardrailSection{Rules: envelope, RulesDigest: digest})
}

// WriteRelease stores the rule set the definition froze, or the empty set when it froze none, as
// a guardrail_config version and names it on the release, inside the publish or restore
// transaction. A publish is checked against every layer the agent inherits now; a restore
// keeps the rules its source was published with but must still carry the type's current rules.
func (g *StudioGuardrails) WriteRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if err := g.ready(); err != nil {
		return err
	}
	catalog, ok := tx.(keelport.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the publish transaction cannot bind another query catalog", domain.ErrNotReady)
	}
	qs := catalog.QueryService(studioGuardrailCatalogID, studioGuardrailQueries)
	config, rules, frozen, err := releaseGuardrails(definition)
	if err != nil {
		return err
	}
	if _, err = g.Rules.Validate(ctx, config); err != nil {
		return fmt.Errorf("release guardrails: %w", err)
	}
	if definition.RestoredFromVersion == "" {
		err = g.checkPublish(ctx, qs, tenantID, definition, rules, frozen)
	} else {
		err = g.checkRestore(ctx, definition, rules)
	}
	if err != nil {
		return err
	}
	if err = publishGuardrailConfig(ctx, qs, tenantID, definition.AgentID, config); err != nil {
		return err
	}
	pinned, err := qs.Query(ctx, qGuardrailPin, config.Version, tenantID, definition.AgentID, definition.Version)
	if err != nil {
		return fmt.Errorf("name release guardrail version: %w", err)
	}
	if len(pinned.Rows) == 0 {
		return fmt.Errorf("%w: release %s of %q is missing or already names a guardrail version", domain.ErrConflict, definition.Version, definition.AgentID)
	}
	return nil
}

// checkPublish refuses a release that would enforce less than the agent inherits: no frozen rule
// set although the draft or its type has rules, a dropped type rule, or a rule that reuses an
// inherited rule's id without tightening it.
func (g *StudioGuardrails) checkPublish(ctx context.Context, qs keelport.QueryService, tenantID int64, definition domain.AgentDefinition, rules []domain.GuardrailRule, frozen bool) error {
	inherited, err := g.inherited(ctx, tenantID, definition.AgentTypeID)
	if err != nil {
		return err
	}
	typeRules := layerRules(inherited, domain.GuardrailLayerType)
	if !frozen {
		own, err := ownRules(ctx, qs, tenantID, definition.AgentID)
		if err != nil {
			return err
		}
		if len(own) > 0 || len(typeRules) > 0 {
			return fmt.Errorf("%w: the %q definition section is not composed, so the release would drop its guardrails", domain.ErrNotReady, GuardrailSectionName)
		}
	}
	if err = carriesTypeRules(typeRules, rules); err != nil {
		return err
	}
	return checkInherited(inherited, rules)
}

// checkRestore refuses to restore rules that drop or loosen a rule the type declares now. The
// platform and tenant layers are enforced at runtime whatever the release carries, but the
// type's rules exist only in the release's own rule set.
func (g *StudioGuardrails) checkRestore(ctx context.Context, definition domain.AgentDefinition, rules []domain.GuardrailRule) error {
	typeRules, err := g.typeRules(ctx, definition.AgentTypeID)
	if err != nil {
		return err
	}
	return carriesTypeRules(typeRules, rules)
}

// carriesTypeRules refuses a rule set that drops a type rule or reuses its id without tightening it.
func carriesTypeRules(typeRules, rules []domain.GuardrailRule) error {
	for _, parent := range typeRules {
		index := slices.IndexFunc(rules, func(candidate domain.GuardrailRule) bool { return candidate.ID == parent.ID })
		if index < 0 {
			return fmt.Errorf("%w: the release drops the type's rule %q", domain.ErrAuthorityExceeded, parent.ID)
		}
		if err := guardrail.CheckTightening(parent, rules[index]); err != nil {
			return fmt.Errorf("guardrail layer %s: %w", domain.GuardrailLayerType, err)
		}
	}
	return nil
}

// check compiles the agent's own rules and the set a release would freeze with the type's, and
// refuses an own rule that reuses an inherited rule's id without tightening it.
func (g *StudioGuardrails) check(ctx context.Context, inherited []domain.InheritedGuardrails, own []domain.GuardrailRule) error {
	if err := g.validate(ctx, own); err != nil {
		return err
	}
	if err := checkInherited(inherited, own); err != nil {
		return err
	}
	if err := g.validate(ctx, releaseRules(layerRules(inherited, domain.GuardrailLayerType), own)); err != nil {
		return fmt.Errorf("with the type's rules: %w", err)
	}
	return nil
}

func (g *StudioGuardrails) validate(ctx context.Context, rules []domain.GuardrailRule) error {
	envelope, digest, _, err := canonicalRules(rules)
	if err != nil {
		return err
	}
	_, err = g.Rules.Validate(ctx, domain.GuardrailConfig{Version: digest, RulesDigest: digest, Rules: envelope})
	return err
}

// inherited returns the non-empty layers an agent of the type inherits, widest first.
func (g *StudioGuardrails) inherited(ctx context.Context, tenantID int64, agentTypeID string) ([]domain.InheritedGuardrails, error) {
	layers, err := g.Restrictions.Layers(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read restriction layers: %w", err)
	}
	typeRules, err := g.typeRules(ctx, agentTypeID)
	if err != nil {
		return nil, err
	}
	var inherited []domain.InheritedGuardrails
	for _, layer := range []domain.InheritedGuardrails{
		{Layer: domain.GuardrailLayerBaseline, Rules: g.Baseline},
		{Layer: domain.GuardrailLayerPlatform, Digest: layers.Platform.Digest, Rules: layers.Platform.Guardrails},
		{Layer: domain.GuardrailLayerTenant, Digest: layers.Tenant.Digest, Rules: layers.Tenant.Guardrails},
		{Layer: domain.GuardrailLayerType, Rules: typeRules},
	} {
		if len(layer.Rules) > 0 {
			inherited = append(inherited, layer)
		}
	}
	return inherited, nil
}

// typeRules are the product type's rules; a type the catalog does not know declares none.
func (g *StudioGuardrails) typeRules(ctx context.Context, agentTypeID string) ([]domain.GuardrailRule, error) {
	if g.Studio.Kinds == nil {
		return nil, nil
	}
	descriptor, err := g.Studio.Kinds.Get(ctx, agentTypeID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load agent type %q: %w", agentTypeID, err)
	}
	return descriptor.Guardrails, nil
}

// ownRules reads the agent's own rules and refuses a stored set that no longer matches its digest.
func ownRules(ctx context.Context, qs keelport.QueryService, tenantID int64, agentID string) ([]domain.GuardrailRule, error) {
	result, err := qs.Query(ctx, qGuardrailDraftGet, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("load agent guardrails: %w", err)
	}
	if len(result.Rows) == 0 {
		return nil, nil
	}
	envelope := []byte(common.AsString(result.Rows[0][0]))
	if guardrail.Digest(envelope) != strings.TrimSpace(common.AsString(result.Rows[0][1])) {
		return nil, fmt.Errorf("%w: the guardrail rules of %q do not match their digest", domain.ErrConflict, agentID)
	}
	var set domain.GuardrailRuleSet
	if err = json.Unmarshal(envelope, &set); err != nil {
		return nil, fmt.Errorf("%w: the guardrail rules of %q do not decode: %v", domain.ErrConflict, agentID, err)
	}
	return set.Rules, nil
}

// releaseGuardrails reads the rule set a definition froze, verified against its digest, or the
// empty set when it froze none.
func releaseGuardrails(definition domain.AgentDefinition) (domain.GuardrailConfig, []domain.GuardrailRule, bool, error) {
	raw, frozen := definition.Sections[GuardrailSectionName]
	if !frozen {
		envelope, digest, rules, err := canonicalRules(nil)
		return domain.GuardrailConfig{Version: digest, RulesDigest: digest, Rules: envelope}, rules, false, err
	}
	var section guardrailSection
	if err := json.Unmarshal(raw, &section); err != nil {
		return domain.GuardrailConfig{}, nil, true, fmt.Errorf("%w: the frozen guardrails do not decode: %v", domain.ErrConflict, err)
	}
	if guardrail.Digest(section.Rules) != section.RulesDigest {
		return domain.GuardrailConfig{}, nil, true, fmt.Errorf("%w: the frozen guardrails do not match their digest", domain.ErrConflict)
	}
	var set domain.GuardrailRuleSet
	if err := json.Unmarshal(section.Rules, &set); err != nil {
		return domain.GuardrailConfig{}, nil, true, fmt.Errorf("%w: the frozen guardrail rules do not decode: %v", domain.ErrConflict, err)
	}
	return domain.GuardrailConfig{Version: section.RulesDigest, RulesDigest: section.RulesDigest, Rules: section.Rules}, set.Rules, true, nil
}

// FrozenGuardrailConfig reads and verifies the guardrails frozen into a definition.
func FrozenGuardrailConfig(definition domain.AgentDefinition) (domain.GuardrailConfig, error) {
	config, _, _, err := releaseGuardrails(definition)
	return config, err
}

// releaseRules is what a release enforces in its own layer: the type's rules, each replaced in
// place by the agent's rule of the same id, then the agent's other rules in their order.
func releaseRules(typeRules, own []domain.GuardrailRule) []domain.GuardrailRule {
	overrides := make(map[string]domain.GuardrailRule, len(own))
	for _, rule := range own {
		overrides[rule.ID] = rule
	}
	rules := make([]domain.GuardrailRule, 0, len(typeRules)+len(own))
	replaced := map[string]bool{}
	for _, rule := range typeRules {
		if override, ok := overrides[rule.ID]; ok && !replaced[rule.ID] {
			replaced[rule.ID] = true
			rule = override
		}
		rules = append(rules, rule)
	}
	for _, rule := range own {
		if !replaced[rule.ID] {
			rules = append(rules, rule)
		}
	}
	return rules
}

// checkInherited refuses a rule that reuses an inherited rule's id without tightening it.
func checkInherited(inherited []domain.InheritedGuardrails, rules []domain.GuardrailRule) error {
	byID := make(map[string]domain.GuardrailRule, len(rules))
	for _, rule := range rules {
		byID[rule.ID] = rule
	}
	for _, layer := range inherited {
		for _, parent := range layer.Rules {
			if rule, reused := byID[parent.ID]; reused {
				if err := guardrail.CheckTightening(parent, rule); err != nil {
					return fmt.Errorf("guardrail layer %s: %w", layer.Layer, err)
				}
			}
		}
	}
	return nil
}

func layerRules(inherited []domain.InheritedGuardrails, layer domain.GuardrailLayer) []domain.GuardrailRule {
	for _, item := range inherited {
		if item.Layer == layer {
			return item.Rules
		}
	}
	return nil
}

// canonicalRules encodes rules as the envelope guardrail_config stores, with sorted keys and no
// insignificant space so equal rule sets share bytes and digest, and decodes them back so what
// is checked is exactly what is stored.
func canonicalRules(rules []domain.GuardrailRule) ([]byte, string, []domain.GuardrailRule, error) {
	if rules == nil {
		rules = []domain.GuardrailRule{}
	}
	raw, err := json.Marshal(domain.GuardrailRuleSet{SchemaVersion: guardrail.RuleSetSchemaVersion, Rules: rules})
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: guardrail rules are not JSON: %v", domain.ErrValidation, err)
	}
	envelope, err := jsonschema.Canonical(raw)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: guardrail rules are not JSON: %v", domain.ErrValidation, err)
	}
	var set domain.GuardrailRuleSet
	if err = json.Unmarshal(envelope, &set); err != nil {
		return nil, "", nil, fmt.Errorf("%w: guardrail rules do not decode: %v", domain.ErrValidation, err)
	}
	return envelope, guardrail.Digest(envelope), set.Rules, nil
}

func validateGuardrailChange(change domain.AgentGuardrailChange) error {
	var fields []domain.AgentFieldError
	if change.ExpectedDraftRevision <= 0 {
		fields = append(fields, domain.AgentFieldError{Field: "expected_agent_revision", Message: "must be positive"})
	}
	for i, rule := range change.Rules {
		if id := strings.TrimSpace(rule.ID); id == "" || id != rule.ID || len(id) > maxGuardrailRuleID {
			fields = append(fields, domain.AgentFieldError{Field: fmt.Sprintf("rules[%d].id", i), Message: fmt.Sprintf("must contain 1 to %d characters without surrounding space", maxGuardrailRuleID)})
		}
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

// guardrailProblem reports rules that do not compile or loosen what they inherit.
func guardrailProblem(err error) bool {
	return errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrAuthorityExceeded)
}
