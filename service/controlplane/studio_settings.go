package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// settingsVersion is the effective release's agent version while a draft compiles.
const settingsVersion = "draft"

// Settings returns the bindings other than prompt sections on the agent's scope chain and
// what they resolve to; a chain that does not compile is reported in Problem.
func (s *StudioService) Settings(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentSettings, error) {
	if err := s.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentSettings{}, err
	}
	draft, err := s.getDraft(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	scopes, err := s.promptScopes(ctx, actor.TenantID, agentID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	settings := domain.AgentSettings{
		AgentID: agentID, Scopes: scopes, DraftRevision: draft.ExpectedDraftRevision,
		PromptProfileRevision: draft.ExpectedPromptProfileRevision,
	}
	chain, err := s.scopeRepository().Chain(ctx, actor.TenantID, scopes.AgentScopeID)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	kinds := make(map[string]string, len(chain))
	ids := make([]string, 0, len(chain))
	for _, node := range chain {
		kinds[node.ScopeID] = node.ScopeKind
		ids = append(ids, node.ScopeID)
	}
	bindings, err := s.scopeRepository().Bindings(ctx, actor.TenantID, ids, s.now())
	if err != nil {
		return domain.AgentSettings{}, err
	}
	for _, binding := range bindings {
		if binding.ResourceKind == domain.ResourcePromptSection {
			continue
		}
		settings.Bindings = append(settings.Bindings, domain.AgentSetting{
			ScopeID: binding.ScopeID, ScopeKind: kinds[binding.ScopeID],
			Editable:     binding.ScopeID == scopes.AgentScopeID || binding.ScopeID == scopes.TypeScopeID,
			ResourceKind: binding.ResourceKind, ResourceID: binding.ResourceID, ResourceVersion: binding.ResourceVersion,
			MergeMode: binding.MergeMode, Sealed: binding.Sealed, Value: binding.Value,
		})
	}
	release, err := s.compileSettings(ctx, actor.TenantID, scopes.AgentScopeID, agentID, nil, nil)
	if settingsProblem(err) {
		settings.Problem = err.Error()
		return settings, nil
	}
	if err != nil {
		return domain.AgentSettings{}, err
	}
	for _, resource := range release.Resources {
		if resource.ResourceKind != domain.ResourcePromptSection {
			settings.Effective = append(settings.Effective, domain.ResourceExplanation{
				ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID, Value: resource.Value,
				Winner: resource.Source, Superseded: resource.Superseded, Sealed: resource.Source.Sealed,
			})
		}
	}
	return settings, nil
}

// SaveSetting binds or ends one resource. The whole chain is compiled with the change first,
// so a value that broadens what it inherits, or sits under a sealed one, is refused unwritten.
func (s *StudioService) SaveSetting(ctx context.Context, actor domain.StudioActor, change domain.AgentSettingChange) (domain.AgentSettings, error) {
	draft, err := s.editable(ctx, actor, domain.StudioEdit, change.AgentID)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	scopes, err := s.promptScopes(ctx, actor.TenantID, change.AgentID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	binding, err := settingBinding(actor, scopes, change)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	if change.ResourceKind == domain.ResourceBudget && !change.Remove {
		if err = s.checkSavedBudget(ctx, binding.Value); err != nil {
			return domain.AgentSettings{}, err
		}
	}
	if change.ResourceKind == domain.ResourceApprovalRouting && !change.Remove {
		if err = s.checkSavedRouting(binding.Value); err != nil {
			return domain.AgentSettings{}, err
		}
	}
	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return domain.AgentSettings{}, fmt.Errorf("begin setting transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	lockedDraft, err := tx.Query(ctx, qStudioLockDraft, actor.TenantID, change.AgentID)
	if err != nil {
		return domain.AgentSettings{}, fmt.Errorf("lock setting draft: %w", err)
	}
	if len(lockedDraft.Rows) == 0 {
		return domain.AgentSettings{}, domain.ErrNotFound
	}
	lockedAlias, err := tx.Query(ctx, qStudioLockAlias, actor.TenantID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentSettings{}, fmt.Errorf("lock setting type: %w", err)
	}
	// A type without an alias has prompt profile revision 0, as Publish reads it.
	aliasRevision := int64(0)
	if len(lockedAlias.Rows) > 0 {
		aliasRevision = common.AsInt64(lockedAlias.Rows[0][1])
	}
	if common.AsInt64(lockedDraft.Rows[0][0]) != change.ExpectedDraftRevision || aliasRevision != change.ExpectedPromptProfileRevision {
		return domain.AgentSettings{}, domain.ErrRevisionConflict
	}
	if change.ScopeID == scopes.TypeScopeID && len(lockedAlias.Rows) == 0 {
		return domain.AgentSettings{}, fmt.Errorf("%w: type %q has no alias to hold its defaults' revision", domain.ErrConflict, draft.AgentTypeID)
	}
	if _, err = s.compileSettings(ctx, actor.TenantID, scopes.AgentScopeID, change.AgentID, &binding, nil); err != nil {
		return domain.AgentSettings{}, err
	}
	var loosening *gateLoosening
	if change.ResourceKind == domain.ResourceEvaluationGate {
		after := changedScopes{ScopeRepository: s.scopeRepository(), change: binding}
		if loosening, err = s.checkGateChange(ctx, actor, change.AgentID, change.ScopeID, after); err != nil {
			return domain.AgentSettings{}, err
		}
	}
	bump, args := qStudioBumpDraft, []any{actorUser(actor), actor.TenantID, change.AgentID, change.ExpectedDraftRevision}
	if change.ScopeID == scopes.TypeScopeID {
		bump, args = qStudioBumpAlias, []any{actorUser(actor), actor.TenantID, draft.AgentTypeID, change.ExpectedPromptProfileRevision}
	}
	bumped, err := tx.Query(ctx, bump, args...)
	if err != nil {
		return domain.AgentSettings{}, fmt.Errorf("bump setting revision: %w", err)
	}
	if len(bumped.Rows) == 0 {
		return domain.AgentSettings{}, domain.ErrRevisionConflict
	}
	key := []any{actor.TenantID, change.ScopeID, string(change.ResourceKind), change.ResourceID}
	if _, err = tx.Query(ctx, qStudioDropSetting, key...); err != nil {
		return domain.AgentSettings{}, fmt.Errorf("drop setting: %w", err)
	}
	if _, err = tx.Query(ctx, qStudioEndSetting, key...); err != nil {
		return domain.AgentSettings{}, fmt.Errorf("end setting: %w", err)
	}
	detail := string(change.ResourceKind) + " " + change.ResourceID + " at " + change.ScopeID
	if change.Remove {
		detail = "removed " + detail
	} else if _, err = tx.Query(ctx, qStudioInsertSetting, actor.TenantID, change.ScopeID, string(binding.ResourceKind),
		binding.ResourceID, binding.ResourceVersion, string(binding.MergeMode), binding.Sealed, string(binding.Value),
		binding.ValueDigest, actorUser(actor)); err != nil {
		return domain.AgentSettings{}, fmt.Errorf("bind setting: %w", err)
	}
	if err = auditStudio(ctx, tx, actor, change.AgentID, "SETTING", detail); err != nil {
		return domain.AgentSettings{}, err
	}
	if err = s.recordGateLoosening(ctx, tx, actor, loosening); err != nil {
		return domain.AgentSettings{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentSettings{}, fmt.Errorf("commit setting: %w", err)
	}
	committed = true
	return s.Settings(ctx, actor, change.AgentID)
}

// freeze compiles the agent's settings and narrows the definition by them: the type's tools
// and skills to the tool and skill sets, and its models must be in the model set. It digests
// the definition with the settings and returns the release to store with it.
func (s *StudioService) freeze(ctx context.Context, tenantID int64, draft domain.AgentDraft, definition *domain.AgentDefinition, compiledBy *int64) (domain.EffectiveRelease, error) {
	scopes, err := s.promptScopes(ctx, tenantID, definition.AgentID, definition.AgentTypeID)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}
	release, err := s.compileSettings(ctx, tenantID, scopes.AgentScopeID, definition.AgentID, nil, compiledBy)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}
	if err = validateSingletonSettings(release.Resources); err != nil {
		return domain.EffectiveRelease{}, err
	}
	release = scope.WithoutOperationalSettings(release)
	if err = s.init(ctx); err != nil {
		return domain.EffectiveRelease{}, err
	}
	if err = s.checkBudget(ctx, s.qs, release.Resources, *definition); err != nil {
		return domain.EffectiveRelease{}, err
	}
	if s.Kinds != nil {
		descriptor, err := s.Kinds.Get(ctx, definition.AgentTypeID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.EffectiveRelease{}, fmt.Errorf("load agent type %q: %w", definition.AgentTypeID, err)
		}
		definition.Tools, definition.ToolLoop, definition.Skills = slices.Clone(descriptor.Tools), descriptor.ToolLoop, slices.Clone(descriptor.Skills)
	}
	if allowed, bound, err := scope.SetValue(release.Resources, domain.ResourceTool); err != nil {
		return domain.EffectiveRelease{}, err
	} else if bound {
		definition.Tools = slices.DeleteFunc(definition.Tools, func(tool domain.ToolReference) bool { return !allowed[tool.ToolID] })
	}
	if allowed, bound, err := scope.SetValue(release.Resources, domain.ResourceSkill); err != nil {
		return domain.EffectiveRelease{}, err
	} else if bound {
		definition.Skills = slices.DeleteFunc(definition.Skills, func(skill domain.SkillReference) bool { return !allowed[skill.SkillID] })
	}
	if allowed, bound, err := scope.SetValue(release.Resources, domain.ResourceModel); err != nil {
		return domain.EffectiveRelease{}, err
	} else if bound {
		for _, reference := range selectedModels(definition.Models) {
			if !allowed[reference.ProviderID+"/"+reference.ModelID] {
				return domain.EffectiveRelease{}, validationError("models", "model "+reference.ProviderID+"/"+reference.ModelID+" is outside the agent's model setting")
			}
		}
	}
	definition.SettingsDigest = scope.SettingsDigest(release.Resources)
	if definition.Sections, err = s.sections(ctx, tenantID, draft); err != nil {
		return domain.EffectiveRelease{}, err
	}
	origin, err := s.draftOrigin(ctx, tenantID, definition.AgentID)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}
	definition.DerivedFrom = origin.derivedFrom()
	if definition.DefinitionDigest, err = s.Compiler.DefinitionDigest(*definition); err != nil {
		return domain.EffectiveRelease{}, err
	}
	return release, nil
}

// sections collects each composed DefinitionSection of the draft as canonical JSON; a
// section that returns nothing is left out.
func (s *StudioService) sections(ctx context.Context, tenantID int64, draft domain.AgentDraft) (map[string]json.RawMessage, error) {
	var sections map[string]json.RawMessage
	for _, section := range s.Sections {
		name := section.Name()
		if strings.TrimSpace(name) == "" || sections[name] != nil {
			return nil, fmt.Errorf("studio service: definition section names must be unique and non-empty, got %q", name)
		}
		raw, err := section.Section(ctx, tenantID, draft)
		if err != nil {
			return nil, fmt.Errorf("definition section %q: %w", name, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("%w: definition section %q is not JSON: %v", domain.ErrValidation, name, err)
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if sections == nil {
			sections = map[string]json.RawMessage{}
		}
		sections[name] = canonical
	}
	return sections, nil
}

func sectionNames(maps ...map[string]json.RawMessage) []string {
	var names []string
	for _, m := range maps {
		for name := range m {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// storeSettings freezes the compiled settings for the published version in the publish transaction.
func storeSettings(ctx context.Context, tx keelport.TxQueryService, release domain.EffectiveRelease, version string) error {
	release.AgentVersion = version
	release.Digest = scope.Digest(release)
	return scope.PutRelease(ctx, tx, release)
}

func (s *StudioService) compileSettings(ctx context.Context, tenantID int64, scopeID, agentID string, change *domain.ScopedBinding, compiledBy *int64) (domain.EffectiveRelease, error) {
	var scopes contract.ScopeRepository = s.scopeRepository()
	if change != nil {
		scopes = changedScopes{ScopeRepository: scopes, change: *change}
	}
	return s.compileOver(ctx, scopes, tenantID, scopeID, agentID, compiledBy)
}

// compileOver compiles the chain of scopeID as scopes answers it.
func (s *StudioService) compileOver(ctx context.Context, scopes contract.ScopeRepository, tenantID int64, scopeID, agentID string, compiledBy *int64) (domain.EffectiveRelease, error) {
	mergers := s.Mergers
	if mergers == nil {
		registry, err := scope.NewMergerRegistry()
		if err != nil {
			return domain.EffectiveRelease{}, err
		}
		mergers = registry
	}
	compiler, err := scope.NewCompiler(scopes, mergers, &scope.LatticeChecker{}, s.MaxScopeDepth)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}
	compiler.Now = s.now
	return compiler.Compile(ctx, domain.CompileRequest{
		TenantID: tenantID, AgentID: agentID, AgentVersion: settingsVersion, ScopeID: scopeID, CompiledBy: compiledBy,
	})
}

func (s *StudioService) scopeRepository() contract.ScopeRepository {
	if s.Scopes != nil {
		return s.Scopes
	}
	return &scope.TableScopeRepository{DB: s.DB}
}

// changedScopes answers the chain's bindings as they would be after one change; a
// removal is a binding with no value.
type changedScopes struct {
	contract.ScopeRepository
	change domain.ScopedBinding
}

func (c changedScopes) Bindings(ctx context.Context, tenantID int64, scopeIDs []string, asOf time.Time) ([]domain.ScopedBinding, error) {
	bindings, err := c.ScopeRepository.Bindings(ctx, tenantID, scopeIDs, asOf)
	if err != nil {
		return nil, err
	}
	bindings = slices.DeleteFunc(bindings, func(binding domain.ScopedBinding) bool {
		return binding.ScopeID == c.change.ScopeID && binding.ResourceKind == c.change.ResourceKind && binding.ResourceID == c.change.ResourceID
	})
	if c.change.Value != nil {
		bindings = append(bindings, c.change)
	}
	return bindings, nil
}

func settingBinding(actor domain.StudioActor, scopes domain.PromptScopes, change domain.AgentSettingChange) (domain.ScopedBinding, error) {
	var fields []domain.AgentFieldError
	if change.ScopeID != scopes.AgentScopeID && change.ScopeID != scopes.TypeScopeID {
		fields = append(fields, domain.AgentFieldError{Field: "scope_id", Message: "must be the agent's scope or its type's"})
	}
	if change.ResourceKind == domain.ResourcePromptSection {
		fields = append(fields, domain.AgentFieldError{Field: "resource_kind", Message: "prompt sections are saved with the draft"})
	}
	if id := strings.TrimSpace(change.ResourceID); id == "" || len(id) > 80 || id != change.ResourceID {
		fields = append(fields, domain.AgentFieldError{Field: "resource_id", Message: "must contain 1 to 80 characters"})
	} else if singletonSetting(change.ResourceKind) && id != string(change.ResourceKind) {
		fields = append(fields, domain.AgentFieldError{Field: "resource_id", Message: "must equal resource_kind for this setting"})
	}
	binding := domain.ScopedBinding{
		TenantID: actor.TenantID, ScopeID: change.ScopeID, ResourceKind: change.ResourceKind, ResourceID: change.ResourceID,
		MergeMode: change.MergeMode, Sealed: change.Sealed, BoundBy: actorUserID(actor),
	}
	if binding.MergeMode == "" {
		binding.MergeMode = domain.MergeReplace
	}
	if !change.Remove {
		value, err := scope.CanonicalValue(change.ResourceKind, change.Value)
		if err != nil {
			fields = append(fields, domain.AgentFieldError{Field: "value", Message: err.Error()})
		}
		sum := sha256.Sum256(value)
		binding.Value, binding.ValueDigest = value, hex.EncodeToString(sum[:])
		binding.ResourceVersion = binding.ValueDigest[:12]
	}
	if len(fields) > 0 {
		return domain.ScopedBinding{}, &StudioValidationError{Fields: fields}
	}
	return binding, nil
}

func singletonSetting(kind domain.ResourceKind) bool {
	switch kind {
	case domain.ResourceTool, domain.ResourceKnowledge, domain.ResourceModel, domain.ResourceEntitlement,
		domain.ResourceBudget, domain.ResourceAutonomy, domain.ResourceSkill, domain.ResourceLimits, domain.ResourceApprovalRouting,
		domain.ResourceEvaluationGate, domain.ResourceHealthAlerts:
		return true
	default:
		return false
	}
}

func validateSingletonSettings(resources []domain.EffectiveResource) error {
	seen := make(map[domain.ResourceKind]struct{})
	for _, resource := range resources {
		if !singletonSetting(resource.ResourceKind) {
			continue
		}
		if _, duplicate := seen[resource.ResourceKind]; duplicate {
			return fmt.Errorf("%w: setting kind %q has more than one resource", domain.ErrValidation, resource.ResourceKind)
		}
		seen[resource.ResourceKind] = struct{}{}
	}
	return nil
}

// settingsProblem reports a chain that does not compile because of what it binds.
func settingsProblem(err error) bool {
	return errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrAuthorityExceeded) || errors.Is(err, domain.ErrSealed)
}

func (s *StudioService) compileSettingsOf(ctx context.Context, tenantID int64, draft domain.AgentDraft) (domain.EffectiveRelease, error) {
	return s.compileSettings(ctx, tenantID, draft.PromptScopes.AgentScopeID, draft.AgentID, nil, nil)
}

// loadRestoredSettings reads the restored version's frozen settings, which storeSettings freezes
// again for the new version. A version published before settings were frozen cannot serve a
// turn, so it is not restored.
func loadRestoredSettings(ctx context.Context, tx keelport.TxQueryService, tenantID int64, agentID, from string) (domain.EffectiveRelease, error) {
	source, err := tx.Query(ctx, qStudioGetSettings, tenantID, agentID, from)
	if err != nil {
		return domain.EffectiveRelease{}, fmt.Errorf("load restored settings: %w", err)
	}
	if len(source.Rows) == 0 {
		return domain.EffectiveRelease{}, fmt.Errorf("%w: version %s has no frozen settings; publish the agent instead", domain.ErrConflict, from)
	}
	release, err := decodeSettings(source.Rows[0], tenantID, agentID)
	if err != nil {
		return domain.EffectiveRelease{}, fmt.Errorf("decode restored settings: %w", err)
	}
	return release, nil
}

// decodeSettings decodes one qStudioGetSettings row; the caller sets the version.
func decodeSettings(row []any, tenantID int64, agentID string) (domain.EffectiveRelease, error) {
	release := domain.EffectiveRelease{TenantID: tenantID, AgentID: agentID, ScopeID: common.AsString(row[0]), CompiledAt: common.AsTime(row[2])}
	if by := common.AsInt64(row[3]); by > 0 {
		release.CompiledBy = &by
	}
	if err := json.Unmarshal([]byte(common.AsString(row[1])), &release.Resources); err != nil {
		return domain.EffectiveRelease{}, err
	}
	return release, nil
}
