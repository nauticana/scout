package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// StudioPortability exports an agent as a Charter AgentDefinition document and imports one as a new
// agent or as the draft of an existing one. An import yields only a disabled, unpublished draft:
// publication recompiles and guards it as any other.
type StudioPortability struct {
	// Studio authorizes every call and owns the drafts; required.
	Studio *StudioService
	// Documents renders and reads the document; required.
	Documents contract.AgentDocumentCodec
	// Guardrails checks imported guardrail rules; nil refuses a document that carries any.
	Guardrails *StudioGuardrails
}

var _ contract.StudioPortabilityBackend = (*StudioPortability)(nil)

func (p *StudioPortability) ready() error {
	if p.Studio == nil || p.Documents == nil {
		return fmt.Errorf("%w: studio portability needs a studio service and a document codec", domain.ErrNotReady)
	}
	return nil
}

// Export renders the agent's active release, its agent-scope bindings, and its draft parts. It never
// reads credentials, secrets, or another scope's bindings, and strips who published and bound what.
func (p *StudioPortability) Export(ctx context.Context, actor domain.StudioActor, agentID string) (json.RawMessage, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	studio := p.Studio
	if err := studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	draft, err := studio.getDraft(ctx, actor.TenantID, agentID)
	if err != nil {
		return nil, err
	}
	active, err := studio.qs.Query(ctx, qStudioActiveDefinition, actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("load active definition: %w", err)
	}
	if len(active.Rows) == 0 {
		return nil, fmt.Errorf("%w: agent %q has no active release; publish it before exporting", domain.ErrConflict, agentID)
	}
	release, err := decodeDefinition(active.Rows[0][1])
	if err != nil {
		return nil, err
	}
	frozen, err := studio.qs.Query(ctx, qStudioGetSettings, actor.TenantID, agentID, release.Version)
	if err != nil {
		return nil, fmt.Errorf("load frozen settings: %w", err)
	}
	if len(frozen.Rows) == 0 {
		return nil, fmt.Errorf("%w: version %s has no frozen settings; publish the agent before exporting", domain.ErrConflict, release.Version)
	}
	effective, err := decodeSettings(frozen.Rows[0], actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("decode frozen settings: %w", err)
	}
	scopes, err := studio.promptScopes(ctx, actor.TenantID, agentID, draft.AgentTypeID)
	if err != nil {
		return nil, err
	}
	bindings, err := studio.portableBindings(ctx, actor.TenantID, scopes.AgentScopeID)
	if err != nil {
		return nil, err
	}
	typed, err := studio.qs.Query(ctx, qSeedAgentType, actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("load agent type version: %w", err)
	}
	agent := domain.PortableAgent{
		AgentTypeID: draft.AgentTypeID, DisplayName: draft.DisplayName, Models: draft.Models, Bindings: bindings,
		Release: sanitizedRelease(release),
	}
	if agent.Extension, err = compactExtension(draft.Extension, studio.extensionLimit()); err != nil {
		return nil, fmt.Errorf("%w: the draft extension %v", domain.ErrConflict, err)
	}
	if len(typed.Rows) > 0 {
		agent.TypeVersion, agent.TypeDigest = common.AsString(typed.Rows[0][0]), strings.TrimSpace(common.AsString(typed.Rows[0][1]))
	}
	if agent.Guardrails, err = ownRules(ctx, studio.qs, actor.TenantID, agentID); err != nil {
		return nil, err
	}
	origin, err := studio.draftOrigin(ctx, actor.TenantID, agentID)
	if err != nil {
		return nil, err
	}
	agent.DraftOrigin = origin.derivedFrom()
	for _, resource := range effective.Resources {
		agent.Settings = append(agent.Settings, domain.PortableSetting{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID, Value: resource.Value})
	}
	document, digest, err := p.Documents.Encode(ctx, actor.TenantID, agent)
	if err != nil {
		return nil, err
	}
	detail := clipDetail(fmt.Sprintf("version %s sha256:%s", release.Version, shortDigest(digest)))
	if _, err = studio.qs.Query(ctx, qStudioAudit, actor.TenantID, agentID, "EXPORT", detail, string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return nil, fmt.Errorf("record studio export audit: %w", err)
	}
	return document, nil
}

// sanitizedRelease removes who published the release and the scope provenance of its prompts; the
// definition digest covers neither, so it still verifies.
func sanitizedRelease(release domain.AgentDefinition) domain.AgentDefinition {
	release.PublishedBy, release.DraftRevision, release.PromptProfileRevision = nil, 0, 0
	languages := slices.Clone(release.Languages)
	for index := range languages {
		sections := slices.Clone(languages[index].Sections)
		for section := range sections {
			sections[section].Source = domain.Provenance{}
		}
		languages[index].Sections = sections
	}
	release.Languages = languages
	return release
}

// Import creates an agent from a document, or with AgentID replaces that agent's draft settings,
// prompts, and own guardrail rules. Every knowledge and entitlement value is mapped through
// References; anything that would widen authority is refused, never dropped.
func (p *StudioPortability) Import(ctx context.Context, actor domain.StudioActor, request domain.AgentImportRequest) (domain.AgentSummary, error) {
	if err := p.ready(); err != nil {
		return domain.AgentSummary{}, err
	}
	if err := p.Studio.init(ctx); err != nil {
		return domain.AgentSummary{}, err
	}
	if err := validateImport(request, p.Studio.referenceLimit()); err != nil {
		return domain.AgentSummary{}, err
	}
	if request.AgentID != "" {
		return p.replace(ctx, actor, request)
	}
	agent, err := p.Documents.Decode(ctx, request.Document)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	studio := p.Studio
	if err = studio.authorizeValue(ctx, actor, domain.StudioCreate, agentTypeScopeValue(agent.AgentTypeID)); err != nil {
		return domain.AgentSummary{}, err
	}
	create := domain.AgentCreateRequest{AgentTypeID: agent.AgentTypeID, DisplayName: request.DisplayName, IdempotencyKey: request.IdempotencyKey}
	if create.DisplayName == "" {
		create.DisplayName = agent.DisplayName
	}
	if err = validateCreate(create, studio.referenceLimit()); err != nil {
		return domain.AgentSummary{}, err
	}
	if err = studio.offered(ctx, agent.AgentTypeID); err != nil {
		return domain.AgentSummary{}, err
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{"import", agent.Digest, create.DisplayName, canonicalReferences(request.References)}, "\x1f")))
	return studio.create(ctx, actor, create, hex.EncodeToString(sum[:]), func() (*agentSeed, error) {
		candidate := domain.AgentDraft{AgentTypeID: agent.AgentTypeID, DisplayName: create.DisplayName, ExpectedDraftRevision: 1}
		return p.importSeed(ctx, actor, agent, request.References, candidate, "")
	})
}

func (p *StudioPortability) replace(ctx context.Context, actor domain.StudioActor, request domain.AgentImportRequest) (domain.AgentSummary, error) {
	studio := p.Studio
	current, err := studio.editable(ctx, actor, domain.StudioEdit, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	agent, err := p.Documents.Decode(ctx, request.Document)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	if agent.AgentTypeID != current.AgentTypeID {
		return domain.AgentSummary{}, validationError("document.agent_type", "must be the agent's type "+current.AgentTypeID)
	}
	pinned, err := studio.qs.Query(ctx, qSeedAgentType, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("load agent type version: %w", err)
	}
	pinnedVersion := ""
	if len(pinned.Rows) > 0 {
		pinnedVersion = common.AsString(pinned.Rows[0][0])
	}
	if pinnedVersion != "" && agent.TypeVersion != "" && pinnedVersion != agent.TypeVersion {
		return domain.AgentSummary{}, validationError("document.type_version", "the agent pins type version "+pinnedVersion)
	}
	full, err := studio.draft(ctx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	candidate := full
	candidate.ExpectedDraftRevision = request.ExpectedDraftRevision
	seed, err := p.importSeed(ctx, actor, agent, request.References, candidate, full.PromptScopes.AgentScopeID)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	scopes := full.PromptScopes

	tx, err := studio.DB.BeginTx(ctx, studioCreateQueries)
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("begin import transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	locked, err := tx.Query(ctx, qStudioLockDraft, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("lock import draft: %w", err)
	}
	if len(locked.Rows) == 0 {
		return domain.AgentSummary{}, domain.ErrNotFound
	}
	if common.AsInt64(locked.Rows[0][0]) != request.ExpectedDraftRevision {
		return domain.AgentSummary{}, domain.ErrRevisionConflict
	}
	if _, err = tx.Query(ctx, qStudioLockAlias, actor.TenantID, current.AgentTypeID); err != nil {
		return domain.AgentSummary{}, fmt.Errorf("lock import type: %w", err)
	}
	product, err := tx.Query(ctx, qSeedNonPlatformAtScope, actor.TenantID, scopes.AgentScopeID, platformKinds())
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("count product bindings: %w", err)
	}
	if len(product.Rows) > 0 && common.AsInt64(product.Rows[0][0]) > 0 {
		return domain.AgentSummary{}, fmt.Errorf("%w: agent %q binds product kinds an import would drop", domain.ErrConflict, request.AgentID)
	}
	if err = studio.checkSeed(ctx, actor, request.AgentID, scopes, seed); err != nil {
		return domain.AgentSummary{}, err
	}
	placed := slices.Clone(seed.bindings)
	for index := range placed {
		placed[index].ScopeID = scopes.AgentScopeID
	}
	loosening, err := studio.checkGateChange(ctx, actor, request.AgentID, scopes.AgentScopeID,
		seededScopes{ScopeRepository: studio.scopeRepository(), scopeID: scopes.AgentScopeID, bindings: placed})
	if err != nil {
		return domain.AgentSummary{}, err
	}
	updated, err := tx.Query(ctx, qStudioUpdateDraft, current.Enabled,
		modelProvider(seed.models.Text), modelID(seed.models.Text), modelProvider(seed.models.Image), modelID(seed.models.Image),
		modelProvider(seed.models.Video), modelID(seed.models.Video), nullableJSON(seed.extension), actorUser(actor),
		actor.TenantID, request.AgentID, request.ExpectedDraftRevision)
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("update import draft: %w", err)
	}
	if len(updated.Rows) == 0 {
		return domain.AgentSummary{}, domain.ErrRevisionConflict
	}
	for _, query := range []string{qSeedDropScope, qSeedEndScope} {
		if _, err = tx.Query(ctx, query, actor.TenantID, scopes.AgentScopeID); err != nil {
			return domain.AgentSummary{}, fmt.Errorf("end agent bindings: %w", err)
		}
	}
	if err = studio.plantSeed(ctx, tx, actor, request.AgentID, scopes.AgentScopeID, seed); err != nil {
		return domain.AgentSummary{}, err
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, seed.event, clipDetail(seed.detail)); err != nil {
		return domain.AgentSummary{}, err
	}
	if err = studio.recordGateLoosening(ctx, tx, actor, loosening); err != nil {
		return domain.AgentSummary{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentSummary{}, fmt.Errorf("commit import: %w", err)
	}
	committed = true
	return studio.summary(ctx, actor.TenantID, request.AgentID)
}

// importSeed checks a decoded document against the importing tenant and returns what the agent
// takes. candidate is the draft the agent will have besides the seed: a new agent's name and type,
// or the replaced agent's draft, whose layers at agentScopeID the seed's prompts replace.
func (p *StudioPortability) importSeed(ctx context.Context, actor domain.StudioActor, agent domain.PortableAgent,
	references map[domain.ResourceKind]map[string]string, candidate domain.AgentDraft, agentScopeID string) (*agentSeed, error) {
	studio := p.Studio
	release := agent.Release
	if release.AgentTypeID != agent.AgentTypeID {
		return nil, validationError("document.agent_type", "must be the type of the embedded release")
	}
	// The codec is a port, so the origin columns' bounds are checked here as well.
	switch {
	case agent.Document.ID == "" || utf8.RuneCountInString(agent.Document.ID) > 120:
		return nil, validationError("document.id", "must contain 1 to 120 characters")
	case utf8.RuneCountInString(agent.Document.Namespace) > 200:
		return nil, validationError("document.namespace", "must not exceed 200 characters")
	case release.Version == "" || utf8.RuneCountInString(release.Version) > 60:
		return nil, validationError("document.extensions.scout:definition.version", "must contain 1 to 60 characters")
	}
	if err := studio.checkTypeVersion(ctx, actor.TenantID, agent.AgentTypeID, agent.TypeVersion, agent.TypeDigest, "document.type_version"); err != nil {
		return nil, err
	}
	if err := p.checkRelease(ctx, actor.TenantID, agent); err != nil {
		return nil, err
	}
	extension, err := compactExtension(agent.Extension, studio.extensionLimit())
	if err != nil {
		return nil, validationError("document.extension", err.Error())
	}
	models, err := studio.resolveModels(ctx, actor.TenantID, agent.Models)
	if err != nil {
		return nil, err
	}
	adopted, err := studio.adopt(ctx, actor, agent.AgentTypeID, "document.bindings", agent.Bindings, references, adoptImport, nil)
	if err != nil {
		return nil, err
	}
	seed := &agentSeed{
		models: models, extension: extension, bindings: adopted.scoped, typeVersion: agent.TypeVersion, event: "IMPORT",
		origin: &draftOrigin{derived: domain.DerivedFrom{Source: agent.Document, Version: release.Version, Digest: agent.Digest}},
		detail: fmt.Sprintf("%s/%s version %s sha256:%s", agent.Document.Namespace, agent.Document.ID, release.Version, shortDigest(agent.Digest)),
	}
	if err = p.checkGuardrails(ctx, actor.TenantID, agent, seed); err != nil {
		return nil, err
	}
	candidate.Models, candidate.Extension = models, extension
	candidate.Languages = mergeLanguages(candidate.Languages, agentScopeID, adopted.languages)
	seed.draft = candidate
	return seed, nil
}

// checkRelease verifies the embedded release against its own digests and the importing tenant's
// type: the definition and settings digests recompute, and its tools and skills are offered.
func (p *StudioPortability) checkRelease(ctx context.Context, tenantID int64, agent domain.PortableAgent) error {
	studio := p.Studio
	release := agent.Release
	if len(release.Sections) > 0 {
		sections := make(map[string]json.RawMessage, len(release.Sections))
		for name, raw := range release.Sections {
			canonical, err := canonicalSection(raw)
			if err != nil {
				return validationError("document.extensions.scout:definition.sections."+name, err.Error())
			}
			sections[name] = canonical
		}
		release.Sections = sections
	}
	digest, err := studio.Compiler.DefinitionDigest(release)
	if err != nil || digest != release.DefinitionDigest {
		return validationError("document.extensions.scout:definition.definition_digest", "does not match the embedded release")
	}
	resources := make([]domain.EffectiveResource, 0, len(agent.Settings))
	for _, setting := range agent.Settings {
		resources = append(resources, domain.EffectiveResource{ResourceKind: setting.ResourceKind, ResourceID: setting.ResourceID, Value: setting.Value})
	}
	if scope.SettingsDigest(resources) != release.SettingsDigest {
		return validationError("document.extensions.scout:agent.settings", "do not match the embedded release's settings digest")
	}
	if len(release.Tools) == 0 && len(release.Skills) == 0 {
		return nil
	}
	descriptor, err := studio.descriptor(ctx, agent.AgentTypeID)
	if err != nil {
		return err
	}
	var fields []domain.AgentFieldError
	for _, tool := range release.Tools {
		if !slices.ContainsFunc(descriptor.Tools, func(offered domain.ToolReference) bool { return offered.ToolID == tool.ToolID }) {
			fields = append(fields, domain.AgentFieldError{Field: "document.extensions.scout:definition.tools", Message: "the agent type does not offer tool " + tool.ToolID})
		}
	}
	for _, skill := range release.Skills {
		if !slices.ContainsFunc(descriptor.Skills, func(offered domain.SkillReference) bool { return offered.SkillID == skill.SkillID }) {
			fields = append(fields, domain.AgentFieldError{Field: "document.extensions.scout:definition.skills", Message: "the agent type does not offer skill " + skill.SkillID})
		}
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

// checkGuardrails admits the agent's own rules only where guardrails are composed, and only rules
// that add to or tighten what the agent inherits in the importing tenant.
func (p *StudioPortability) checkGuardrails(ctx context.Context, tenantID int64, agent domain.PortableAgent, seed *agentSeed) error {
	if p.Guardrails == nil {
		if len(agent.Guardrails) > 0 {
			return validationError("document.guardrails", "this product does not compose guardrails")
		}
		return nil
	}
	if err := p.Guardrails.ready(); err != nil {
		return err
	}
	_, _, rules, err := canonicalRules(agent.Guardrails)
	if err != nil {
		return err
	}
	for index, rule := range rules {
		if id := strings.TrimSpace(rule.ID); id == "" || id != rule.ID || len(id) > maxGuardrailRuleID {
			return validationError(fmt.Sprintf("document.guardrails[%d].id", index), fmt.Sprintf("must contain 1 to %d characters", maxGuardrailRuleID))
		}
	}
	inherited, err := p.Guardrails.inherited(ctx, tenantID, agent.AgentTypeID)
	if err != nil {
		return err
	}
	if err = p.Guardrails.check(ctx, inherited, rules); err != nil {
		return err
	}
	seed.guardrails = &rules
	return nil
}

// mergeLanguages replaces the layers a draft holds at the agent's scope by the seed's.
func mergeLanguages(current []domain.AgentLanguageDraft, agentScopeID string, seeded []domain.AgentLanguageDraft) []domain.AgentLanguageDraft {
	merged := make([]domain.AgentLanguageDraft, 0, len(current)+len(seeded))
	index := map[string]int{}
	for _, language := range current {
		kept := domain.AgentLanguageDraft{LanguageCode: language.LanguageCode}
		for _, section := range language.Sections {
			section.Layers = slices.DeleteFunc(slices.Clone(section.Layers), func(layer domain.PromptLayer) bool {
				return agentScopeID != "" && layer.ScopeID == agentScopeID
			})
			kept.Sections = append(kept.Sections, section)
		}
		index[language.LanguageCode] = len(merged)
		merged = append(merged, kept)
	}
	for _, language := range seeded {
		at, exists := index[language.LanguageCode]
		if !exists {
			index[language.LanguageCode] = len(merged)
			merged = append(merged, language)
			continue
		}
		for _, section := range language.Sections {
			position := slices.IndexFunc(merged[at].Sections, func(candidate domain.AgentPromptSection) bool {
				return candidate.PromptSectionID == section.PromptSectionID
			})
			if position < 0 {
				merged[at].Sections = append(merged[at].Sections, section)
				continue
			}
			merged[at].Sections[position].Layers = append(merged[at].Sections[position].Layers, section.Layers...)
		}
	}
	return merged
}

func validateImport(request domain.AgentImportRequest, referenceLimit int) error {
	var fields []domain.AgentFieldError
	if len(bytes.TrimSpace(request.Document)) == 0 {
		fields = append(fields, domain.AgentFieldError{Field: "document", Message: "required"})
	}
	if request.AgentID == "" {
		if key := strings.TrimSpace(request.IdempotencyKey); key == "" || len(key) > 120 {
			fields = append(fields, domain.AgentFieldError{Field: "idempotency_key", Message: "must contain 1 to 120 characters"})
		}
		if request.ExpectedDraftRevision != 0 {
			fields = append(fields, domain.AgentFieldError{Field: "expected_agent_revision", Message: "applies only to a replace"})
		}
	} else {
		if request.ExpectedDraftRevision <= 0 {
			fields = append(fields, domain.AgentFieldError{Field: "expected_agent_revision", Message: "must be positive"})
		}
		if request.DisplayName != "" || request.IdempotencyKey != "" {
			fields = append(fields, domain.AgentFieldError{Field: "display_name", Message: "display_name and idempotency_key apply only to a create"})
		}
	}
	fields = append(fields, validateReferences("references", request.References, referenceLimit)...)
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

func platformKinds() string {
	kinds := make([]string, 0, len(scope.PlatformNarrowingRules))
	for kind := range scope.PlatformNarrowingRules {
		kinds = append(kinds, string(kind))
	}
	slices.Sort(kinds)
	return strings.Join(kinds, "\x1f")
}

func shortDigest(digest string) string {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if len(hexDigest) > 12 {
		return hexDigest[:12]
	}
	return hexDigest
}
