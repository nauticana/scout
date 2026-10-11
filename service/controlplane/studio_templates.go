package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// tenantTemplateID is the id of a tenant's template; a platform template's id never starts with "tpl-".
var tenantTemplateID = regexp.MustCompile(`^tpl-[0-9a-f]{20}$`)

// platformTemplateID is a Charter identifier, since a derived agent names the template in derivedFrom.
var platformTemplateID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,79}$`)

// templateAudienceValue is the AGENT_STUDIO TEMPLATE_PUBLISH grant value of an audience.
func templateAudienceValue(all bool) string {
	if all {
		return "template_audience:all"
	}
	return "template_audience:tenants"
}

// StudioTemplates saves agents as templates, shares them, and reports what an agent derives from.
// The store is Studio.Templates; Studio.Create makes agents from templates.
type StudioTemplates struct {
	Studio *StudioService
}

var _ contract.StudioTemplatesBackend = (*StudioTemplates)(nil)

func (t *StudioTemplates) ready(ctx context.Context) error {
	if t.Studio == nil || t.Studio.Templates == nil {
		return fmt.Errorf("%w: studio templates need a studio service with a template store", domain.ErrNotReady)
	}
	return t.Studio.init(ctx)
}

// ListTemplates returns the templates of a type the caller's tenant may use; the caller needs CREATE
// or TEMPLATE on the type.
func (t *StudioTemplates) ListTemplates(ctx context.Context, actor domain.StudioActor, agentTypeID string) ([]domain.AgentTemplate, error) {
	if strings.TrimSpace(agentTypeID) == "" {
		return nil, validationError("agent_type", "required")
	}
	if t.Studio == nil {
		return nil, fmt.Errorf("%w: studio templates need a studio service", domain.ErrNotReady)
	}
	value := agentTypeScopeValue(agentTypeID)
	if err := t.Studio.authorizeValue(ctx, actor, domain.StudioCreate, value); errors.Is(err, domain.ErrForbidden) {
		if err = t.Studio.authorizeValue(ctx, actor, domain.StudioTemplate, value); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := t.ready(ctx); err != nil {
		return nil, err
	}
	return t.Studio.Templates.Usable(ctx, actor.TenantID, agentTypeID)
}

// SaveTemplate stores the agent's own bindings as a new template or a new version of one the tenant
// owns. Knowledge and entitlement values become the slot names the request maps them to, so the
// template names no resource of the owner; the agent's guardrails, models, and extension stay behind.
func (t *StudioTemplates) SaveTemplate(ctx context.Context, actor domain.StudioActor, request domain.AgentTemplateSaveRequest) (domain.AgentTemplate, error) {
	if err := validateTemplateSave(request); err != nil {
		return domain.AgentTemplate{}, err
	}
	if t.Studio == nil {
		return domain.AgentTemplate{}, fmt.Errorf("%w: studio templates need a studio service", domain.ErrNotReady)
	}
	studio := t.Studio
	draft, err := studio.editable(ctx, actor, domain.StudioEdit, request.AgentID)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	if err = studio.authorizeValue(ctx, actor, domain.StudioTemplate, agentTypeScopeValue(draft.AgentTypeID)); err != nil {
		return domain.AgentTemplate{}, err
	}
	if err = t.ready(ctx); err != nil {
		return domain.AgentTemplate{}, err
	}
	if request.TemplateID != "" {
		owned, err := studio.Templates.Own(ctx, actor.TenantID, request.TemplateID)
		if err != nil {
			return domain.AgentTemplate{}, err
		}
		if owned.Latest.AgentTypeID != draft.AgentTypeID {
			return domain.AgentTemplate{}, domain.ErrNotFound
		}
	}
	scopes, err := studio.promptScopes(ctx, actor.TenantID, request.AgentID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	bindings, err := studio.portableBindings(ctx, actor.TenantID, scopes.AgentScopeID)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	adopted, err := studio.adopt(ctx, actor, draft.AgentTypeID, "bindings", bindings, request.Slots, adoptTemplateSave, nil)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	typed, err := studio.qs.Query(ctx, qSeedAgentType, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("load agent type version: %w", err)
	}
	version := domain.AgentTemplateVersion{
		TemplateID: request.TemplateID, Owner: domain.TemplateOwn, OwnerTenantID: actor.TenantID, AgentTypeID: draft.AgentTypeID,
		DisplayName: request.DisplayName, Description: request.Description, ChangeSummary: request.ChangeSummary,
		Payload: domain.TemplatePayload{Bindings: nonNilBindings(adopted.portable)}, SourceAgentID: request.AgentID,
		SourceDraftRevision: draft.ExpectedDraftRevision,
	}
	if len(adopted.slots) > 0 {
		version.Payload.Slots = adopted.slots
	}
	if len(typed.Rows) > 0 && common.AsString(typed.Rows[0][0]) != "" {
		version.TypeVersion, version.TypeDigest = common.AsString(typed.Rows[0][0]), strings.TrimSpace(common.AsString(typed.Rows[0][1]))
	}
	if version.Digest, err = templatePayloadDigest(version.Payload); err != nil {
		return domain.AgentTemplate{}, err
	}
	return studio.Templates.Save(ctx, actor, version, request.IdempotencyKey, studio.TemplateLimits)
}

// PublishTemplate shares an owned template with listed tenants or every tenant, or withdraws that.
// The caller needs TEMPLATE on the type and TEMPLATE_PUBLISH at the audience; no role holds the latter
// unless a product grants it. Publication copies nothing beyond the template itself.
func (t *StudioTemplates) PublishTemplate(ctx context.Context, actor domain.StudioActor, publication domain.AgentTemplatePublication) (domain.AgentTemplate, error) {
	if err := t.ready(ctx); err != nil {
		return domain.AgentTemplate{}, err
	}
	studio := t.Studio
	if err := validatePublication(actor, publication, studio.audienceLimit()); err != nil {
		return domain.AgentTemplate{}, err
	}
	if err := validateActor(actor); err != nil {
		return domain.AgentTemplate{}, err
	}
	owned, err := studio.Templates.Own(ctx, actor.TenantID, publication.TemplateID)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	if err = studio.authorizeValue(ctx, actor, domain.StudioTemplate, agentTypeScopeValue(owned.Latest.AgentTypeID)); err != nil {
		return domain.AgentTemplate{}, err
	}
	if err = studio.authorizeValue(ctx, actor, domain.StudioTemplatePublish, templateAudienceValue(publication.Audience.All)); err != nil {
		return domain.AgentTemplate{}, err
	}
	return studio.Templates.Publish(ctx, actor, publication)
}

// TemplateStatus reports the agent's origin and, for a template, the newest version the tenant may use.
func (t *StudioTemplates) TemplateStatus(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentTemplateStatus, error) {
	if t.Studio == nil {
		return domain.AgentTemplateStatus{}, fmt.Errorf("%w: studio templates need a studio service", domain.ErrNotReady)
	}
	studio := t.Studio
	if err := studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentTemplateStatus{}, err
	}
	if _, err := studio.getDraft(ctx, actor.TenantID, agentID); err != nil {
		return domain.AgentTemplateStatus{}, err
	}
	origin, err := studio.draftOrigin(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentTemplateStatus{}, err
	}
	status := domain.AgentTemplateStatus{AgentID: agentID, DerivedFrom: origin.derivedFrom()}
	if origin == nil || origin.templateID == "" {
		return status, nil
	}
	status.TemplateID, status.Version = origin.templateID, origin.templateVersion
	if studio.Templates == nil {
		return status, nil
	}
	latest, err := studio.Templates.Version(ctx, actor.TenantID, origin.templateID, 0)
	if errors.Is(err, domain.ErrNotFound) {
		return status, nil
	}
	if err != nil {
		return domain.AgentTemplateStatus{}, err
	}
	status.Available, status.LatestVersion, status.Newer = true, latest.Version, latest.Version > origin.templateVersion
	return status, nil
}

// RegisterPlatform stores an immutable platform template version for a product to offer every tenant.
// Its bindings are checked against the type; models are checked when a tenant creates from it.
func (t *StudioTemplates) RegisterPlatform(ctx context.Context, version domain.AgentTemplateVersion) error {
	if err := t.ready(ctx); err != nil {
		return err
	}
	var fields []domain.AgentFieldError
	if !platformTemplateID.MatchString(version.TemplateID) || strings.HasPrefix(version.TemplateID, "tpl-") {
		fields = append(fields, domain.AgentFieldError{Field: "template_id", Message: "must be a Charter identifier of at most 80 characters not starting with tpl-"})
	}
	if version.Version <= 0 {
		fields = append(fields, domain.AgentFieldError{Field: "version", Message: "must be positive"})
	}
	if strings.TrimSpace(version.AgentTypeID) == "" {
		fields = append(fields, domain.AgentFieldError{Field: "agent_type", Message: "required"})
	}
	if (version.TypeVersion == "") != (version.TypeDigest == "") || (version.TypeDigest != "" && len(version.TypeDigest) != 64) {
		fields = append(fields, domain.AgentFieldError{Field: "type_version", Message: "a type version names its 64-character digest"})
	}
	fields = append(fields, templateLabels(version.DisplayName, version.Description, version.ChangeSummary)...)
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	payload, err := t.checkPlatformPayload(ctx, version.AgentTypeID, version.Payload)
	if err != nil {
		return err
	}
	version.Payload, version.Owner, version.OwnerTenantID = payload, domain.TemplatePlatform, 0
	version.SourceAgentID, version.SourceDraftRevision = "", 0
	if version.Digest, err = templatePayloadDigest(payload); err != nil {
		return err
	}
	return t.Studio.Templates.RegisterPlatform(ctx, version)
}

// checkPlatformPayload validates a platform payload: platform kinds and canonical values, tools and
// skills of the type, captions that exist, and knowledge and entitlement values that are exactly the declared slots.
func (t *StudioTemplates) checkPlatformPayload(ctx context.Context, agentTypeID string, payload domain.TemplatePayload) (domain.TemplatePayload, error) {
	studio := t.Studio
	var fields []domain.AgentFieldError
	for kind, names := range payload.Slots {
		if !mappedKinds[kind] {
			fields = append(fields, domain.AgentFieldError{Field: "slots." + string(kind), Message: "only " + mappedKindsList + " values are slots"})
		}
		for _, name := range names {
			if !templateSlotName.MatchString(name) {
				fields = append(fields, domain.AgentFieldError{Field: "slots." + string(kind), Message: name + " must match " + templateSlotName.String()})
			}
		}
	}
	if len(fields) > 0 {
		return domain.TemplatePayload{}, &StudioValidationError{Fields: fields}
	}
	bindings := slices.Clone(payload.Bindings)
	// Models depend on the creating tenant, so they are checked only when an agent is made.
	offerable := slices.DeleteFunc(slices.Clone(bindings), func(binding domain.PortableBinding) bool {
		return binding.ResourceKind == domain.ResourceModel
	})
	offer, err := studio.offer(ctx, 0, agentTypeID, offerable)
	if err != nil {
		return domain.TemplatePayload{}, err
	}
	seen := map[string]bool{}
	named := map[domain.ResourceKind]map[string]bool{}
	for index, binding := range bindings {
		path := fmt.Sprintf("bindings[%d]", index)
		problems := checkPortableShape(path, binding, offer.sectionIDs)
		key := string(binding.ResourceKind) + "\x1f" + binding.ResourceID + "\x1f" + binding.Caption + "\x1f" + binding.Language
		if seen[key] {
			problems = append(problems, domain.AgentFieldError{Field: path, Message: "binds a resource another binding already binds"})
		}
		seen[key] = true
		if len(problems) > 0 {
			fields = append(fields, problems...)
			continue
		}
		value, err := scope.CanonicalValue(binding.ResourceKind, binding.Value)
		if err != nil {
			fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: err.Error()})
			continue
		}
		bindings[index].Value = json.RawMessage(value)
		if binding.ResourceKind == domain.ResourceApprovalRouting {
			// A platform template names no tenant's roles or positions: every approver is a slot.
			if _, _, err = remapApprovers(value, adoptTemplateCreate, func(slot string) (string, bool) {
				if named[binding.ResourceKind] == nil {
					named[binding.ResourceKind] = map[string]bool{}
				}
				named[binding.ResourceKind][slot] = true
				if !slices.Contains(payload.Slots[binding.ResourceKind], slot) {
					fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: slot + " is not a declared slot"})
				}
				return "role:" + slot, true
			}); err != nil {
				return domain.TemplatePayload{}, err
			}
			continue
		}
		if scope.PlatformNarrowingRules[binding.ResourceKind] != scope.NarrowSubset || binding.ResourceKind == domain.ResourceModel {
			continue
		}
		var members []string
		if err = json.Unmarshal(value, &members); err != nil {
			return domain.TemplatePayload{}, err
		}
		if missing := offer.missing(binding.ResourceKind, members); len(missing) > 0 {
			fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: "the agent type does not offer " + strings.Join(missing, ", ")})
		}
		if mappedKinds[binding.ResourceKind] {
			if named[binding.ResourceKind] == nil {
				named[binding.ResourceKind] = map[string]bool{}
			}
			for _, member := range members {
				named[binding.ResourceKind][member] = true
				if !slices.Contains(payload.Slots[binding.ResourceKind], member) {
					fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: member + " is not a declared slot"})
				}
			}
		}
	}
	if len(fields) > 0 {
		return domain.TemplatePayload{}, &StudioValidationError{Fields: fields}
	}
	// A slot no binding names could never be mapped at create, so the version would be unusable.
	for kind, names := range payload.Slots {
		for _, name := range names {
			if !named[kind][name] {
				fields = append(fields, domain.AgentFieldError{Field: "slots." + string(kind), Message: name + " is declared but no binding uses it"})
			}
		}
	}
	if len(fields) > 0 {
		return domain.TemplatePayload{}, &StudioValidationError{Fields: fields}
	}
	slots := map[domain.ResourceKind][]string{}
	for kind, names := range payload.Slots {
		sorted := slices.Clone(names)
		slices.Sort(sorted)
		slots[kind] = slices.Compact(sorted)
	}
	if len(slots) == 0 {
		slots = nil
	}
	return domain.TemplatePayload{Bindings: nonNilBindings(bindings), Slots: slots}, nil
}

// templateSeed is what an agent made from a template takes: the template's bindings with each slot
// mapped to a value of the tenant, the template's type version, and the template as its origin.
func (s *StudioService) templateSeed(ctx context.Context, actor domain.StudioActor, request domain.AgentCreateRequest) (*agentSeed, error) {
	version, err := s.Templates.Version(ctx, actor.TenantID, request.TemplateID, request.TemplateVersion)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, validationError("template_id", "the tenant may use no template with this id and version")
	}
	if err != nil {
		return nil, fmt.Errorf("load template %q: %w", request.TemplateID, err)
	}
	if version.AgentTypeID != request.AgentTypeID {
		return nil, validationError("template_id", "is a template of another agent type")
	}
	if err = s.checkTypeVersion(ctx, actor.TenantID, version.AgentTypeID, version.TypeVersion, version.TypeDigest, "template_id"); err != nil {
		return nil, err
	}
	adopted, err := s.adopt(ctx, actor, request.AgentTypeID, "template.bindings", version.Payload.Bindings, request.References, adoptTemplateCreate, version.Payload.Slots)
	if err != nil {
		return nil, err
	}
	origin := &draftOrigin{
		derived: domain.DerivedFrom{
			Source:  domain.CharterObjectRef{Kind: domain.DerivedFromTemplateKind, ID: version.TemplateID, External: true},
			Version: strconv.FormatInt(version.Version, 10), Digest: "sha256:" + version.Digest,
		},
		templateID: version.TemplateID, templateVersion: version.Version,
	}
	if version.Owner != domain.TemplatePlatform {
		origin.templateTenant = version.OwnerTenantID
	}
	return &agentSeed{
		bindings: adopted.scoped, typeVersion: version.TypeVersion, origin: origin, event: "CREATE",
		detail: fmt.Sprintf("template %s version %d", version.TemplateID, version.Version),
		draft: domain.AgentDraft{
			AgentTypeID: request.AgentTypeID, DisplayName: request.DisplayName, Languages: adopted.languages, ExpectedDraftRevision: 1,
		},
	}, nil
}

// templatePayloadDigest is the hex sha256 of a payload's canonical JSON, which a template version stores.
func templatePayloadDigest(payload domain.TemplatePayload) (string, error) {
	encoded, err := canonicalTemplatePayload(payload)
	if err != nil {
		return "", err
	}
	return valueDigest(encoded), nil
}

// canonicalTemplatePayload is the stored form of a payload: sorted keys, no insignificant space.
func canonicalTemplatePayload(payload domain.TemplatePayload) ([]byte, error) {
	payload.Bindings = nonNilBindings(payload.Bindings)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var value any
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func nonNilBindings(bindings []domain.PortableBinding) []domain.PortableBinding {
	if bindings == nil {
		return []domain.PortableBinding{}
	}
	return bindings
}

func validateTemplateSave(request domain.AgentTemplateSaveRequest) error {
	var fields []domain.AgentFieldError
	if strings.TrimSpace(request.AgentID) == "" {
		fields = append(fields, domain.AgentFieldError{Field: "agent_name", Message: "required"})
	}
	if request.TemplateID == "" {
		if key := strings.TrimSpace(request.IdempotencyKey); key == "" || len(key) > 120 || key != request.IdempotencyKey {
			fields = append(fields, domain.AgentFieldError{Field: "idempotency_key", Message: "a new template needs a key of 1 to 120 characters"})
		}
	} else if !tenantTemplateID.MatchString(request.TemplateID) {
		fields = append(fields, domain.AgentFieldError{Field: "template_id", Message: "must be a tenant template id"})
	}
	fields = append(fields, templateLabels(request.DisplayName, request.Description, request.ChangeSummary)...)
	for kind, mapping := range request.Slots {
		if !mappedKinds[kind] {
			fields = append(fields, domain.AgentFieldError{Field: "slots." + string(kind), Message: "only " + mappedKindsList + " values become slots"})
			continue
		}
		for from, to := range mapping {
			if from == "" || len(from) > maxReferenceValue || !templateSlotName.MatchString(to) {
				fields = append(fields, domain.AgentFieldError{Field: "slots." + string(kind) + "." + from, Message: "must map a value to a slot name matching " + templateSlotName.String()})
			}
		}
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

func templateLabels(displayName, description, changeSummary string) []domain.AgentFieldError {
	var fields []domain.AgentFieldError
	if name := strings.TrimSpace(displayName); name == "" || name != displayName || len(name) > 200 {
		fields = append(fields, domain.AgentFieldError{Field: "display_name", Message: "must contain 1 to 200 characters without surrounding spaces"})
	}
	if len(description) > 500 {
		fields = append(fields, domain.AgentFieldError{Field: "description", Message: "must not exceed 500 characters"})
	}
	if len(changeSummary) > 400 {
		fields = append(fields, domain.AgentFieldError{Field: "change_summary", Message: "must not exceed 400 characters"})
	}
	return fields
}

func validatePublication(actor domain.StudioActor, publication domain.AgentTemplatePublication, audienceLimit int) error {
	var fields []domain.AgentFieldError
	if !tenantTemplateID.MatchString(publication.TemplateID) {
		fields = append(fields, domain.AgentFieldError{Field: "template_id", Message: "must be a tenant template id"})
	}
	audience := publication.Audience
	if audience.All == (len(audience.Tenants) > 0) {
		fields = append(fields, domain.AgentFieldError{Field: "tenants", Message: "name either all or one or more tenants"})
	}
	if len(audience.Tenants) > audienceLimit {
		fields = append(fields, domain.AgentFieldError{Field: "tenants", Message: fmt.Sprintf("must not exceed %d tenants", audienceLimit)})
	}
	seen := map[int64]bool{}
	for _, tenant := range audience.Tenants {
		if tenant <= 0 || tenant == actor.TenantID || seen[tenant] {
			fields = append(fields, domain.AgentFieldError{Field: "tenants", Message: "must be distinct positive tenants other than the owner"})
			break
		}
		seen[tenant] = true
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}
