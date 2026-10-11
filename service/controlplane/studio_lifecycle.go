package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// Create makes a disabled, unpublished agent of an existing type. The type row is
// locked for the transaction, so the per-type cap, the display-name check, and the
// idempotency record cannot race another create of the same type.
func (s *StudioService) Create(ctx context.Context, actor domain.StudioActor, request domain.AgentCreateRequest) (domain.AgentSummary, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentSummary{}, err
	}
	if err := validateCreate(request, s.referenceLimit()); err != nil {
		return domain.AgentSummary{}, err
	}
	if err := s.authorizeValue(ctx, actor, domain.StudioCreate, agentTypeScopeValue(request.AgentTypeID)); err != nil {
		return domain.AgentSummary{}, err
	}
	if request.CopyFrom != "" {
		if err := s.Authorize(ctx, actor, domain.StudioView, request.CopyFrom); err != nil {
			return domain.AgentSummary{}, err
		}
	}
	var prepare func() (*agentSeed, error)
	if request.TemplateID != "" {
		if s.Templates == nil {
			return domain.AgentSummary{}, validationError("template_id", "no template catalog is composed")
		}
		prepare = func() (*agentSeed, error) { return s.templateSeed(ctx, actor, request) }
	}
	if err := s.offered(ctx, request.AgentTypeID); err != nil {
		return domain.AgentSummary{}, err
	}
	return s.create(ctx, actor, request, createDigest(request), prepare)
}

// offered refuses a type the product's catalog does not offer; without a catalog every type is offered.
func (s *StudioService) offered(ctx context.Context, agentTypeID string) error {
	if s.Kinds == nil {
		return nil
	}
	if _, err := s.Kinds.Get(ctx, agentTypeID); errors.Is(err, domain.ErrNotFound) {
		return validationError("agent_type", "the product does not offer this agent type")
	} else if err != nil {
		return fmt.Errorf("load agent type %q: %w", agentTypeID, err)
	}
	return nil
}

// create runs one create under the type's lock. A replay of the actor's key returns the agent it
// made before any seed is prepared, so it answers what the first call created even when the
// tenant's catalogs have changed since; otherwise prepare, when set, supplies the new agent's seed.
func (s *StudioService) create(ctx context.Context, actor domain.StudioActor, request domain.AgentCreateRequest, digest string, prepare func() (*agentSeed, error)) (domain.AgentSummary, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentSummary{}, err
	}
	tx, err := s.DB.BeginTx(ctx, studioCreateQueries)
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("begin create transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	locked, err := tx.Query(ctx, qStudioLockType, actor.TenantID, request.AgentTypeID)
	if err != nil {
		return domain.AgentSummary{}, fmt.Errorf("lock agent type: %w", err)
	}
	if len(locked.Rows) == 0 {
		return domain.AgentSummary{}, validationError("agent_type", "unknown agent type")
	}
	agentID, replayed, err := s.replayedCreate(ctx, tx, actor, request.IdempotencyKey, digest)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	if !replayed {
		var seed *agentSeed
		if prepare != nil {
			if seed, err = prepare(); err != nil {
				return domain.AgentSummary{}, err
			}
		}
		if agentID, err = s.createAgent(ctx, tx, actor, request, digest, seed); err != nil {
			return domain.AgentSummary{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentSummary{}, fmt.Errorf("commit create: %w", err)
	}
	committed = true
	return s.summary(ctx, actor.TenantID, agentID)
}

func (s *StudioService) replayedCreate(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, key, digest string) (string, bool, error) {
	prior, err := tx.Query(ctx, qStudioCreateReplay, actor.TenantID, string(actor.Principal.Kind), actor.Principal.ID, key)
	if err != nil {
		return "", false, fmt.Errorf("load create request: %w", err)
	}
	if len(prior.Rows) == 0 {
		return "", false, nil
	}
	if common.AsString(prior.Rows[0][0]) != digest {
		return "", false, fmt.Errorf("%w: idempotency key %q was used for a different create", domain.ErrConflict, key)
	}
	return common.AsString(prior.Rows[0][1]), true, nil
}

func (s *StudioService) createAgent(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, request domain.AgentCreateRequest, digest string, seed *agentSeed) (string, error) {
	census, err := tx.Query(ctx, qStudioTypeCensus, request.DisplayName, actor.TenantID, request.AgentTypeID)
	if err != nil {
		return "", fmt.Errorf("count agents of type: %w", err)
	}
	if count := common.AsInt64(census.Rows[0][0]); count >= int64(s.MaxAgentsPerType) {
		return "", fmt.Errorf("%w: the tenant already has %d agents of type %q", domain.ErrConflict, count, request.AgentTypeID)
	}
	if common.AsInt64(census.Rows[0][1]) > 0 {
		return "", validationError("display_name", "another agent of this type has this name")
	}
	source := domain.AgentDraft{}
	if request.CopyFrom != "" {
		if source, err = s.getDraft(ctx, actor.TenantID, request.CopyFrom); err != nil {
			return "", err
		}
		if source.AgentTypeID != request.AgentTypeID {
			return "", validationError("copy_from", "must be an agent of the same type")
		}
	}
	agentID, err := s.newAgentID(request.AgentTypeID)
	if err != nil {
		return "", err
	}
	scopes, err := s.promptScopes(ctx, actor.TenantID, agentID, request.AgentTypeID)
	if err != nil {
		return "", err
	}
	event, detail := "CREATE", "type defaults"
	if seed != nil {
		if err = s.checkSeed(ctx, actor, agentID, scopes, seed); err != nil {
			return "", err
		}
		source.Models, source.Extension = seed.models, seed.extension
		event, detail = seed.event, seed.detail
	}
	if _, err = tx.Query(ctx, qProvisionScope, actor.TenantID, scopes.TypeScopeID, RootPromptScopeID, "agent_type", request.AgentTypeID); err != nil {
		return "", fmt.Errorf("ensure type scope: %w", err)
	}
	if _, err = tx.Query(ctx, qProvisionScope, actor.TenantID, scopes.AgentScopeID, scopes.TypeScopeID, "agent", request.DisplayName); err != nil {
		return "", fmt.Errorf("create agent scope: %w", err)
	}
	if _, err = tx.Query(ctx, qProvisionProfile, actor.TenantID, agentID, request.AgentTypeID, request.DisplayName, string(domain.AgentStateDraft)); err != nil {
		return "", fmt.Errorf("create agent profile: %w", err)
	}
	if _, err = tx.Query(ctx, qStudioCreateDraft, actor.TenantID, agentID,
		modelProvider(source.Models.Text), modelID(source.Models.Text), modelProvider(source.Models.Image), modelID(source.Models.Image),
		modelProvider(source.Models.Video), modelID(source.Models.Video), nullableJSON(source.Extension), actorUser(actor)); err != nil {
		return "", fmt.Errorf("create agent draft: %w", err)
	}
	if seed != nil {
		if err = s.plantSeed(ctx, tx, actor, agentID, scopes.AgentScopeID, seed); err != nil {
			return "", err
		}
	}
	if request.CopyFrom != "" {
		sourceScopes, scopeErr := s.promptScopes(ctx, actor.TenantID, request.CopyFrom, request.AgentTypeID)
		if scopeErr != nil {
			return "", scopeErr
		}
		if _, err = tx.Query(ctx, qStudioCopyBindings, scopes.AgentScopeID, actorUser(actor), actor.TenantID, sourceScopes.AgentScopeID); err != nil {
			return "", fmt.Errorf("copy agent bindings: %w", err)
		}
		if _, err = tx.Query(ctx, qStudioCopyGuardrails, agentID, actorUser(actor), actor.TenantID, request.CopyFrom); err != nil {
			return "", fmt.Errorf("copy agent guardrails: %w", err)
		}
		detail = "copy of " + request.CopyFrom
	}
	recorded, err := tx.Query(ctx, qStudioCreateRecord, actor.TenantID, string(actor.Principal.Kind), actor.Principal.ID,
		request.IdempotencyKey, digest, agentID)
	if err != nil {
		return "", fmt.Errorf("record create request: %w", err)
	}
	if len(recorded.Rows) == 0 {
		return "", fmt.Errorf("%w: idempotency key %q was used concurrently", domain.ErrConflict, request.IdempotencyKey)
	}
	return agentID, auditStudio(ctx, tx, actor, agentID, event, clipDetail(detail))
}

// Rename changes the display name; grants and evidence key on the permanent agent id.
func (s *StudioService) Rename(ctx context.Context, actor domain.StudioActor, request domain.AgentRenameRequest) (domain.AgentSummary, error) {
	current, err := s.editable(ctx, actor, domain.StudioEdit, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	name := strings.TrimSpace(request.DisplayName)
	if name == "" || len(name) > 200 {
		return domain.AgentSummary{}, validationError("display_name", "must contain 1 to 200 characters")
	}
	err = s.transition(ctx, actor, request.AgentID, request.ExpectedDraftRevision, "RENAME", current.DisplayName+" -> "+name,
		func(tx keelport.TxQueryService) error {
			// The type lock Create also takes keeps two writers from claiming one name.
			if _, err := tx.Query(ctx, qStudioLockType, actor.TenantID, current.AgentTypeID); err != nil {
				return err
			}
			taken, err := tx.Query(ctx, qStudioNameTaken, actor.TenantID, current.AgentTypeID, name, request.AgentID)
			if err != nil {
				return fmt.Errorf("check display name: %w", err)
			}
			if len(taken.Rows) > 0 {
				return validationError("display_name", "another agent of this type has this name")
			}
			_, err = tx.Query(ctx, qStudioRename, name, actor.TenantID, request.AgentID)
			return err
		})
	if err != nil {
		return domain.AgentSummary{}, err
	}
	return s.summary(ctx, actor.TenantID, request.AgentID)
}

// Archive withdraws an agent from use. Its releases, audit, and conversations remain,
// and Unarchive returns it disabled. The type's default and an agent in an open
// delegation stay; product validators refuse other references, such as schedules.
func (s *StudioService) Archive(ctx context.Context, actor domain.StudioActor, request domain.AgentArchiveRequest) (domain.AgentSummary, error) {
	current, err := s.editable(ctx, actor, domain.StudioArchive, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	if current.Default {
		return domain.AgentSummary{}, fmt.Errorf("%w: agent %q is the default of its type", domain.ErrConflict, request.AgentID)
	}
	if err = s.refuseOpenDelegation(ctx, actor.TenantID, request.AgentID); err != nil {
		return domain.AgentSummary{}, err
	}
	if err = s.runValidators(ctx, actor.TenantID, current, domain.ValidateArchive); err != nil {
		return domain.AgentSummary{}, err
	}
	err = s.transition(ctx, actor, request.AgentID, request.ExpectedDraftRevision, "ARCHIVE", "",
		func(tx keelport.TxQueryService) error {
			alias, err := tx.Query(ctx, qStudioLockAlias, actor.TenantID, current.AgentTypeID)
			if err != nil {
				return err
			}
			if len(alias.Rows) > 0 && common.AsString(alias.Rows[0][0]) == request.AgentID {
				return fmt.Errorf("%w: agent %q became the default of its type", domain.ErrConflict, request.AgentID)
			}
			_, err = tx.Query(ctx, qStudioSetProfileActive, string(domain.AgentStateArchived), "archived", actorRef(actor), actor.TenantID, request.AgentID)
			return err
		})
	if err != nil {
		return domain.AgentSummary{}, err
	}
	return s.summary(ctx, actor.TenantID, request.AgentID)
}

// Unarchive restores an archived agent as disabled; enabling it is a separate decision.
func (s *StudioService) Unarchive(ctx context.Context, actor domain.StudioActor, request domain.AgentArchiveRequest) (domain.AgentSummary, error) {
	if err := s.Authorize(ctx, actor, domain.StudioArchive, request.AgentID); err != nil {
		return domain.AgentSummary{}, err
	}
	current, err := s.getDraft(ctx, actor.TenantID, request.AgentID)
	if err != nil {
		return domain.AgentSummary{}, err
	}
	if !current.Archived {
		return domain.AgentSummary{}, fmt.Errorf("%w: agent %q is not archived", domain.ErrConflict, request.AgentID)
	}
	err = s.transition(ctx, actor, request.AgentID, request.ExpectedDraftRevision, "UNARCHIVE", "",
		func(tx keelport.TxQueryService) error {
			if _, err := tx.Query(ctx, qStudioSetProfileActive, string(domain.AgentStateSuspended), "unarchived", actorRef(actor), actor.TenantID, request.AgentID); err != nil {
				return err
			}
			_, err := tx.Query(ctx, qStudioDisableDraft, actor.TenantID, request.AgentID)
			return err
		})
	if err != nil {
		return domain.AgentSummary{}, err
	}
	return s.summary(ctx, actor.TenantID, request.AgentID)
}

// transition bumps the draft revision, applies change, and records event in one transaction.
func (s *StudioService) transition(ctx context.Context, actor domain.StudioActor, agentID string, expectedRevision int64, event, detail string, change func(keelport.TxQueryService) error) error {
	tx, err := s.DB.BeginTx(ctx, studioQueries)
	if err != nil {
		return fmt.Errorf("begin %s transaction: %w", strings.ToLower(event), err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	bumped, err := tx.Query(ctx, qStudioBumpDraft, actorUser(actor), actor.TenantID, agentID, expectedRevision)
	if err != nil {
		return fmt.Errorf("bump draft revision: %w", err)
	}
	if len(bumped.Rows) == 0 {
		return domain.ErrRevisionConflict
	}
	if err = change(tx); err != nil {
		return fmt.Errorf("%s agent: %w", strings.ToLower(event), err)
	}
	if err = auditStudio(ctx, tx, actor, agentID, event, detail); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", strings.ToLower(event), err)
	}
	committed = true
	return nil
}

func (s *StudioService) refuseOpenDelegation(ctx context.Context, tenantID int64, agentID string) error {
	if s.Delegations == nil {
		return nil
	}
	grants, err := s.Delegations.ForAgent(ctx, tenantID, agentID)
	if err != nil {
		return fmt.Errorf("list delegations of %q: %w", agentID, err)
	}
	if len(grants) > 0 {
		return fmt.Errorf("%w: agent %q is named by delegation grant %q", domain.ErrConflict, agentID, grants[0].GrantID)
	}
	return nil
}

func (s *StudioService) summary(ctx context.Context, tenantID int64, agentID string) (domain.AgentSummary, error) {
	summaries, err := s.summaries(ctx, tenantID, func(id string) (bool, error) { return id == agentID, nil })
	if err != nil {
		return domain.AgentSummary{}, err
	}
	if len(summaries) == 0 {
		return domain.AgentSummary{}, domain.ErrNotFound
	}
	return summaries[0], nil
}

// newAgentID keeps a readable type prefix; the random suffix makes it unguessable and unique.
func (s *StudioService) newAgentID(agentTypeID string) (string, error) {
	entropy := s.Entropy
	if entropy == nil {
		entropy = rand.Reader
	}
	suffix := make([]byte, 10)
	if _, err := io.ReadFull(entropy, suffix); err != nil {
		return "", fmt.Errorf("generate agent id: %w", err)
	}
	prefix := agentTypeID
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return prefix + "-" + hex.EncodeToString(suffix), nil
}

func validateCreate(request domain.AgentCreateRequest, referenceLimit int) error {
	var fields []domain.AgentFieldError
	if strings.TrimSpace(request.AgentTypeID) == "" {
		fields = append(fields, domain.AgentFieldError{Field: "agent_type", Message: "required"})
	}
	if name := strings.TrimSpace(request.DisplayName); name == "" || len(name) > 200 || name != request.DisplayName {
		fields = append(fields, domain.AgentFieldError{Field: "display_name", Message: "must contain 1 to 200 characters without surrounding spaces"})
	}
	if key := strings.TrimSpace(request.IdempotencyKey); key == "" || len(key) > 120 {
		fields = append(fields, domain.AgentFieldError{Field: "idempotency_key", Message: "must contain 1 to 120 characters"})
	}
	if request.CopyFrom != "" && request.TemplateID != "" {
		fields = append(fields, domain.AgentFieldError{Field: "copy_from", Message: "copy_from and template_id are exclusive"})
	}
	if request.TemplateID == "" && (request.TemplateVersion != 0 || len(request.References) > 0) {
		fields = append(fields, domain.AgentFieldError{Field: "template_id", Message: "template_version and references apply only to a template"})
	}
	if len(request.TemplateID) > 80 || request.TemplateVersion < 0 {
		fields = append(fields, domain.AgentFieldError{Field: "template_id", Message: "must contain at most 80 characters, with a version of 0 or more"})
	}
	fields = append(fields, validateReferences("references", request.References, referenceLimit)...)
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

func createDigest(request domain.AgentCreateRequest) string {
	parts := []string{
		request.AgentTypeID, request.DisplayName, request.CopyFrom, request.TemplateID,
		strconv.FormatInt(request.TemplateVersion, 10), canonicalReferences(request.References),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}
