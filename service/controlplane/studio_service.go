package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/isolation"
	"github.com/nauticana/scout/service/scope"
)

// StudioValidationError carries field-level validation failures.
type StudioValidationError struct {
	Fields []domain.AgentFieldError
}

func (e *StudioValidationError) Error() string { return domain.ErrValidation.Error() }
func (e *StudioValidationError) Unwrap() error { return domain.ErrValidation }

// StudioAuthorizationObject is the keel authorization object Studio actions are granted on.
const StudioAuthorizationObject = "AGENT_STUDIO"

// StudioService implements the shared authoring and release lifecycle.
type StudioService struct {
	DB keelport.DatabaseRepository
	// Currencies checks a cost budget's unit; defaults to keel's catalog over DB.
	Currencies scope.CurrencyCatalog
	// Principals and Roles authorize every call at the agent's scope; both are required.
	Principals contract.PrincipalResolver
	Roles      contract.PrincipalAuthorizer
	// MaxAgentsPerType caps the agents of one type a tenant may create; 0 refuses every create.
	MaxAgentsPerType int
	// Entropy generates new agent ids; nil uses crypto/rand.
	Entropy io.Reader
	// Delegations is optional; when set an agent named by an open delegation grant cannot be archived.
	Delegations contract.DelegationGrantRepository
	// Scopes reads the scope chain settings compile over and Mergers folds it; nil uses
	// scope.TableScopeRepository over DB and the platform mergers. MaxScopeDepth is
	// agent_max_scope_depth; zero takes scope.DefaultMaxScopeDepth.
	Scopes        contract.ScopeRepository
	Mergers       contract.ResourceMergerRegistry
	MaxScopeDepth int
	// Guards may refuse a publish or restore; Sections freeze named parts into each release.
	Guards     []contract.PublishGuard
	Sections   []contract.DefinitionSection
	Sources    contract.PromptSourceRepository
	Assembler  contract.PromptDraftAssembler
	Compiler   contract.PromptCompiler
	Validators []contract.AgentDraftValidator
	Tester     contract.AgentDraftTestExecutor
	Kinds      contract.AgentTypeCatalog
	Catalog    contract.StudioModelCatalog
	Activity   contract.AgentActivityReporter
	// Layout places an agent's prompt scopes; nil uses BasePromptScopeLayout. It must be the prompt repository's.
	Layout contract.PromptScopeLayout
	// ReleaseWriters run inside the publish and restore transaction, after the
	// version row exists: tool and skill bindings and the compiled execution graph
	// commit with the release or not at all.
	ReleaseWriters []ReleaseWriter
	// Pricer prices the selected models of a release with a cost budget, which publish,
	// restore, and activation refuse without it; controlplane.ModelCatalog implements it.
	Pricer contract.ModelPricer
	// Templates keeps agent templates; nil refuses a create from a template. TemplateLimits bound a
	// tenant's templates: agent_studio_max_templates_per_type and agent_studio_max_template_versions.
	Templates      contract.AgentTemplateStore
	TemplateLimits domain.TemplateLimits
	// ApprovalRoutingLimits tighten every approval_routing value a save, import, or adoption
	// writes; zero takes the structural bounds. StudioApprovalRouting.MaxRoutes bounds the agent.
	ApprovalRoutingLimits domain.ApprovalRoutingLimits
	// MaxActivationLead is how far ahead a publish may schedule its activation; zero takes
	// DefaultMaxActivationLead.
	MaxActivationLead time.Duration
	// MaxPortableBindings bounds the bindings of one portable document or template, MaxReferences
	// the reference mappings of one import or template create, MaxPortableExtension the bytes of
	// one portable draft extension, MaxTemplateAudience the tenants one template publication
	// names, and MaxCommentsShown the draft comments one list returns; zero takes the Default of each.
	MaxPortableBindings  int
	MaxReferences        int
	MaxPortableExtension int
	MaxTemplateAudience  int
	MaxCommentsShown     int
	Now                  func() time.Time
	// Audit records a setting change that removes or loosens an evaluation gate, which needs
	// OVERRIDE_GATE; such a change is refused without it.
	Audit contract.AuditSink

	once sync.Once
	qs   keelport.QueryService
}

// ReleaseWriter persists what an immutable definition implies beyond its own row.
type ReleaseWriter interface {
	WriteRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error
}

func (s *StudioService) writeRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	for _, writer := range s.ReleaseWriters {
		if err := writer.WriteRelease(ctx, tx, tenantID, definition); err != nil {
			return err
		}
	}
	if err := requireReleaseBindings(ctx, tx, tenantID, definition); err != nil {
		return err
	}
	return requirePinnedGuardrails(ctx, tx, tenantID, definition)
}

// requireReleaseBindings refuses a release whose tools, skills, or tool loop no composed writer
// bound, so it never publishes an agent whose every tool call would be denied.
func requireReleaseBindings(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	tools := map[string]struct{}{}
	for _, tool := range definition.Tools {
		tools[tool.ToolID] = struct{}{}
	}
	skills := map[string]struct{}{}
	for _, skill := range definition.Skills {
		skills[skill.SkillID] = struct{}{}
	}
	graphs := 0
	if definition.ToolLoop != nil {
		graphs = 1
	}
	for _, binding := range []struct {
		query, what string
		declared    int
	}{{qStudioReleaseTools, "tools", len(tools)}, {qStudioReleaseSkills, "skills", len(skills)}, {qStudioReleaseGraph, "a tool loop", graphs}} {
		if binding.declared == 0 {
			continue
		}
		bound, err := tx.Query(ctx, binding.query, tenantID, definition.AgentID, definition.Version)
		if err != nil {
			return fmt.Errorf("read release %s: %w", binding.what, err)
		}
		if len(bound.Rows) == 0 || common.AsInt64(bound.Rows[0][0]) < int64(binding.declared) {
			return fmt.Errorf("%w: release %s of %q declares %s that no release writer bound", domain.ErrNotReady, definition.Version, definition.AgentID, binding.what)
		}
	}
	return nil
}

// requirePinnedGuardrails refuses a release whose definition froze guardrails that no composed
// writer named on agent_version, so it never runs without the rule set it carries.
func requirePinnedGuardrails(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if _, frozen := definition.Sections[GuardrailSectionName]; !frozen {
		return nil
	}
	config, _, _, err := releaseGuardrails(definition)
	if err != nil {
		return err
	}
	pinned, err := tx.Query(ctx, qStudioReleaseGuardrail, tenantID, definition.AgentID, definition.Version)
	if err != nil {
		return fmt.Errorf("read release guardrail version: %w", err)
	}
	if len(pinned.Rows) == 0 || strings.TrimSpace(common.AsString(pinned.Rows[0][0])) != config.Version {
		return fmt.Errorf("%w: release %s of %q freezes guardrails but no release writer named their version", domain.ErrNotReady, definition.Version, definition.AgentID)
	}
	return nil
}

func (s *StudioService) activationLead() time.Duration {
	return orDefault(s.MaxActivationLead, DefaultMaxActivationLead)
}

func (s *StudioService) portableBindingLimit() int {
	return orDefault(s.MaxPortableBindings, DefaultMaxPortableBindings)
}

func (s *StudioService) referenceLimit() int { return orDefault(s.MaxReferences, DefaultMaxReferences) }

func (s *StudioService) extensionLimit() int {
	return orDefault(s.MaxPortableExtension, DefaultMaxPortableExtension)
}

func (s *StudioService) audienceLimit() int {
	return orDefault(s.MaxTemplateAudience, DefaultMaxTemplateAudience)
}

func (s *StudioService) commentLimit() int {
	return orDefault(s.MaxCommentsShown, DefaultMaxCommentsShown)
}

// orDefault is value when it is positive and fallback otherwise; negative limits are refused before use.
func orDefault[T ~int | ~int64](value, fallback T) T {
	if value > 0 {
		return value
	}
	return fallback
}

func (s *StudioService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *StudioService) init(ctx context.Context) error {
	if s.Sources == nil || s.Assembler == nil || s.Compiler == nil {
		return fmt.Errorf("studio service: prompt sources, assembler, and compiler are required")
	}
	if s.MaxActivationLead < 0 || s.MaxPortableBindings < 0 || s.MaxReferences < 0 || s.MaxPortableExtension < 0 ||
		s.MaxTemplateAudience < 0 || s.MaxCommentsShown < 0 {
		return fmt.Errorf("%w: studio service limits cannot be negative", domain.ErrValidation)
	}
	if s.qs != nil {
		return nil
	}
	if s.DB == nil {
		return fmt.Errorf("studio service: database is required")
	}
	s.once.Do(func() {
		s.qs = s.DB.GetQueryService(ctx, studioCreateQueries)
		s.Currencies = scope.Currencies(s.Currencies, s.DB)
	})
	return nil
}

// ListAgents returns the tenant agents the actor may view, with generic readiness and product labels.
func (s *StudioService) ListAgents(ctx context.Context, actor domain.StudioActor) ([]domain.AgentSummary, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	viewable, err := s.viewFilter(ctx, actor)
	if err != nil {
		return nil, err
	}
	return s.summaries(ctx, actor.TenantID, viewable)
}

func (s *StudioService) summaries(ctx context.Context, tenantID int64, keep func(agentID string) (bool, error)) ([]domain.AgentSummary, error) {
	res, err := s.qs.Query(ctx, qStudioListAgents, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list studio agents: %w", err)
	}
	descriptors := map[string]domain.AgentTypeDescriptor{}
	if s.Kinds != nil {
		items, listErr := s.Kinds.List(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list agent kinds: %w", listErr)
		}
		for _, item := range items {
			descriptors[item.AgentTypeID] = item
		}
	}
	lastRun, err := s.lastActivity(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	summaries := make([]domain.AgentSummary, 0, len(res.Rows))
	for _, row := range res.Rows {
		state := domain.AgentState(common.AsString(row[3]))
		summary := domain.AgentSummary{
			AgentID: common.AsString(row[0]), AgentTypeID: common.AsString(row[1]),
			DisplayName: common.AsString(row[2]), Active: state == domain.AgentStateActive,
			Archived: state == domain.AgentStateArchived, Enabled: common.AsBool(row[4]),
			DraftRevision: common.AsInt64(row[5]), PromptProfileRevision: common.AsInt64(row[6]),
			Default: common.AsBool(row[7]), PublishedVersion: common.AsString(row[8]),
		}
		if kept, keepErr := keep(summary.AgentID); keepErr != nil {
			return nil, keepErr
		} else if !kept {
			continue
		}
		if value, ok := row[9].(time.Time); ok {
			summary.PublishedAt = &value
		}
		if descriptor, ok := descriptors[summary.AgentTypeID]; ok {
			summary.Purpose = descriptor.Purpose
			if summary.DisplayName == "" {
				summary.DisplayName = descriptor.DisplayName
			}
		}
		if at, ok := lastRun[summary.AgentID]; ok {
			summary.LastRunAt = &at
		}
		summary.Readiness, summary.ReadinessReason = readiness(summary, row[10])
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// lastActivity merges Studio test runs with injected execution history; the
// newest of the two is what an operator means by "last run".
func (s *StudioService) lastActivity(ctx context.Context, tenantID int64) (map[string]time.Time, error) {
	res, err := s.qs.Query(ctx, qStudioLastTest, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load studio test activity: %w", err)
	}
	lastRun := make(map[string]time.Time, len(res.Rows))
	for _, row := range res.Rows {
		if at, ok := row[1].(time.Time); ok {
			lastRun[common.AsString(row[0])] = at
		}
	}
	if s.Activity == nil {
		return lastRun, nil
	}
	additional, err := s.Activity.LastRun(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load agent activity: %w", err)
	}
	for agentID, at := range additional {
		if current, seen := lastRun[agentID]; !seen || at.After(current) {
			lastRun[agentID] = at
		}
	}
	return lastRun, nil
}

func readiness(summary domain.AgentSummary, encodedDefinition any) (domain.AgentReadiness, string) {
	switch {
	case !summary.Active || !summary.Enabled:
		return domain.AgentDisabled, "agent is disabled"
	case summary.PublishedVersion == "":
		return domain.AgentUnpublished, "no published version is active"
	}
	definition, err := decodeDefinition(encodedDefinition)
	if err != nil {
		return domain.AgentError, "published definition is invalid"
	}
	if definition.Models.Text == nil {
		return domain.AgentMissingModel, "published definition has no text model"
	}
	return domain.AgentReady, ""
}

// GetDraft returns mutable configuration with resolved prompt provenance.
func (s *StudioService) GetDraft(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentDraft, error) {
	if err := s.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentDraft{}, err
	}
	return s.draft(ctx, actor.TenantID, agentID)
}

func (s *StudioService) draft(ctx context.Context, tenantID int64, agentID string) (domain.AgentDraft, error) {
	draft, err := s.getDraft(ctx, tenantID, agentID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	if draft.PromptScopes, err = s.promptScopes(ctx, tenantID, agentID, draft.AgentTypeID); err != nil {
		return domain.AgentDraft{}, err
	}
	languages, err := s.Sources.Languages(ctx, tenantID, agentID)
	if err != nil {
		return domain.AgentDraft{}, fmt.Errorf("list draft languages: %w", err)
	}
	for _, language := range languages {
		resolved, resolveErr := s.Sources.Resolve(ctx, tenantID, agentID, language)
		if resolveErr != nil {
			return domain.AgentDraft{}, fmt.Errorf("resolve draft language %q: %w", language, resolveErr)
		}
		assembled, assembleErr := s.Assembler.Assemble(resolved)
		if assembleErr != nil {
			return domain.AgentDraft{}, fmt.Errorf("assemble draft language %q: %w", language, assembleErr)
		}
		draft.Languages = append(draft.Languages, assembled)
	}
	draft.Drift, err = s.drift(ctx, tenantID, draft)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	return draft, nil
}

func (s *StudioService) getDraft(ctx context.Context, tenantID int64, agentID string) (domain.AgentDraft, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentDraft{}, err
	}
	res, err := s.qs.Query(ctx, qStudioGetDraft, tenantID, agentID)
	if err != nil {
		return domain.AgentDraft{}, fmt.Errorf("load studio draft: %w", err)
	}
	if len(res.Rows) == 0 {
		return domain.AgentDraft{}, domain.ErrNotFound
	}
	row := res.Rows[0]
	state := domain.AgentState(common.AsString(row[2]))
	return domain.AgentDraft{
		AgentID: agentID, AgentTypeID: common.AsString(row[0]), DisplayName: common.AsString(row[1]),
		Active: state == domain.AgentStateActive, Archived: state == domain.AgentStateArchived, Enabled: common.AsBool(row[3]),
		Models: domain.AgentModelSelection{
			Text: dbModelReference(row[4], row[5]), Image: dbModelReference(row[6], row[7]), Video: dbModelReference(row[8], row[9]),
		},
		Extension: json.RawMessage(common.AsString(row[10])), ExpectedDraftRevision: common.AsInt64(row[11]),
		ExpectedPromptProfileRevision: common.AsInt64(row[12]), Default: common.AsBool(row[13]),
	}, nil
}

// SaveDraft atomically replaces agent overrides and changed tenant defaults.
func (s *StudioService) SaveDraft(ctx context.Context, actor domain.StudioActor, draft domain.AgentDraft) (domain.AgentDraft, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentDraft{}, err
	}
	current, err := s.editable(ctx, actor, domain.StudioEdit, draft.AgentID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	draft.AgentTypeID = current.AgentTypeID
	draft.Default = current.Default
	draft.Models, err = s.resolveModels(ctx, actor.TenantID, draft.Models)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	if err = s.validateDraft(ctx, actor.TenantID, draft, false); err != nil {
		return domain.AgentDraft{}, err
	}
	scopes, err := s.promptScopes(ctx, actor.TenantID, draft.AgentID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	if err = s.checkSealedLayers(ctx, actor.TenantID, draft, scopes); err != nil {
		return domain.AgentDraft{}, err
	}

	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return domain.AgentDraft{}, fmt.Errorf("begin draft transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Query(ctx, qStudioUpdateProfile, draft.DisplayName, agentState(draft.Active), actor.TenantID, draft.AgentID); err != nil {
		return domain.AgentDraft{}, fmt.Errorf("update agent profile: %w", err)
	}
	updated, err := tx.Query(ctx, qStudioUpdateDraft,
		draft.Enabled,
		modelProvider(draft.Models.Text), modelID(draft.Models.Text), modelProvider(draft.Models.Image), modelID(draft.Models.Image),
		modelProvider(draft.Models.Video), modelID(draft.Models.Video), nullableJSON(draft.Extension), actorUser(actor),
		actor.TenantID, draft.AgentID, draft.ExpectedDraftRevision)
	if err != nil {
		return domain.AgentDraft{}, fmt.Errorf("update agent draft: %w", err)
	}
	if len(updated.Rows) == 0 {
		return domain.AgentDraft{}, domain.ErrRevisionConflict
	}
	persisted, err := persistedPromptBindings(ctx, tx, actor.TenantID, scopes.AgentScopeID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	if err = syncPromptBindings(ctx, tx, actor, scopes.AgentScopeID, desiredPromptBindings(draft, scopes.AgentScopeID), persisted); err != nil {
		return domain.AgentDraft{}, err
	}
	changedDefaults, err := s.saveDefaults(ctx, tx, actor, draft, scopes.TypeScopeID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	detail := "agent"
	if changedDefaults {
		detail = "agent and tenant defaults"
	}
	if err = auditStudio(ctx, tx, actor, draft.AgentID, "DRAFT_SAVE", detail); err != nil {
		return domain.AgentDraft{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentDraft{}, fmt.Errorf("commit draft: %w", err)
	}
	committed = true
	return s.draft(ctx, actor.TenantID, draft.AgentID)
}

// SetEnabled changes the kill switch without invoking draft validators.
func (s *StudioService) SetEnabled(ctx context.Context, actor domain.StudioActor, request domain.AgentSetEnabledRequest) (domain.AgentEnabledState, error) {
	if _, err := s.editable(ctx, actor, domain.StudioEdit, request.AgentID); err != nil {
		return domain.AgentEnabledState{}, err
	}
	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return domain.AgentEnabledState{}, fmt.Errorf("begin enabled transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	res, err := tx.Query(ctx, qStudioSetDraftEnabled, request.Enabled, actorUser(actor), actor.TenantID, request.AgentID, request.ExpectedDraftRevision)
	if err != nil {
		return domain.AgentEnabledState{}, fmt.Errorf("set draft enabled: %w", err)
	}
	if len(res.Rows) == 0 {
		return domain.AgentEnabledState{}, domain.ErrRevisionConflict
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "kill_switch"
	}
	if len(reason) > 400 {
		return domain.AgentEnabledState{}, validationError("reason", "must not exceed 400 characters")
	}
	if _, err = tx.Query(ctx, qStudioSetProfileActive, agentState(request.Enabled), reason,
		actorRef(actor), actor.TenantID, request.AgentID); err != nil {
		return domain.AgentEnabledState{}, fmt.Errorf("set profile state: %w", err)
	}
	event := "ENABLE"
	if !request.Enabled {
		event = "DISABLE"
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, event, reason); err != nil {
		return domain.AgentEnabledState{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentEnabledState{}, fmt.Errorf("commit enabled state: %w", err)
	}
	committed = true
	return domain.AgentEnabledState{Enabled: request.Enabled, DraftRevision: common.AsInt64(res.Rows[0][0])}, nil
}

// TestDraft executes the current saved draft through the injected governed tester.
func (s *StudioService) TestDraft(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest) (domain.AgentTestResult, error) {
	if s.Tester == nil {
		return domain.AgentTestResult{}, domain.ErrNotReady
	}
	if err := s.Authorize(ctx, actor, domain.StudioTest, request.AgentID); err != nil {
		return domain.AgentTestResult{}, err
	}
	draft, err := s.draft(ctx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	definition, settings, err := s.frozenDraft(ctx, actor.TenantID, draft, nil)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	if request.Limits, err = isolation.FrozenExecutionLimits(settings.Resources); err != nil {
		return domain.AgentTestResult{}, err
	}
	result, err := s.Tester.Execute(ctx, actor, request, definition)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	// The lifecycle audit is Studio's, not the product tester's.
	if _, auditErr := s.qs.Query(ctx, qStudioAudit, actor.TenantID, request.AgentID, "TEST",
		nullableString("language "+result.LanguageCode), string(actor.Principal.Kind), actor.Principal.ID); auditErr != nil {
		return domain.AgentTestResult{}, fmt.Errorf("record studio test audit: %w", auditErr)
	}
	return result, nil
}

// Publish freezes the saved draft and deploys its new release.
func (s *StudioService) Publish(ctx context.Context, actor domain.StudioActor, request domain.AgentPublishRequest) (domain.AgentRelease, error) {
	if _, err := s.editable(ctx, actor, domain.StudioPublish, request.AgentID); err != nil {
		return domain.AgentRelease{}, err
	}
	draft, err := s.draft(ctx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	definition, settings, err := s.frozenDraft(ctx, actor.TenantID, draft, actorUserID(actor))
	if err != nil {
		return domain.AgentRelease{}, err
	}
	definition.ChangeSummary = request.ChangeSummary
	definition.DraftRevision = request.ExpectedDraftRevision
	definition.PromptProfileRevision = request.ExpectedPromptProfileRevision
	definition.PublishedBy = actorUserID(actor)
	if err = s.guard(ctx, domain.PublishCandidate{Actor: actor, Definition: definition, Settings: settings}); err != nil {
		return domain.AgentRelease{}, err
	}

	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("begin publish transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	lock, err := tx.Query(ctx, qStudioLockDraft, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("lock publish draft: %w", err)
	}
	if len(lock.Rows) == 0 {
		return domain.AgentRelease{}, domain.ErrNotFound
	}
	if common.AsInt64(lock.Rows[0][0]) != request.ExpectedDraftRevision {
		return domain.AgentRelease{}, domain.ErrRevisionConflict
	}
	alias, err := tx.Query(ctx, qStudioLockAlias, actor.TenantID, draft.AgentTypeID)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("lock publish alias: %w", err)
	}
	aliasRevision := int64(0)
	if len(alias.Rows) > 0 {
		aliasRevision = common.AsInt64(alias.Rows[0][1])
	}
	if aliasRevision != request.ExpectedPromptProfileRevision {
		return domain.AgentRelease{}, domain.ErrRevisionConflict
	}
	if err = s.checkGateInTx(ctx, tx, domain.PublishCandidate{Actor: actor, Definition: definition, Settings: settings}); err != nil {
		return domain.AgentRelease{}, err
	}
	version, err := nextStudioVersion(ctx, tx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	definition.Version = version
	definition.PublishedAt = s.now()
	encoded, err := json.Marshal(definition)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("marshal agent definition: %w", err)
	}
	if _, err = tx.Query(ctx, qStudioInsertVersion, actor.TenantID, request.AgentID, version, string(encoded),
		definition.DefinitionDigest, definition.DraftRevision, definition.PromptProfileRevision,
		nullableString(definition.ChangeSummary), actorUser(actor), nil); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("insert agent version: %w", err)
	}
	if err = storeSettings(ctx, tx, settings, version); err != nil {
		return domain.AgentRelease{}, err
	}
	if err = s.writeRelease(ctx, tx, actor.TenantID, definition); err != nil {
		return domain.AgentRelease{}, err
	}
	for _, reference := range selectedModels(definition.Models) {
		if _, err = tx.Query(ctx, qStudioGrantModel, actor.TenantID, reference.ProviderID, reference.ModelID); err != nil {
			return domain.AgentRelease{}, fmt.Errorf("grant model %s/%s: %w", reference.ProviderID, reference.ModelID, err)
		}
	}
	active, detail, activation, err := s.deployOrSchedule(ctx, tx, actor, definition, request)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, "PUBLISH", "version "+version+detail); err != nil {
		return domain.AgentRelease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("commit publish: %w", err)
	}
	committed = true
	release := releaseFromDefinition(definition, active)
	release.Activation = activation
	return release, nil
}

// Restore copies an immutable release into a new version.
func (s *StudioService) Restore(ctx context.Context, actor domain.StudioActor, request domain.AgentRestoreRequest) (domain.AgentRelease, error) {
	if _, err := s.editable(ctx, actor, domain.StudioRestore, request.AgentID); err != nil {
		return domain.AgentRelease{}, err
	}
	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("begin restore transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	lock, err := tx.Query(ctx, qStudioLockDraft, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("lock restore draft: %w", err)
	}
	if len(lock.Rows) == 0 {
		return domain.AgentRelease{}, domain.ErrNotFound
	}
	source, err := tx.Query(ctx, qStudioGetVersion, actor.TenantID, request.AgentID, request.Version)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("load restore version: %w", err)
	}
	if len(source.Rows) == 0 {
		return domain.AgentRelease{}, domain.ErrNotFound
	}
	definition, err := decodeDefinition(source.Rows[0][0])
	if err != nil {
		return domain.AgentRelease{}, err
	}
	restored := domain.PublishCandidate{Actor: actor, Definition: definition, RestoredFrom: request.Version}
	if err = s.guard(ctx, restored); err != nil {
		return domain.AgentRelease{}, err
	}
	if err = s.checkGateInTx(ctx, tx, restored); err != nil {
		return domain.AgentRelease{}, err
	}
	settings, err := loadRestoredSettings(ctx, tx, actor.TenantID, request.AgentID, request.Version)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	if err = s.checkBudget(ctx, settings.Resources, definition); err != nil {
		return domain.AgentRelease{}, err
	}
	version, err := nextStudioVersion(ctx, tx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	definition.Version = version
	definition.RestoredFromVersion = request.Version
	definition.ChangeSummary = "Restored from version " + request.Version
	definition.PublishedBy = actorUserID(actor)
	definition.PublishedAt = s.now()
	encoded, err := json.Marshal(definition)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("marshal restored definition: %w", err)
	}
	if _, err = tx.Query(ctx, qStudioInsertVersion, actor.TenantID, request.AgentID, version, string(encoded),
		definition.DefinitionDigest, definition.DraftRevision, definition.PromptProfileRevision,
		definition.ChangeSummary, actorUser(actor), request.Version); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("insert restored version: %w", err)
	}
	if err = storeSettings(ctx, tx, settings, version); err != nil {
		return domain.AgentRelease{}, err
	}
	if err = s.writeRelease(ctx, tx, actor.TenantID, definition); err != nil {
		return domain.AgentRelease{}, err
	}
	active, _, err := deploy(ctx, tx, actor.TenantID, request.AgentID, version, 0)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, "RESTORE", definition.ChangeSummary); err != nil {
		return domain.AgentRelease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("commit restore: %w", err)
	}
	committed = true
	return releaseFromDefinition(definition, active), nil
}

// Reset removes selected editable prompt levels with revision checks.
func (s *StudioService) Reset(ctx context.Context, actor domain.StudioActor, request domain.AgentResetRequest) (domain.AgentDraft, error) {
	current, err := s.editable(ctx, actor, domain.StudioEdit, request.AgentID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	resetAgent := request.Scope == domain.ResetAgentOverride || request.Scope == domain.ResetToBaseline
	resetTenant := request.Scope == domain.ResetTenantDefault || request.Scope == domain.ResetToBaseline
	if !resetAgent && !resetTenant {
		return domain.AgentDraft{}, validationError("scope", "must be agent_override, type_default, or platform_baseline")
	}
	scopes, err := s.promptScopes(ctx, actor.TenantID, request.AgentID, current.AgentTypeID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return domain.AgentDraft{}, fmt.Errorf("begin reset transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if resetAgent {
		res, bumpErr := tx.Query(ctx, qStudioBumpDraft, actorUser(actor), actor.TenantID, request.AgentID, request.ExpectedDraftRevision)
		if bumpErr != nil {
			return domain.AgentDraft{}, fmt.Errorf("bump draft revision: %w", bumpErr)
		}
		if len(res.Rows) == 0 {
			return domain.AgentDraft{}, domain.ErrRevisionConflict
		}
		if err = resetPromptBindings(ctx, tx, actor.TenantID, scopes.AgentScopeID, request); err != nil {
			return domain.AgentDraft{}, err
		}
	}
	if resetTenant {
		res, bumpErr := tx.Query(ctx, qStudioBumpAlias, actorUser(actor), actor.TenantID, current.AgentTypeID, request.ExpectedPromptProfileRevision)
		if bumpErr != nil {
			return domain.AgentDraft{}, fmt.Errorf("bump prompt profile revision: %w", bumpErr)
		}
		if len(res.Rows) == 0 {
			return domain.AgentDraft{}, domain.ErrRevisionConflict
		}
		if err = resetPromptBindings(ctx, tx, actor.TenantID, scopes.TypeScopeID, request); err != nil {
			return domain.AgentDraft{}, err
		}
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, "RESET", "scope "+string(request.Scope)); err != nil {
		return domain.AgentDraft{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentDraft{}, fmt.Errorf("commit reset: %w", err)
	}
	committed = true
	return s.draft(ctx, actor.TenantID, request.AgentID)
}

// SetDefault moves the logical kind alias with optimistic revision checking.
func (s *StudioService) SetDefault(ctx context.Context, actor domain.StudioActor, request domain.AgentSetDefaultRequest) ([]domain.AgentSummary, error) {
	current, err := s.editable(ctx, actor, domain.StudioPublish, request.AgentID)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return nil, fmt.Errorf("begin alias transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	res, err := tx.Query(ctx, qStudioSetAlias, request.AgentID, actorUser(actor), actor.TenantID, current.AgentTypeID, request.ExpectedAliasRevision)
	if err != nil {
		return nil, fmt.Errorf("set agent alias: %w", err)
	}
	if len(res.Rows) == 0 {
		return nil, domain.ErrRevisionConflict
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, "SET_DEFAULT", ""); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit alias: %w", err)
	}
	committed = true
	return s.ListAgents(ctx, actor)
}

// History returns immutable releases newest first.
func (s *StudioService) History(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.AgentRelease, error) {
	if err := s.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	if _, err := s.getDraft(ctx, actor.TenantID, agentID); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qStudioHistory, actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list agent history: %w", err)
	}
	releases := make([]domain.AgentRelease, 0, len(res.Rows))
	for _, row := range res.Rows {
		definition, decodeErr := decodeDefinition(row[0])
		if decodeErr != nil {
			return nil, decodeErr
		}
		releases = append(releases, releaseFromDefinition(definition, common.AsBool(row[1])))
	}
	return releases, nil
}

// AuditLog returns queryable lifecycle events newest first.
func (s *StudioService) AuditLog(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.AgentStudioEvent, error) {
	if err := s.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	if _, err := s.getDraft(ctx, actor.TenantID, agentID); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qStudioAuditLog, actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list studio audit: %w", err)
	}
	events := make([]domain.AgentStudioEvent, 0, len(res.Rows))
	for _, row := range res.Rows {
		event := domain.AgentStudioEvent{Event: common.AsString(row[0]), Detail: common.AsString(row[1])}
		if row[2] != nil {
			event.Actor = domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[2])), ID: common.AsString(row[3])}
		}
		if value, ok := row[4].(time.Time); ok {
			event.OccurredAt = value
		}
		events = append(events, event)
	}
	return events, nil
}

// ReleaseSections returns compiled sections from one immutable definition.
func (s *StudioService) ReleaseSections(ctx context.Context, actor domain.StudioActor, agentID, version string) ([]domain.AgentReleaseSection, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	if err := s.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qStudioGetVersion, actor.TenantID, agentID, version)
	if err != nil {
		return nil, fmt.Errorf("load release sections: %w", err)
	}
	if len(res.Rows) == 0 {
		return nil, domain.ErrNotFound
	}
	definition, err := decodeDefinition(res.Rows[0][0])
	if err != nil {
		return nil, err
	}
	var sections []domain.AgentReleaseSection
	for _, language := range definition.Languages {
		for _, section := range language.Sections {
			sections = append(sections, domain.AgentReleaseSection{LanguageCode: language.LanguageCode, CompiledPromptSection: section})
		}
	}
	return sections, nil
}

// Models returns the injected tenant model catalog.
func (s *StudioService) Models(ctx context.Context, actor domain.StudioActor) ([]domain.StudioModel, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	if s.Catalog == nil {
		return nil, fmt.Errorf("studio service: model catalog is required")
	}
	return s.Catalog.List(ctx, actor.TenantID)
}

func (s *StudioService) definition(ctx context.Context, tenantID int64, draft domain.AgentDraft) (domain.AgentDefinition, error) {
	definition := domain.AgentDefinition{
		AgentID: draft.AgentID, AgentTypeID: draft.AgentTypeID, Enabled: draft.Enabled, Models: draft.Models,
		Extension:     cloneJSON(draft.Extension),
		DraftRevision: draft.ExpectedDraftRevision, PromptProfileRevision: draft.ExpectedPromptProfileRevision,
	}
	for _, language := range draft.Languages {
		resolved, err := s.Sources.Resolve(ctx, tenantID, draft.AgentID, language.LanguageCode)
		if err != nil {
			return domain.AgentDefinition{}, fmt.Errorf("resolve publish language %q: %w", language.LanguageCode, err)
		}
		compiled, err := s.Compiler.Compile(language.LanguageCode, resolved.Sections)
		if err != nil {
			return domain.AgentDefinition{}, fmt.Errorf("compile publish language %q: %w", language.LanguageCode, err)
		}
		definition.Languages = append(definition.Languages, compiled)
	}
	if len(definition.Languages) == 0 {
		return domain.AgentDefinition{}, domain.ErrNoPrompts
	}
	digest, err := s.Compiler.DefinitionDigest(definition)
	if err != nil {
		return domain.AgentDefinition{}, err
	}
	definition.DefinitionDigest = digest
	return definition, nil
}

func (s *StudioService) guard(ctx context.Context, candidate domain.PublishCandidate) error {
	for _, guard := range s.Guards {
		if err := guard.CheckPublish(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (s *StudioService) drift(ctx context.Context, tenantID int64, draft domain.AgentDraft) (*domain.AgentDrift, error) {
	res, err := s.qs.Query(ctx, qStudioActiveDefinition, tenantID, draft.AgentID)
	if err != nil {
		return nil, fmt.Errorf("load active definition: %w", err)
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	active, err := decodeDefinition(res.Rows[0][1])
	if err != nil {
		return nil, err
	}
	current, err := s.definition(ctx, tenantID, draft)
	if err != nil && !errors.Is(err, domain.ErrNoPrompts) {
		return nil, err
	}
	report := &domain.AgentDrift{ActiveVersion: common.AsString(res.Rows[0][0])}
	if active.Enabled != draft.Enabled {
		report.Causes = append(report.Causes, "enabled")
	}
	if !modelSelectionsEqual(active.Models, draft.Models) {
		report.Causes = append(report.Causes, "models")
	}
	if !bytes.Equal(compactStudioJSON(active.Extension), compactStudioJSON(draft.Extension)) {
		report.Causes = append(report.Causes, "extension")
	}
	origin, err := s.draftOrigin(ctx, tenantID, draft.AgentID)
	if err != nil {
		return nil, err
	}
	if !derivedFromEqual(origin.derivedFrom(), active.DerivedFrom) {
		report.Causes = append(report.Causes, "derived_from")
	}
	settings, err := s.compileSettingsOf(ctx, tenantID, draft)
	if err != nil && !settingsProblem(err) {
		return nil, err
	}
	if err != nil || scope.SettingsDigest(settings.Resources) != active.SettingsDigest {
		report.Causes = append(report.Causes, "settings")
	}
	sections, err := s.sections(ctx, tenantID, draft)
	if err != nil {
		return nil, err
	}
	for _, name := range sectionNames(sections, active.Sections) {
		if !bytes.Equal(sections[name], active.Sections[name]) {
			report.Causes = append(report.Causes, "section:"+name)
		}
	}
	activeDigests := languageDigests(active.Languages)
	for language, digest := range languageDigests(current.Languages) {
		if activeDigests[language] != digest {
			report.ChangedLanguages = append(report.ChangedLanguages, language)
		}
		delete(activeDigests, language)
	}
	for language := range activeDigests {
		report.ChangedLanguages = append(report.ChangedLanguages, language)
	}
	sort.Strings(report.ChangedLanguages)
	if len(report.Causes) == 0 && len(report.ChangedLanguages) == 0 {
		return nil, nil
	}
	return report, nil
}

func (s *StudioService) validateDraft(ctx context.Context, tenantID int64, draft domain.AgentDraft, publishing bool) error {
	var fields []domain.AgentFieldError
	if strings.TrimSpace(draft.AgentID) == "" {
		fields = append(fields, domain.AgentFieldError{Field: "agent_id", Message: "required"})
	}
	if strings.TrimSpace(draft.DisplayName) == "" || len(draft.DisplayName) > 200 {
		fields = append(fields, domain.AgentFieldError{Field: "display_name", Message: "must contain 1 to 200 characters"})
	}
	if draft.ExpectedDraftRevision <= 0 {
		fields = append(fields, domain.AgentFieldError{Field: "expected_agent_revision", Message: "must be positive"})
	}
	if draft.ExpectedPromptProfileRevision < 0 {
		fields = append(fields, domain.AgentFieldError{Field: "expected_prompt_profile_revision", Message: "must not be negative"})
	}
	if publishing || draft.Enabled {
		if draft.Models.Text == nil {
			fields = append(fields, domain.AgentFieldError{Field: "models.text", Message: "required"})
		}
		if len(draft.Languages) == 0 {
			fields = append(fields, domain.AgentFieldError{Field: "languages", Message: "at least one language is required"})
		}
	}
	fields = append(fields, validatePromptDraft(draft)...)
	if publishing || draft.Enabled || hasModels(draft.Models) {
		if s.Catalog == nil {
			return fmt.Errorf("studio service: model catalog is required")
		}
		catalogFields, err := s.Catalog.Validate(ctx, tenantID, draft.Models)
		if err != nil {
			return fmt.Errorf("validate model selection: %w", err)
		}
		fields = append(fields, catalogFields...)
	}
	phase := domain.ValidateDraft
	if publishing {
		phase = domain.ValidateRelease
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return s.runValidators(ctx, tenantID, draft, phase)
}

func (s *StudioService) runValidators(ctx context.Context, tenantID int64, draft domain.AgentDraft, phase domain.ValidationPhase) error {
	var fields []domain.AgentFieldError
	for _, validator := range s.Validators {
		validatorFields, err := validator.Validate(ctx, tenantID, draft, phase)
		if err != nil {
			return fmt.Errorf("validate agent draft: %w", err)
		}
		fields = append(fields, validatorFields...)
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

func (s *StudioService) resolveModels(ctx context.Context, tenantID int64, selection domain.AgentModelSelection) (domain.AgentModelSelection, error) {
	if !modelsNeedResolution(selection) {
		return selection, nil
	}
	if s.Catalog == nil {
		return domain.AgentModelSelection{}, validationError("models", "provider is required for every model")
	}
	models, err := s.Catalog.List(ctx, tenantID)
	if err != nil {
		return domain.AgentModelSelection{}, fmt.Errorf("resolve model selection: %w", err)
	}
	fields := []domain.AgentFieldError{}
	selection.Text = resolveModelReference(selection.Text, "models.text", models, &fields)
	selection.Image = resolveModelReference(selection.Image, "models.image", models, &fields)
	selection.Video = resolveModelReference(selection.Video, "models.video", models, &fields)
	if len(fields) > 0 {
		return domain.AgentModelSelection{}, &StudioValidationError{Fields: fields}
	}
	return selection, nil
}

func modelsNeedResolution(selection domain.AgentModelSelection) bool {
	for _, reference := range []*domain.ModelReference{selection.Text, selection.Image, selection.Video} {
		if reference != nil && reference.ModelID != "" && reference.ProviderID == "" {
			return true
		}
	}
	return false
}

func resolveModelReference(reference *domain.ModelReference, field string, models []domain.StudioModel, fields *[]domain.AgentFieldError) *domain.ModelReference {
	if reference == nil || reference.ModelID == "" || reference.ProviderID != "" {
		return reference
	}
	var matches []domain.ModelReference
	for _, model := range models {
		if model.Reference.ModelID == reference.ModelID {
			matches = append(matches, model.Reference)
		}
	}
	if len(matches) != 1 {
		*fields = append(*fields, domain.AgentFieldError{Field: field, Message: "model id must resolve to exactly one provider"})
		return reference
	}
	resolved := matches[0]
	return &resolved
}

// saveDefaults syncs the type scope's bindings behind the prompt profile revision. An unchanged
// set bumps nothing, so an agent-only save never collides with a concurrent type-level edit.
func (s *StudioService) saveDefaults(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, draft domain.AgentDraft, typeScopeID string) (bool, error) {
	desired := desiredPromptBindings(draft, typeScopeID)
	alias, err := tx.Query(ctx, qStudioLockAlias, actor.TenantID, draft.AgentTypeID)
	if err != nil {
		return false, fmt.Errorf("lock prompt profile: %w", err)
	}
	if len(alias.Rows) == 0 {
		if len(desired) > 0 {
			return false, domain.ErrConflict
		}
		return false, nil
	}
	persisted, err := persistedPromptBindings(ctx, tx, actor.TenantID, typeScopeID)
	if err != nil {
		return false, err
	}
	if promptBindingsEqual(desired, persisted) {
		return false, nil
	}
	if common.AsInt64(alias.Rows[0][1]) != draft.ExpectedPromptProfileRevision {
		return false, domain.ErrRevisionConflict
	}
	bumped, err := tx.Query(ctx, qStudioBumpAlias, actorUser(actor), actor.TenantID, draft.AgentTypeID, draft.ExpectedPromptProfileRevision)
	if err != nil {
		return false, fmt.Errorf("bump prompt profile: %w", err)
	}
	if len(bumped.Rows) == 0 {
		return false, domain.ErrRevisionConflict
	}
	return true, syncPromptBindings(ctx, tx, actor, typeScopeID, desired, persisted)
}

func validatePromptDraft(draft domain.AgentDraft) []domain.AgentFieldError {
	var fields []domain.AgentFieldError
	languages := map[string]struct{}{}
	for _, language := range draft.Languages {
		code := strings.TrimSpace(language.LanguageCode)
		if code == "" {
			fields = append(fields, domain.AgentFieldError{Field: "languages.language_code", Message: "required"})
		}
		if _, exists := languages[code]; exists {
			fields = append(fields, domain.AgentFieldError{Field: "languages.language_code", Message: "duplicate"})
		}
		languages[code] = struct{}{}
		sections := map[int64]struct{}{}
		for _, section := range language.Sections {
			if section.PromptSectionID <= 0 {
				fields = append(fields, domain.AgentFieldError{Field: "prompt_section_id", Message: "must be positive"})
			}
			if _, exists := sections[section.PromptSectionID]; exists {
				fields = append(fields, domain.AgentFieldError{Field: "prompt_section_id", Message: "duplicate in language"})
			}
			sections[section.PromptSectionID] = struct{}{}
			for _, layer := range section.Layers {
				if len(layer.Instruction) > 16000 || len(layer.Output) > 16000 {
					fields = append(fields, domain.AgentFieldError{Field: "prompt", Message: "instruction and output must not exceed 16000 characters"})
				}
				if layer.MergeMode != "" && layer.MergeMode != domain.MergeAppend && layer.MergeMode != domain.MergeReplace {
					fields = append(fields, domain.AgentFieldError{Field: "merge_mode", Message: "must be " + string(domain.MergeAppend) + " or " + string(domain.MergeReplace)})
				}
			}
		}
	}
	return fields
}

func auditStudio(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, agentID, event, detail string) error {
	if _, err := tx.Query(ctx, qStudioAudit, actor.TenantID, agentID, event, nullableString(detail), string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return fmt.Errorf("record studio audit: %w", err)
	}
	return nil
}

// deploy makes a new release stable, or with a canary percentage gives it that share of
// new conversations beside the current stable one. It reports whether the release is stable.
func deploy(ctx context.Context, tx keelport.TxQueryService, tenantID int64, agentID, version string, canary int) (bool, string, error) {
	if canary < 0 || canary > 100 {
		return false, "", validationError("canary_percentage", "must be between 0 and 100")
	}
	if canary == 0 || canary == 100 {
		if _, err := tx.Query(ctx, qStudioDeployVersion, tenantID, agentID, version); err != nil {
			return false, "", fmt.Errorf("deploy agent version: %w", err)
		}
		return true, "", nil
	}
	updated, err := tx.Query(ctx, qStudioCanaryVersion, version, canary, tenantID, agentID)
	if err != nil {
		return false, "", fmt.Errorf("deploy canary version: %w", err)
	}
	if len(updated.Rows) == 0 {
		return false, "", validationError("canary_percentage", "the first release of an agent deploys as stable")
	}
	return false, fmt.Sprintf(" as a %d%% canary", canary), nil
}

func nextStudioVersion(ctx context.Context, tx keelport.TxQueryService, tenantID int64, agentID string) (string, error) {
	res, err := tx.Query(ctx, qStudioNextVersion, tenantID, agentID)
	if err != nil {
		return "", fmt.Errorf("allocate agent version: %w", err)
	}
	if len(res.Rows) == 0 {
		return "", fmt.Errorf("allocate agent version: empty result")
	}
	return strconv.FormatInt(common.AsInt64(res.Rows[0][0]), 10), nil
}

func releaseFromDefinition(definition domain.AgentDefinition, active bool) domain.AgentRelease {
	languages := make([]string, 0, len(definition.Languages))
	for _, language := range definition.Languages {
		languages = append(languages, language.LanguageCode)
	}
	return domain.AgentRelease{
		AgentID: definition.AgentID, AgentTypeID: definition.AgentTypeID, Version: definition.Version,
		Enabled: definition.Enabled, Models: definition.Models,
		DefinitionDigest: definition.DefinitionDigest, DraftRevision: definition.DraftRevision,
		PromptProfileRevision: definition.PromptProfileRevision, ChangeSummary: definition.ChangeSummary,
		PublishedBy: definition.PublishedBy, PublishedAt: definition.PublishedAt,
		RestoredFromVersion: definition.RestoredFromVersion, Active: active, Languages: languages,
	}
}

func decodeDefinition(value any) (domain.AgentDefinition, error) {
	var definition domain.AgentDefinition
	if err := json.Unmarshal([]byte(common.AsString(value)), &definition); err != nil {
		return domain.AgentDefinition{}, fmt.Errorf("decode agent definition: %w", err)
	}
	return definition, nil
}

func dbModelReference(provider, model any) *domain.ModelReference {
	modelID := strings.TrimSpace(common.AsString(model))
	if modelID == "" {
		return nil
	}
	return &domain.ModelReference{ProviderID: common.AsString(provider), ModelID: modelID}
}

func modelProvider(reference *domain.ModelReference) any {
	if reference == nil || reference.ProviderID == "" {
		return nil
	}
	return reference.ProviderID
}

func modelID(reference *domain.ModelReference) any {
	if reference == nil || reference.ModelID == "" {
		return nil
	}
	return reference.ModelID
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableJSON(value json.RawMessage) any {
	if len(bytes.TrimSpace(value)) == 0 {
		return nil
	}
	return string(value)
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func compactStudioJSON(value json.RawMessage) []byte {
	if len(bytes.TrimSpace(value)) == 0 {
		return nil
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return value
	}
	encoded, _ := json.Marshal(decoded)
	return encoded
}

func validateActor(actor domain.StudioActor) error {
	switch {
	case actor.TenantID <= 0 || strings.TrimSpace(actor.Principal.ID) == "":
		return fmt.Errorf("%w: tenant and actor are required", domain.ErrValidation)
	case actor.Principal.Kind == domain.PrincipalService:
		return nil
	case actor.Principal.Kind == domain.PrincipalHuman && actorUserID(actor) != nil:
		return nil
	}
	return fmt.Errorf("%w: a studio actor is a person with a user account id or a service", domain.ErrValidation)
}

// actorUserID projects a human actor onto the user_account columns; a service has none.
func actorUserID(actor domain.StudioActor) *int64 {
	if actor.Principal.Kind != domain.PrincipalHuman {
		return nil
	}
	id, err := strconv.ParseInt(actor.Principal.ID, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

func actorUser(actor domain.StudioActor) any {
	if id := actorUserID(actor); id != nil {
		return *id
	}
	return nil
}

func actorRef(actor domain.StudioActor) string {
	return string(actor.Principal.Kind) + ":" + actor.Principal.ID
}

func validationError(field, message string) error {
	return &StudioValidationError{Fields: []domain.AgentFieldError{{Field: field, Message: message}}}
}

func hasModels(selection domain.AgentModelSelection) bool {
	return selection.Text != nil || selection.Image != nil || selection.Video != nil
}

func modelSelectionsEqual(left, right domain.AgentModelSelection) bool {
	return modelReferenceEqual(left.Text, right.Text) && modelReferenceEqual(left.Image, right.Image) && modelReferenceEqual(left.Video, right.Video)
}

func modelReferenceEqual(left, right *domain.ModelReference) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func languageDigests(languages []domain.CompiledPrompt) map[string]string {
	digests := make(map[string]string, len(languages))
	for _, language := range languages {
		digests[language.LanguageCode] = language.Digest
	}
	return digests
}

var _ contract.AgentStudioHTTPBackend = (*StudioService)(nil)

// agentState maps the studio-v1 enabled flag onto the agent lifecycle. Studio
// only ever toggles between active and suspended; draining and retirement are
// operational transitions, not authoring ones.
func agentState(enabled bool) string {
	if enabled {
		return string(domain.AgentStateActive)
	}
	return string(domain.AgentStateSuspended)
}
