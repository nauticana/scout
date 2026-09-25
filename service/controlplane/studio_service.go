package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioValidationError carries field-level validation failures.
type StudioValidationError struct {
	Fields []domain.AgentFieldError
}

func (e *StudioValidationError) Error() string { return domain.ErrValidation.Error() }
func (e *StudioValidationError) Unwrap() error { return domain.ErrValidation }

// StudioService implements the shared authoring and release lifecycle.
type StudioService struct {
	DB         keelport.DatabaseRepository
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
	Now            func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

// ReleaseWriter persists what an immutable definition implies beyond its own row.
type ReleaseWriter interface {
	WriteRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error
}

func (s *StudioService) writeRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if (len(definition.Tools) > 0 || definition.ToolLoop != nil || len(definition.Skills) > 0) && len(s.ReleaseWriters) == 0 {
		return fmt.Errorf("%w: definition %q declares tools, a tool loop, or skills but no release writer is composed", domain.ErrNotReady, definition.AgentID)
	}
	for _, writer := range s.ReleaseWriters {
		if err := writer.WriteRelease(ctx, tx, tenantID, definition); err != nil {
			return err
		}
	}
	return nil
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
	if s.qs != nil {
		return nil
	}
	if s.DB == nil {
		return fmt.Errorf("studio service: database is required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, studioQueries) })
	return nil
}

// ListAgents returns tenant agents with generic readiness and product labels.
func (s *StudioService) ListAgents(ctx context.Context, tenantID int64) ([]domain.AgentSummary, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
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
		summary := domain.AgentSummary{
			AgentID: common.AsString(row[0]), AgentTypeID: common.AsString(row[1]),
			DisplayName: common.AsString(row[2]), Active: common.AsBool(row[3]), Enabled: common.AsBool(row[4]),
			DraftRevision: common.AsInt64(row[5]), PromptProfileRevision: common.AsInt64(row[6]),
			Default: common.AsBool(row[7]), PublishedVersion: common.AsString(row[8]),
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
func (s *StudioService) GetDraft(ctx context.Context, tenantID int64, agentID string) (domain.AgentDraft, error) {
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
	return domain.AgentDraft{
		AgentID: agentID, AgentTypeID: common.AsString(row[0]), DisplayName: common.AsString(row[1]),
		Active: common.AsBool(row[2]), Enabled: common.AsBool(row[3]),
		ApprovalPolicy: domain.AgentApprovalPolicy{RequireApproval: common.AsBool(row[4])},
		Models: domain.AgentModelSelection{
			Text: dbModelReference(row[5], row[6]), Image: dbModelReference(row[7], row[8]), Video: dbModelReference(row[9], row[10]),
		},
		Extension: json.RawMessage(common.AsString(row[11])), ExpectedDraftRevision: common.AsInt64(row[12]),
		ExpectedPromptProfileRevision: common.AsInt64(row[13]), Default: common.AsBool(row[14]),
	}, nil
}

// SaveDraft atomically replaces agent overrides and changed tenant defaults.
func (s *StudioService) SaveDraft(ctx context.Context, actor domain.StudioActor, draft domain.AgentDraft) (domain.AgentDraft, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentDraft{}, err
	}
	if err := validateActor(actor); err != nil {
		return domain.AgentDraft{}, err
	}
	current, err := s.getDraft(ctx, actor.TenantID, draft.AgentID)
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
		draft.Enabled, draft.ApprovalPolicy.RequireApproval,
		modelProvider(draft.Models.Text), modelID(draft.Models.Text), modelProvider(draft.Models.Image), modelID(draft.Models.Image),
		modelProvider(draft.Models.Video), modelID(draft.Models.Video), nullableJSON(draft.Extension), actor.ActorID,
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
	return s.GetDraft(ctx, actor.TenantID, draft.AgentID)
}

// SetEnabled changes the kill switch without invoking draft validators.
func (s *StudioService) SetEnabled(ctx context.Context, actor domain.StudioActor, request domain.AgentSetEnabledRequest) (domain.AgentEnabledState, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentEnabledState{}, err
	}
	if err := validateActor(actor); err != nil {
		return domain.AgentEnabledState{}, err
	}
	if _, err := s.getDraft(ctx, actor.TenantID, request.AgentID); err != nil {
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
	res, err := tx.Query(ctx, qStudioSetDraftEnabled, request.Enabled, actor.ActorID, actor.TenantID, request.AgentID, request.ExpectedDraftRevision)
	if err != nil {
		return domain.AgentEnabledState{}, fmt.Errorf("set draft enabled: %w", err)
	}
	if len(res.Rows) == 0 {
		return domain.AgentEnabledState{}, domain.ErrRevisionConflict
	}
	if _, err = tx.Query(ctx, qStudioSetProfileActive, agentState(request.Enabled), "kill_switch",
		strconv.FormatInt(actor.ActorID, 10), actor.TenantID, request.AgentID); err != nil {
		return domain.AgentEnabledState{}, fmt.Errorf("set profile state: %w", err)
	}
	event := "ENABLE"
	if !request.Enabled {
		event = "DISABLE"
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, event, "kill_switch"); err != nil {
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
	if err := validateActor(actor); err != nil {
		return domain.AgentTestResult{}, err
	}
	if s.Tester == nil {
		return domain.AgentTestResult{}, domain.ErrNotReady
	}
	draft, err := s.GetDraft(ctx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	testDraft := draft
	testDraft.Enabled = true
	if err = s.validateDraft(ctx, actor.TenantID, testDraft, true); err != nil {
		return domain.AgentTestResult{}, err
	}
	definition, err := s.definition(ctx, actor.TenantID, draft)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	if err = s.freezeBindings(ctx, &definition, domain.AgentPublishRequest{}); err != nil {
		return domain.AgentTestResult{}, err
	}
	result, err := s.Tester.Execute(ctx, actor, request, definition)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	// The lifecycle audit is Studio's, not the product tester's.
	if _, auditErr := s.qs.Query(ctx, qStudioAudit, actor.TenantID, request.AgentID, "TEST",
		nullableString("language "+result.LanguageCode), actor.ActorID); auditErr != nil {
		return domain.AgentTestResult{}, fmt.Errorf("record studio test audit: %w", auditErr)
	}
	return result, nil
}

// Publish freezes the saved draft and promotes it when the agent owns its alias.
func (s *StudioService) Publish(ctx context.Context, actor domain.StudioActor, request domain.AgentPublishRequest) (domain.AgentRelease, error) {
	if err := validateActor(actor); err != nil {
		return domain.AgentRelease{}, err
	}
	draft, err := s.GetDraft(ctx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	fullDraft := draft
	fullDraft.Enabled = true
	if err = s.validateDraft(ctx, actor.TenantID, fullDraft, true); err != nil {
		return domain.AgentRelease{}, err
	}
	definition, err := s.definition(ctx, actor.TenantID, draft)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	if err = s.freezeBindings(ctx, &definition, request); err != nil {
		return domain.AgentRelease{}, err
	}
	definition.ChangeSummary = request.ChangeSummary
	definition.DraftRevision = request.ExpectedDraftRevision
	definition.PromptProfileRevision = request.ExpectedPromptProfileRevision
	definition.PublishedBy = &actor.ActorID

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
	active := false
	if len(alias.Rows) > 0 {
		aliasRevision = common.AsInt64(alias.Rows[0][1])
		active = common.AsString(alias.Rows[0][0]) == request.AgentID
	}
	if aliasRevision != request.ExpectedPromptProfileRevision {
		return domain.AgentRelease{}, domain.ErrRevisionConflict
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
		nullableString(definition.ChangeSummary), actor.ActorID, nil); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("insert agent version: %w", err)
	}
	if err = s.writeRelease(ctx, tx, actor.TenantID, definition); err != nil {
		return domain.AgentRelease{}, err
	}
	for _, reference := range selectedModels(definition.Models) {
		if _, err = tx.Query(ctx, qStudioGrantModel, actor.TenantID, reference.ProviderID, reference.ModelID); err != nil {
			return domain.AgentRelease{}, fmt.Errorf("grant model %s/%s: %w", reference.ProviderID, reference.ModelID, err)
		}
	}
	if active {
		if _, err = tx.Query(ctx, qStudioDeployVersion, actor.TenantID, request.AgentID, version); err != nil {
			return domain.AgentRelease{}, fmt.Errorf("deploy agent version: %w", err)
		}
	}
	if err = auditStudio(ctx, tx, actor, request.AgentID, "PUBLISH", "version "+version); err != nil {
		return domain.AgentRelease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("commit publish: %w", err)
	}
	committed = true
	return releaseFromDefinition(definition, active), nil
}

// Restore copies an immutable release into a new version.
func (s *StudioService) Restore(ctx context.Context, actor domain.StudioActor, request domain.AgentRestoreRequest) (domain.AgentRelease, error) {
	if err := validateActor(actor); err != nil {
		return domain.AgentRelease{}, err
	}
	if err := s.init(ctx); err != nil {
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
	agentTypeID := common.AsString(lock.Rows[0][1])
	alias, err := tx.Query(ctx, qStudioLockAlias, actor.TenantID, agentTypeID)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("lock restore alias: %w", err)
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
	version, err := nextStudioVersion(ctx, tx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentRelease{}, err
	}
	definition.Version = version
	definition.RestoredFromVersion = request.Version
	definition.ChangeSummary = "Restored from version " + request.Version
	definition.PublishedBy = &actor.ActorID
	definition.PublishedAt = s.now()
	encoded, err := json.Marshal(definition)
	if err != nil {
		return domain.AgentRelease{}, fmt.Errorf("marshal restored definition: %w", err)
	}
	if _, err = tx.Query(ctx, qStudioInsertVersion, actor.TenantID, request.AgentID, version, string(encoded),
		definition.DefinitionDigest, definition.DraftRevision, definition.PromptProfileRevision,
		definition.ChangeSummary, actor.ActorID, request.Version); err != nil {
		return domain.AgentRelease{}, fmt.Errorf("insert restored version: %w", err)
	}
	if err = s.writeRelease(ctx, tx, actor.TenantID, definition); err != nil {
		return domain.AgentRelease{}, err
	}
	active := len(alias.Rows) > 0 && common.AsString(alias.Rows[0][0]) == request.AgentID
	if active {
		if _, err = tx.Query(ctx, qStudioDeployVersion, actor.TenantID, request.AgentID, version); err != nil {
			return domain.AgentRelease{}, fmt.Errorf("deploy restored version: %w", err)
		}
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
	if err := validateActor(actor); err != nil {
		return domain.AgentDraft{}, err
	}
	current, err := s.getDraft(ctx, actor.TenantID, request.AgentID)
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
		res, bumpErr := tx.Query(ctx, qStudioBumpDraft, actor.ActorID, actor.TenantID, request.AgentID, request.ExpectedDraftRevision)
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
		res, bumpErr := tx.Query(ctx, qStudioBumpAlias, actor.ActorID, actor.TenantID, current.AgentTypeID, request.ExpectedPromptProfileRevision)
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
	return s.GetDraft(ctx, actor.TenantID, request.AgentID)
}

// SetDefault moves the logical kind alias with optimistic revision checking.
func (s *StudioService) SetDefault(ctx context.Context, actor domain.StudioActor, request domain.AgentSetDefaultRequest) ([]domain.AgentSummary, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	current, err := s.getDraft(ctx, actor.TenantID, request.AgentID)
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
	res, err := tx.Query(ctx, qStudioSetAlias, request.AgentID, actor.ActorID, actor.TenantID, current.AgentTypeID, request.ExpectedAliasRevision)
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
	return s.ListAgents(ctx, actor.TenantID)
}

// History returns immutable releases newest first.
func (s *StudioService) History(ctx context.Context, tenantID int64, agentID string) ([]domain.AgentRelease, error) {
	if _, err := s.getDraft(ctx, tenantID, agentID); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qStudioHistory, tenantID, agentID)
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
func (s *StudioService) AuditLog(ctx context.Context, tenantID int64, agentID string) ([]domain.AgentStudioEvent, error) {
	if _, err := s.getDraft(ctx, tenantID, agentID); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qStudioAuditLog, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list studio audit: %w", err)
	}
	events := make([]domain.AgentStudioEvent, 0, len(res.Rows))
	for _, row := range res.Rows {
		event := domain.AgentStudioEvent{Event: common.AsString(row[0]), Detail: common.AsString(row[1])}
		if row[2] != nil {
			actorID := common.AsInt64(row[2])
			event.ActorID = &actorID
		}
		if value, ok := row[3].(time.Time); ok {
			event.OccurredAt = value
		}
		events = append(events, event)
	}
	return events, nil
}

// ReleaseSections returns compiled sections from one immutable definition.
func (s *StudioService) ReleaseSections(ctx context.Context, tenantID int64, agentID, version string) ([]domain.AgentReleaseSection, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qStudioGetVersion, tenantID, agentID, version)
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
func (s *StudioService) Models(ctx context.Context, tenantID int64) ([]domain.StudioModel, error) {
	if s.Catalog == nil {
		return nil, fmt.Errorf("studio service: model catalog is required")
	}
	return s.Catalog.List(ctx, tenantID)
}

func (s *StudioService) definition(ctx context.Context, tenantID int64, draft domain.AgentDraft) (domain.AgentDefinition, error) {
	definition := domain.AgentDefinition{
		AgentID: draft.AgentID, AgentTypeID: draft.AgentTypeID, Enabled: draft.Enabled, Models: draft.Models,
		ApprovalPolicy: draft.ApprovalPolicy, Extension: cloneJSON(draft.Extension),
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

// freezeBindings takes the request's tools and loop, and its skills, each falling back to what the
// agent type declares, so a publish that names none cannot drop a type's tool or skill set.
func (s *StudioService) freezeBindings(ctx context.Context, definition *domain.AgentDefinition, request domain.AgentPublishRequest) error {
	tools, loop, skills := request.Tools, request.ToolLoop, request.Skills
	if s.Kinds != nil && (len(tools) == 0 && loop == nil || len(skills) == 0) {
		descriptor, err := s.Kinds.Get(ctx, definition.AgentTypeID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("load agent type %q: %w", definition.AgentTypeID, err)
		}
		if len(tools) == 0 && loop == nil {
			tools, loop = descriptor.Tools, descriptor.ToolLoop
		}
		if len(skills) == 0 {
			skills = descriptor.Skills
		}
	}
	if len(tools) == 0 && loop == nil && len(skills) == 0 {
		return nil
	}
	definition.Tools, definition.ToolLoop, definition.Skills = slices.Clone(tools), loop, slices.Clone(skills)
	digest, err := s.Compiler.DefinitionDigest(*definition)
	if err != nil {
		return err
	}
	definition.DefinitionDigest = digest
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
	if active.ApprovalPolicy != draft.ApprovalPolicy {
		report.Causes = append(report.Causes, "approval_policy")
	}
	if !modelSelectionsEqual(active.Models, draft.Models) {
		report.Causes = append(report.Causes, "models")
	}
	if !bytes.Equal(compactStudioJSON(active.Extension), compactStudioJSON(draft.Extension)) {
		report.Causes = append(report.Causes, "extension")
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
		fields = append(fields, domain.AgentFieldError{Field: "expected_draft_revision", Message: "must be positive"})
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
	bumped, err := tx.Query(ctx, qStudioBumpAlias, actor.ActorID, actor.TenantID, draft.AgentTypeID, draft.ExpectedPromptProfileRevision)
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
					fields = append(fields, domain.AgentFieldError{Field: "merge_mode", Message: "must be append or replace"})
				}
			}
		}
	}
	return fields
}

func auditStudio(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, agentID, event, detail string) error {
	if _, err := tx.Query(ctx, qStudioAudit, actor.TenantID, agentID, event, nullableString(detail), actor.ActorID); err != nil {
		return fmt.Errorf("record studio audit: %w", err)
	}
	return nil
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
		Enabled: definition.Enabled, Models: definition.Models, ApprovalPolicy: definition.ApprovalPolicy,
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
	if actor.TenantID <= 0 || actor.ActorID <= 0 {
		return fmt.Errorf("%w: tenant and actor are required", domain.ErrValidation)
	}
	return nil
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
