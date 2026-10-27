package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// Defaults of the StudioService and TableAgentTemplateStore limits on portable documents and templates.
const (
	DefaultMaxPortableBindings  = 1024
	DefaultMaxReferences        = 1024
	DefaultMaxTemplateAudience  = 100
	DefaultUsableTemplateLimit  = 200
	DefaultMaxPortableExtension = 256 << 10
)

const (
	maxReferenceValue    = 200
	maxPromptText        = 16000
	maxStudioEventDetail = 400
)

var templateSlotName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// mappedKinds hold values the owning tenant names: an import or a template maps each to a value of
// the target and never keeps the source's. An approval route's approver is mapped by "kind:ref".
var mappedKinds = map[domain.ResourceKind]bool{domain.ResourceKnowledge: true, domain.ResourceEntitlement: true, domain.ResourceApprovalRouting: true}

const mappedKindsList = "knowledge, entitlement, and approval_routing"

const (
	qSeedTypeVersion        = "scout_studio_seed_type_version"
	qSeedAgentType          = "scout_studio_seed_agent_type"
	qSeedPinTypeVersion     = "scout_studio_seed_pin_type_version"
	qSeedPromptSections     = "scout_studio_seed_prompt_sections"
	qSeedKnowledgeOwned     = "scout_studio_seed_knowledge_owned"
	qSeedDraftOrigin        = "scout_studio_seed_draft_origin"
	qSeedPutOrigin          = "scout_studio_seed_put_origin"
	qSeedDropScope          = "scout_studio_seed_drop_scope"
	qSeedEndScope           = "scout_studio_seed_end_scope"
	qSeedNonPlatformAtScope = "scout_studio_seed_non_platform_at_scope"
)

var studioSeedQueries = map[string]string{
	qSeedTypeVersion: `SELECT definition_digest FROM agent_type_version WHERE partner_id = ? AND agent_type_id = ? AND type_version = ?`,
	qSeedAgentType: `
SELECT p.agent_type_version, v.definition_digest
  FROM agent_profile p
  LEFT JOIN agent_type_version v
    ON v.partner_id = p.partner_id AND v.agent_type_id = p.agent_type_id AND v.type_version = p.agent_type_version
 WHERE p.partner_id = ? AND p.agent_id = ?`,
	qSeedPinTypeVersion: `UPDATE agent_profile SET agent_type_version = ? WHERE partner_id = ? AND agent_id = ?`,
	qSeedPromptSections: `SELECT id, caption FROM prompt_section`,
	qSeedKnowledgeOwned: `
SELECT knowledge_base_id FROM knowledge_base
 WHERE partner_id = ? AND knowledge_base_id = ANY(string_to_array(?, chr(31)))`,
	qSeedDraftOrigin: `
SELECT source_kind, source_id, source_namespace, source_external, source_version, source_digest,
       template_partner_id, template_id, template_version, catalog_template_id, catalog_template_version
  FROM agent_draft_origin WHERE partner_id = ? AND agent_id = ?`,
	qSeedPutOrigin: `
INSERT INTO agent_draft_origin (partner_id, agent_id, source_kind, source_id, source_namespace, source_external,
       source_version, source_digest, template_partner_id, template_id, template_version,
       catalog_template_id, catalog_template_version, recorded_by_kind, recorded_by_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, agent_id) DO UPDATE
   SET source_kind = EXCLUDED.source_kind, source_id = EXCLUDED.source_id, source_namespace = EXCLUDED.source_namespace,
       source_external = EXCLUDED.source_external, source_version = EXCLUDED.source_version,
       source_digest = EXCLUDED.source_digest, template_partner_id = EXCLUDED.template_partner_id,
       template_id = EXCLUDED.template_id, template_version = EXCLUDED.template_version,
       catalog_template_id = EXCLUDED.catalog_template_id, catalog_template_version = EXCLUDED.catalog_template_version,
       recorded_by_kind = EXCLUDED.recorded_by_kind, recorded_by_id = EXCLUDED.recorded_by_id,
       recorded_at = CURRENT_TIMESTAMP`,
	// A binding made in this same instant was never in force, so it is dropped rather than ended.
	qSeedDropScope: `
DELETE FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND endda IS NULL AND begda >= CURRENT_TIMESTAMP`,
	qSeedEndScope: `UPDATE config_scope_binding SET endda = CURRENT_TIMESTAMP WHERE partner_id = ? AND scope_id = ? AND endda IS NULL`,
	qSeedNonPlatformAtScope: `
SELECT COUNT(*) FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND endda IS NULL
   AND NOT (resource_kind_code = ANY(string_to_array(?, chr(31))))`,
	qGuardrailDraftGet:  studioGuardrailQueries[qGuardrailDraftGet],
	qGuardrailDraftPut:  studioGuardrailQueries[qGuardrailDraftPut],
	qGuardrailDraftDrop: studioGuardrailQueries[qGuardrailDraftDrop],
}

// studioCreateQueries is the catalog Studio reads and creates through; it adds the seed queries.
var studioCreateQueries = common.MergeMaps(studioQueries, studioSeedQueries)

// draftOrigin is what an agent's draft derives from: an imported document or a template version,
// tenant-owned (templateTenant set) or the platform's.
type draftOrigin struct {
	derived         domain.DerivedFrom
	templateTenant  int64
	templateID      string
	templateVersion int64
}

// agentSeed is what a created or replaced agent takes besides its type defaults: draft models and
// extension, the bindings of its own scope, its own guardrail rules when guardrails is set, the type
// version it pins, and where it came from. draft is the candidate the product's validators see.
type agentSeed struct {
	models      domain.AgentModelSelection
	extension   json.RawMessage
	bindings    []domain.ScopedBinding
	guardrails  *[]domain.GuardrailRule
	typeVersion string
	origin      *draftOrigin
	event       string
	detail      string
	draft       domain.AgentDraft
}

func (s *StudioService) draftOrigin(ctx context.Context, tenantID int64, agentID string) (*draftOrigin, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qSeedDraftOrigin, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("load draft origin: %w", err)
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	row := res.Rows[0]
	origin := &draftOrigin{derived: domain.DerivedFrom{
		Source: domain.CharterObjectRef{
			Kind: common.AsString(row[0]), ID: common.AsString(row[1]), Namespace: common.AsString(row[2]), External: common.AsBool(row[3]),
		},
		Version: common.AsString(row[4]), Digest: common.AsString(row[5]),
	}}
	switch {
	case common.AsString(row[7]) != "":
		origin.templateTenant, origin.templateID, origin.templateVersion = common.AsInt64(row[6]), common.AsString(row[7]), common.AsInt64(row[8])
	case common.AsString(row[9]) != "":
		origin.templateID, origin.templateVersion = common.AsString(row[9]), common.AsInt64(row[10])
	}
	return origin, nil
}

func (o *draftOrigin) derivedFrom() *domain.DerivedFrom {
	if o == nil {
		return nil
	}
	derived := o.derived
	return &derived
}

func derivedFromEqual(left, right *domain.DerivedFrom) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// promptCaptions reads the platform prompt section catalog both ways.
func (s *StudioService) promptCaptions(ctx context.Context) (map[int64]string, map[string]int64, error) {
	res, err := s.qs.Query(ctx, qSeedPromptSections)
	if err != nil {
		return nil, nil, fmt.Errorf("list prompt sections: %w", err)
	}
	captions := make(map[int64]string, len(res.Rows))
	ids := make(map[string]int64, len(res.Rows))
	for _, row := range res.Rows {
		id, caption := common.AsInt64(row[0]), common.AsString(row[1])
		captions[id], ids[caption] = caption, id
	}
	return captions, ids, nil
}

// portableBindings reads every binding in force at the agent's own scope. A product kind refuses,
// because it may carry anything and a partial export would import as a different agent.
func (s *StudioService) portableBindings(ctx context.Context, tenantID int64, scopeID string) ([]domain.PortableBinding, error) {
	stored, err := s.scopeRepository().Bindings(ctx, tenantID, []string{scopeID}, s.now())
	if err != nil {
		return nil, err
	}
	var captions map[int64]string
	portable := make([]domain.PortableBinding, 0, len(stored))
	for _, binding := range stored {
		if binding.ScopeID != scopeID {
			continue
		}
		if _, platform := scope.PlatformNarrowingRules[binding.ResourceKind]; !platform {
			return nil, validationError("bindings", fmt.Sprintf("the agent binds %s, a product kind that does not travel", binding.ResourceKind))
		}
		item := domain.PortableBinding{
			ResourceKind: binding.ResourceKind, MergeMode: binding.MergeMode, Sealed: binding.Sealed, Value: json.RawMessage(binding.Value),
		}
		if binding.ResourceKind != domain.ResourcePromptSection {
			item.ResourceID = binding.ResourceID
			portable = append(portable, item)
			continue
		}
		if captions == nil {
			if captions, _, err = s.promptCaptions(ctx); err != nil {
				return nil, err
			}
		}
		section, language, _ := strings.Cut(binding.ResourceID, "/")
		sectionID, parseErr := strconv.ParseInt(section, 10, 64)
		caption, known := captions[sectionID]
		if parseErr != nil || !known || language == "" {
			return nil, fmt.Errorf("%w: prompt binding %q names no prompt section", domain.ErrConflict, binding.ResourceID)
		}
		item.Caption, item.Language = caption, language
		portable = append(portable, item)
	}
	if limit := s.portableBindingLimit(); len(portable) > limit {
		return nil, fmt.Errorf("%w: the agent binds %d resources, more than the %d one document carries", domain.ErrConflict, len(portable), limit)
	}
	sort.Slice(portable, func(i, j int) bool {
		left, right := portable[i], portable[j]
		if left.ResourceKind != right.ResourceKind {
			return left.ResourceKind < right.ResourceKind
		}
		if left.ResourceID != right.ResourceID {
			return left.ResourceID < right.ResourceID
		}
		if left.Caption != right.Caption {
			return left.Caption < right.Caption
		}
		return left.Language < right.Language
	})
	return portable, nil
}

type adoptMode int

const (
	// adoptImport maps knowledge and entitlement values through the caller's references.
	adoptImport adoptMode = iota
	// adoptTemplateCreate maps a template's declared slots through the caller's references.
	adoptTemplateCreate
	// adoptTemplateSave replaces the agent's own values by slot names.
	adoptTemplateSave
)

// adoption is adopted bindings in both shapes, the languages their prompt bindings write, and for a
// template save the slot names they use.
type adoption struct {
	portable  []domain.PortableBinding
	scoped    []domain.ScopedBinding
	languages []domain.AgentLanguageDraft
	slots     map[domain.ResourceKind][]string
}

// adopt checks portable bindings for an agent of agentTypeID in the actor's tenant and maps every
// knowledge and entitlement value, so no value of another tenant survives. It collects every problem
// and refuses them together; what would widen authority is refused, never dropped. The caller sets
// each ScopedBinding's ScopeID.
func (s *StudioService) adopt(ctx context.Context, actor domain.StudioActor, agentTypeID, at string, bindings []domain.PortableBinding,
	mapping map[domain.ResourceKind]map[string]string, mode adoptMode, slots map[domain.ResourceKind][]string) (adoption, error) {
	if limit := s.portableBindingLimit(); len(bindings) > limit {
		return adoption{}, validationError(at, fmt.Sprintf("must not exceed %d bindings", limit))
	}
	offer, err := s.offer(ctx, actor.TenantID, agentTypeID, bindings)
	if err != nil {
		return adoption{}, err
	}
	refPath := "references."
	if mode == adoptTemplateSave {
		refPath = "slots."
	}
	var fields []domain.AgentFieldError
	used := map[domain.ResourceKind]map[string]bool{}
	var targets []string
	targetFrom := map[string]string{}
	result := adoption{slots: map[domain.ResourceKind][]string{}}
	seen := map[string]bool{}
	languages := map[string]*domain.AgentLanguageDraft{}
	// mapMember maps one value the source tenant names to the target's, or a template's slot.
	mapMember := func(kind domain.ResourceKind, member, path string) (string, bool) {
		target, ok := mapping[kind][member]
		switch {
		case mode == adoptTemplateCreate && !slices.Contains(slots[kind], member):
			fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: "names " + member + ", which the template does not declare as a slot"})
			return "", false
		case !ok:
			fields = append(fields, domain.AgentFieldError{Field: refPath + string(kind) + "." + member, Message: "map this value to a value of the tenant"})
			return "", false
		case mode == adoptTemplateSave && (!templateSlotName.MatchString(target) || target == member):
			fields = append(fields, domain.AgentFieldError{Field: refPath + string(kind) + "." + member, Message: "must be a slot name other than the value, matching " + templateSlotName.String()})
			return "", false
		}
		if used[kind] == nil {
			used[kind] = map[string]bool{}
		}
		used[kind][member] = true
		if mode == adoptTemplateSave && !slices.Contains(result.slots[kind], target) {
			result.slots[kind] = append(result.slots[kind], target)
		}
		return target, true
	}
	for index, binding := range bindings {
		path := fmt.Sprintf("%s[%d]", at, index)
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
		var prompt promptBindingValue
		if binding.ResourceKind == domain.ResourcePromptSection {
			if err = json.Unmarshal(value, &prompt); err != nil || len(prompt.Instruction) > maxPromptText || len(prompt.Output) > maxPromptText {
				fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: fmt.Sprintf("instruction and output must not exceed %d characters", maxPromptText)})
				continue
			}
		}
		if scope.PlatformNarrowingRules[binding.ResourceKind] == scope.NarrowSubset {
			var members []string
			if err = json.Unmarshal(value, &members); err != nil {
				return adoption{}, fmt.Errorf("decode %s value: %w", binding.ResourceKind, err)
			}
			if missing := offer.missing(binding.ResourceKind, members); len(missing) > 0 {
				fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: fmt.Sprintf("the agent type or tenant does not offer %s %s", binding.ResourceKind, strings.Join(missing, ", "))})
				continue
			}
			if mappedKinds[binding.ResourceKind] {
				mapped := make([]string, 0, len(members))
				for _, member := range members {
					target, ok := mapMember(binding.ResourceKind, member, path)
					if !ok {
						continue
					}
					mapped = append(mapped, target)
					if binding.ResourceKind == domain.ResourceKnowledge {
						targets = append(targets, target)
						targetFrom[target] = member
					}
				}
				encoded, err := json.Marshal(mapped)
				if err != nil {
					return adoption{}, err
				}
				if value, err = scope.CanonicalValue(binding.ResourceKind, encoded); err != nil {
					fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: err.Error()})
					continue
				}
			}
		}
		if binding.ResourceKind == domain.ResourceApprovalRouting {
			remapped, ok, err := remapApprovers(value, mode, func(member string) (string, bool) { return mapMember(binding.ResourceKind, member, path) })
			if err != nil {
				return adoption{}, err
			}
			if !ok {
				continue
			}
			if value, err = scope.CanonicalValue(binding.ResourceKind, remapped); err != nil {
				fields = append(fields, domain.AgentFieldError{Field: path + ".value", Message: err.Error()})
				continue
			}
			var invalid *StudioValidationError
			if err = s.checkSavedRouting(value); errors.As(err, &invalid) {
				for _, field := range invalid.Fields {
					fields = append(fields, domain.AgentFieldError{Field: path + "." + field.Field, Message: field.Message})
				}
				continue
			} else if err != nil {
				return adoption{}, err
			}
		}
		if binding.ResourceKind == domain.ResourceBudget {
			var invalid *StudioValidationError
			if err = s.checkSavedBudget(ctx, value); errors.As(err, &invalid) {
				for _, field := range invalid.Fields {
					fields = append(fields, domain.AgentFieldError{Field: path + "." + field.Field, Message: field.Message})
				}
				continue
			} else if err != nil {
				return adoption{}, err
			}
		}
		portable := binding
		portable.Value = json.RawMessage(value)
		result.portable = append(result.portable, portable)
		resourceID := binding.ResourceID
		if binding.ResourceKind == domain.ResourcePromptSection {
			sectionID := offer.sectionIDs[binding.Caption]
			resourceID = PromptResourceID(sectionID, binding.Language)
			language := languages[binding.Language]
			if language == nil {
				language = &domain.AgentLanguageDraft{LanguageCode: binding.Language}
				languages[binding.Language] = language
			}
			language.Sections = append(language.Sections, domain.AgentPromptSection{
				PromptSectionID: sectionID, Caption: binding.Caption,
				Layers: []domain.PromptLayer{{ScopeKind: "agent", MergeMode: binding.MergeMode, Sealed: binding.Sealed, Editable: true, Instruction: prompt.Instruction, Output: prompt.Output}},
			})
		}
		digest := valueDigest(value)
		result.scoped = append(result.scoped, domain.ScopedBinding{
			TenantID: actor.TenantID, ResourceKind: binding.ResourceKind, ResourceID: resourceID, ResourceVersion: digest[:12],
			MergeMode: binding.MergeMode, Sealed: binding.Sealed, Value: value, ValueDigest: digest, BoundBy: actorUserID(actor),
		})
	}
	for kind, entries := range mapping {
		if !mappedKinds[kind] {
			fields = append(fields, domain.AgentFieldError{Field: refPath + string(kind), Message: "only " + mappedKindsList + " values are mapped"})
			continue
		}
		for from := range entries {
			if !used[kind][from] {
				fields = append(fields, domain.AgentFieldError{Field: refPath + string(kind) + "." + from, Message: "is not a value any binding names"})
			}
		}
	}
	if mode == adoptTemplateCreate {
		for kind, names := range slots {
			for _, name := range names {
				if _, mapped := mapping[kind][name]; !mapped {
					fields = append(fields, domain.AgentFieldError{Field: refPath + string(kind) + "." + name, Message: "map this slot to a value of the tenant"})
				}
			}
		}
	}
	if len(fields) == 0 && len(targets) > 0 {
		owned, err := s.ownedKnowledge(ctx, actor.TenantID, targets)
		if err != nil {
			return adoption{}, err
		}
		for _, target := range targets {
			switch {
			case mode == adoptTemplateSave && owned[target]:
				fields = append(fields, domain.AgentFieldError{Field: refPath + "knowledge." + targetFrom[target], Message: "must not be a knowledge base id of the tenant"})
			case mode != adoptTemplateSave && !owned[target]:
				fields = append(fields, domain.AgentFieldError{Field: refPath + "knowledge." + targetFrom[target], Message: "names no knowledge base of the tenant"})
			}
		}
	}
	if len(fields) > 0 {
		return adoption{}, &StudioValidationError{Fields: fields}
	}
	for _, names := range result.slots {
		sort.Strings(names)
	}
	codes := make([]string, 0, len(languages))
	for code := range languages {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		result.languages = append(result.languages, *languages[code])
	}
	return result, nil
}

// checkPortableShape reports what makes one binding malformed: a kind with no platform narrowing
// rule, an id or merge mode its kind does not take, or a prompt caption the catalog lacks.
func checkPortableShape(path string, binding domain.PortableBinding, sectionIDs map[string]int64) []domain.AgentFieldError {
	if _, platform := scope.PlatformNarrowingRules[binding.ResourceKind]; !platform {
		return []domain.AgentFieldError{{Field: path + ".resource_kind", Message: "is not a kind an agent carries to another tenant"}}
	}
	var fields []domain.AgentFieldError
	if binding.ResourceKind == domain.ResourcePromptSection {
		if binding.ResourceID != "" {
			fields = append(fields, domain.AgentFieldError{Field: path + ".resource_id", Message: "a prompt section is named by caption and language"})
		}
		if _, known := sectionIDs[binding.Caption]; !known {
			fields = append(fields, domain.AgentFieldError{Field: path + ".caption", Message: "names no prompt section"})
		}
		if language := binding.Language; language == "" || len(language) > 16 || strings.TrimSpace(language) != language || strings.Contains(language, "/") {
			fields = append(fields, domain.AgentFieldError{Field: path + ".language", Message: "must contain 1 to 16 characters"})
		}
		if binding.MergeMode != domain.MergeAppend && binding.MergeMode != domain.MergeReplace {
			fields = append(fields, domain.AgentFieldError{Field: path + ".merge_mode", Message: "must be " + string(domain.MergeAppend) + " or " + string(domain.MergeReplace)})
		}
		return fields
	}
	if binding.Caption != "" || binding.Language != "" {
		fields = append(fields, domain.AgentFieldError{Field: path + ".caption", Message: "only a prompt section has a caption and language"})
	}
	if id := strings.TrimSpace(binding.ResourceID); id == "" || len(id) > 80 || id != binding.ResourceID {
		fields = append(fields, domain.AgentFieldError{Field: path + ".resource_id", Message: "must contain 1 to 80 characters"})
	} else if singletonSetting(binding.ResourceKind) && id != string(binding.ResourceKind) {
		fields = append(fields, domain.AgentFieldError{Field: path + ".resource_id", Message: "must equal resource_kind for this setting"})
	}
	switch binding.MergeMode {
	case domain.MergeReplace, domain.MergeAppend, domain.MergeIntersect:
	default:
		fields = append(fields, domain.AgentFieldError{Field: path + ".merge_mode", Message: "must be " + string(domain.MergeReplace) + ", " + string(domain.MergeAppend) + ", or " + string(domain.MergeIntersect)})
	}
	return fields
}

// offering is what bindings of an agent of one type in one tenant may name: the type's tools and
// skills, the active models the tenant may use, and the platform's prompt sections. Each is read
// only when a binding names that kind.
type offering struct {
	tools, skills, models map[string]bool
	sectionIDs            map[string]int64
}

func (s *StudioService) offer(ctx context.Context, tenantID int64, agentTypeID string, bindings []domain.PortableBinding) (offering, error) {
	kinds := map[domain.ResourceKind]bool{}
	for _, binding := range bindings {
		kinds[binding.ResourceKind] = true
	}
	var offer offering
	if kinds[domain.ResourceTool] || kinds[domain.ResourceSkill] {
		descriptor, err := s.descriptor(ctx, agentTypeID)
		if err != nil {
			return offering{}, err
		}
		offer.tools, offer.skills = map[string]bool{}, map[string]bool{}
		for _, tool := range descriptor.Tools {
			offer.tools[tool.ToolID] = true
		}
		for _, skill := range descriptor.Skills {
			offer.skills[skill.SkillID] = true
		}
	}
	if kinds[domain.ResourceModel] {
		if s.Catalog == nil {
			return offering{}, fmt.Errorf("%w: model bindings cannot be checked without a model catalog", domain.ErrNotReady)
		}
		models, err := s.Catalog.List(ctx, tenantID)
		if err != nil {
			return offering{}, fmt.Errorf("list tenant models: %w", err)
		}
		offer.models = make(map[string]bool, len(models))
		for _, model := range models {
			if model.Active {
				offer.models[model.Reference.ProviderID+"/"+model.Reference.ModelID] = true
			}
		}
	}
	if kinds[domain.ResourcePromptSection] {
		_, ids, err := s.promptCaptions(ctx)
		if err != nil {
			return offering{}, err
		}
		offer.sectionIDs = ids
	}
	return offer, nil
}

// descriptor is the product's type; without a type catalog tools and skills cannot be checked.
func (s *StudioService) descriptor(ctx context.Context, agentTypeID string) (domain.AgentTypeDescriptor, error) {
	if s.Kinds == nil {
		return domain.AgentTypeDescriptor{}, fmt.Errorf("%w: the tools and skills of agent type %q cannot be checked without an agent type catalog", domain.ErrNotReady, agentTypeID)
	}
	descriptor, err := s.Kinds.Get(ctx, agentTypeID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.AgentTypeDescriptor{}, validationError("agent_type", "the product does not offer this agent type")
	}
	if err != nil {
		return domain.AgentTypeDescriptor{}, fmt.Errorf("load agent type %q: %w", agentTypeID, err)
	}
	return descriptor, nil
}

func (o offering) missing(kind domain.ResourceKind, members []string) []string {
	allowed := map[domain.ResourceKind]map[string]bool{domain.ResourceTool: o.tools, domain.ResourceSkill: o.skills, domain.ResourceModel: o.models}[kind]
	if allowed == nil {
		return nil
	}
	var missing []string
	for _, member := range members {
		if !allowed[member] {
			missing = append(missing, member)
		}
	}
	return missing
}

func (s *StudioService) ownedKnowledge(ctx context.Context, tenantID int64, ids []string) (map[string]bool, error) {
	res, err := s.qs.Query(ctx, qSeedKnowledgeOwned, tenantID, strings.Join(ids, "\x1f"))
	if err != nil {
		return nil, fmt.Errorf("check knowledge bases: %w", err)
	}
	owned := make(map[string]bool, len(res.Rows))
	for _, row := range res.Rows {
		owned[common.AsString(row[0])] = true
	}
	return owned, nil
}

// checkTypeVersion refuses a named type version the tenant lacks or holds with another digest; an
// empty version pins nothing.
func (s *StudioService) checkTypeVersion(ctx context.Context, tenantID int64, agentTypeID, version, digest, field string) error {
	if version == "" {
		return nil
	}
	res, err := s.qs.Query(ctx, qSeedTypeVersion, tenantID, agentTypeID, version)
	if err != nil {
		return fmt.Errorf("load agent type version: %w", err)
	}
	if len(res.Rows) == 0 || strings.TrimSpace(common.AsString(res.Rows[0][0])) != digest {
		return validationError(field, "the tenant has no version "+version+" of agent type "+agentTypeID+" with this digest")
	}
	return nil
}

// validateReferences checks the shape of a knowledge and entitlement mapping of at most limit entries.
func validateReferences(field string, references map[domain.ResourceKind]map[string]string, limit int) []domain.AgentFieldError {
	var fields []domain.AgentFieldError
	count := 0
	for kind, mapping := range references {
		if !mappedKinds[kind] {
			fields = append(fields, domain.AgentFieldError{Field: field + "." + string(kind), Message: "only " + mappedKindsList + " values are mapped"})
			continue
		}
		for from, to := range mapping {
			count++
			if strings.TrimSpace(from) != from || strings.TrimSpace(to) != to || from == "" || to == "" || len(from) > maxReferenceValue || len(to) > maxReferenceValue {
				fields = append(fields, domain.AgentFieldError{Field: field + "." + string(kind) + "." + from, Message: fmt.Sprintf("must map a value to a value of 1 to %d characters", maxReferenceValue)})
			} else if _, valid := approverTarget(to); kind == domain.ResourceApprovalRouting && !valid {
				fields = append(fields, domain.AgentFieldError{Field: field + "." + string(kind) + "." + from, Message: "must map an approver to role:<ref> or position:<ref>"})
			}
		}
	}
	if count > limit {
		fields = append(fields, domain.AgentFieldError{Field: field, Message: fmt.Sprintf("must not exceed %d mappings", limit)})
	}
	return fields
}

// canonicalReferences renders a mapping deterministically for request digests.
func canonicalReferences(references map[domain.ResourceKind]map[string]string) string {
	var parts []string
	for kind, mapping := range references {
		for from, to := range mapping {
			parts = append(parts, string(kind)+"\x1e"+from+"\x1e"+to)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1d")
}

// seededScopes answers the scope chain as it will be once the agent's own scope holds exactly the
// seed's bindings; a scope not provisioned yet, such as a new agent's, is answered from virtual.
type seededScopes struct {
	contract.ScopeRepository
	scopeID  string
	bindings []domain.ScopedBinding
	virtual  []domain.Scope
}

func (o seededScopes) Chain(ctx context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
	chain, err := o.ScopeRepository.Chain(ctx, tenantID, scopeID)
	if !errors.Is(err, domain.ErrNotFound) {
		return chain, err
	}
	for _, node := range o.virtual {
		if node.ScopeID == scopeID && node.ParentScopeID != scopeID {
			parent, parentErr := o.Chain(ctx, tenantID, node.ParentScopeID)
			if parentErr != nil {
				return nil, parentErr
			}
			return append(parent, node), nil
		}
	}
	return nil, err
}

func (o seededScopes) Bindings(ctx context.Context, tenantID int64, scopeIDs []string, asOf time.Time) ([]domain.ScopedBinding, error) {
	stored, err := o.ScopeRepository.Bindings(ctx, tenantID, scopeIDs, asOf)
	if err != nil {
		return nil, err
	}
	bindings := slices.DeleteFunc(stored, func(binding domain.ScopedBinding) bool { return binding.ScopeID == o.scopeID })
	if slices.Contains(scopeIDs, o.scopeID) {
		bindings = append(bindings, o.bindings...)
	}
	return bindings, nil
}

// checkSeed compiles the agent's chain with the seed's bindings at its scope, so a seed that widens
// what the agent inherits, or overrides a sealed value, is refused before anything is written, and
// runs the draft checks and the product's validators over the candidate draft.
func (s *StudioService) checkSeed(ctx context.Context, actor domain.StudioActor, agentID string, scopes domain.PromptScopes, seed *agentSeed) error {
	placed := slices.Clone(seed.bindings)
	for index := range placed {
		placed[index].ScopeID = scopes.AgentScopeID
	}
	overlay := seededScopes{ScopeRepository: s.scopeRepository(), scopeID: scopes.AgentScopeID, bindings: placed, virtual: []domain.Scope{
		{TenantID: actor.TenantID, ScopeID: scopes.TypeScopeID, ParentScopeID: RootPromptScopeID, ScopeKind: "agent_type", DisplayName: seed.draft.AgentTypeID},
		{TenantID: actor.TenantID, ScopeID: scopes.AgentScopeID, ParentScopeID: scopes.TypeScopeID, ScopeKind: "agent", DisplayName: seed.draft.DisplayName},
	}}
	release, err := s.compileOver(ctx, overlay, actor.TenantID, scopes.AgentScopeID, agentID, nil)
	if err != nil {
		return err
	}
	if err = validateSingletonSettings(release.Resources); err != nil {
		return err
	}
	candidate := seed.draft
	candidate.AgentID, candidate.PromptScopes = agentID, scopes
	candidate.Languages = slices.Clone(candidate.Languages)
	for index := range candidate.Languages {
		candidate.Languages[index].Sections = slices.Clone(candidate.Languages[index].Sections)
		for section := range candidate.Languages[index].Sections {
			layers := slices.Clone(candidate.Languages[index].Sections[section].Layers)
			for layer := range layers {
				if layers[layer].ScopeKind == "agent" && layers[layer].ScopeID == "" {
					layers[layer].ScopeID = scopes.AgentScopeID
				}
			}
			candidate.Languages[index].Sections[section].Layers = layers
		}
	}
	return s.validateDraft(ctx, actor.TenantID, candidate, false)
}

// plantSeed writes a seed at the agent's scope inside the caller's transaction: its bindings, the type
// version it pins, its own guardrail rules when set, and its origin.
func (s *StudioService) plantSeed(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, agentID, scopeID string, seed *agentSeed) error {
	for _, binding := range seed.bindings {
		if _, err := tx.Query(ctx, qStudioInsertSetting, actor.TenantID, scopeID, string(binding.ResourceKind), binding.ResourceID,
			binding.ResourceVersion, string(binding.MergeMode), binding.Sealed, string(binding.Value), binding.ValueDigest, actorUser(actor)); err != nil {
			return fmt.Errorf("bind %s %q: %w", binding.ResourceKind, binding.ResourceID, err)
		}
	}
	if seed.typeVersion != "" {
		if _, err := tx.Query(ctx, qSeedPinTypeVersion, seed.typeVersion, actor.TenantID, agentID); err != nil {
			return fmt.Errorf("pin agent type version: %w", err)
		}
	}
	if seed.guardrails != nil {
		if len(*seed.guardrails) == 0 {
			if _, err := tx.Query(ctx, qGuardrailDraftDrop, actor.TenantID, agentID); err != nil {
				return fmt.Errorf("drop agent guardrails: %w", err)
			}
		} else {
			envelope, digest, _, err := canonicalRules(*seed.guardrails)
			if err != nil {
				return err
			}
			if _, err = tx.Query(ctx, qGuardrailDraftPut, actor.TenantID, agentID, string(envelope), digest, actorUser(actor)); err != nil {
				return fmt.Errorf("store agent guardrails: %w", err)
			}
		}
	}
	if origin := seed.origin; origin != nil {
		var templateTenant, templateID, templateVersion, catalogID, catalogVersion any
		switch {
		case origin.templateID != "" && origin.templateTenant > 0:
			templateTenant, templateID, templateVersion = origin.templateTenant, origin.templateID, origin.templateVersion
		case origin.templateID != "":
			catalogID, catalogVersion = origin.templateID, origin.templateVersion
		}
		source := origin.derived.Source
		if _, err := tx.Query(ctx, qSeedPutOrigin, actor.TenantID, agentID, source.Kind, source.ID, common.NullIfEmpty(source.Namespace), source.External,
			origin.derived.Version, origin.derived.Digest, templateTenant, templateID, templateVersion, catalogID, catalogVersion,
			string(actor.Principal.Kind), actor.Principal.ID); err != nil {
			return fmt.Errorf("record draft origin: %w", err)
		}
	}
	return nil
}

// canonicalSection re-encodes a frozen section the way publication stores it, so a section that
// travelled through another encoding digests as it was published.
func canonicalSection(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func clipDetail(detail string) string {
	if len(detail) <= maxStudioEventDetail {
		return detail
	}
	return detail[:maxStudioEventDetail]
}

func compactExtension(raw json.RawMessage, limit int) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("must not exceed %d bytes", limit)
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// remapApprovers rewrites every approver of an approval_routing value through mapMember. The member
// is "kind:ref", or the slot name a template stored as the ref; the target is "kind:ref", or a slot
// name when a template is saved. ok is false when any approver could not be mapped.
func remapApprovers(value []byte, mode adoptMode, mapMember func(string) (string, bool)) ([]byte, bool, error) {
	var routing domain.ApprovalRouting
	if err := json.Unmarshal(value, &routing); err != nil {
		return nil, false, fmt.Errorf("decode approval routing: %w", err)
	}
	ok, invalid := true, ""
	remap := func(approver *domain.ApproverReference) {
		member := string(approver.Kind) + ":" + approver.Ref
		if mode == adoptTemplateCreate {
			member = approver.Ref
		}
		target, mapped := mapMember(member)
		if !mapped {
			ok = false
			return
		}
		if mode == adoptTemplateSave {
			approver.Ref = target
			return
		}
		reference, valid := approverTarget(target)
		if !valid {
			invalid = target
			return
		}
		*approver = reference
	}
	for index := range routing.Routes {
		route := &routing.Routes[index]
		remap(&route.Approver)
		for step := range route.Escalation {
			if route.Escalation[step].Approver != nil {
				remap(route.Escalation[step].Approver)
			}
		}
	}
	if invalid != "" {
		return nil, false, fmt.Errorf("%w: approver %q must be role:<ref> or position:<ref>", domain.ErrValidation, invalid)
	}
	if !ok {
		return nil, false, nil
	}
	encoded, err := json.Marshal(routing)
	return encoded, err == nil, err
}

// approverTarget parses a mapped approver, "role:<ref>" or "position:<ref>".
func approverTarget(target string) (domain.ApproverReference, bool) {
	kind, ref, found := strings.Cut(target, ":")
	if !found || ref == "" || (kind != string(domain.ApproverRole) && kind != string(domain.ApproverPosition)) {
		return domain.ApproverReference{}, false
	}
	return domain.ApproverReference{Kind: domain.ApproverReferenceKind(kind), Ref: ref}, true
}
