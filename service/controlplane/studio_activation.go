package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// DefaultActivationTimeout bounds one activation's checks and swap when the settings' Timeout is zero.
const DefaultActivationTimeout = 2 * time.Minute

// ReleaseActivations shows and cancels scheduled publishes, and activates due ones. An
// activation rechecks, at its effective time, everything a publish checked: the release
// digest, the scheduling actor's PUBLISH authority, the agent's state, and every publish
// guard; it then compares and swaps the deployment it was scheduled over. A failed check
// refuses it with a reason; a transient failure is retried at most MaxAttempts times.
// Conversations already pinned to a release keep it, because only agent_deployment changes.
type ReleaseActivations struct {
	DB keelport.DatabaseRepository
	// Studio authorizes, recomputes digests, and runs the publish guards.
	Studio *StudioService

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioActivationBackend = (*ReleaseActivations)(nil)

// activationRefusal is a check that failed for good; the activation is refused with it.
type activationRefusal struct {
	reason domain.ReleaseActivationReason
	detail string
}

func (s *ReleaseActivations) ready(ctx context.Context) error {
	if s.DB == nil || s.Studio == nil {
		return fmt.Errorf("release activations: database and studio service are required")
	}
	if err := s.Studio.init(ctx); err != nil {
		return err
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, activationQueries) })
	return nil
}

// Activations returns the agent's most recent scheduled publishes, newest effective time first.
func (s *ReleaseActivations) Activations(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.ReleaseActivation, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if err := s.Studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	res, err := s.qs.Query(ctx, qActivationList, actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list release activations: %w", err)
	}
	items := make([]domain.ReleaseActivation, 0, len(res.Rows))
	for _, row := range res.Rows {
		items = append(items, activationFromRow(row))
	}
	return items, nil
}

// Cancel withdraws a pending activation; one that already activated, was refused, or was
// cancelled is a conflict.
func (s *ReleaseActivations) Cancel(ctx context.Context, actor domain.StudioActor, request domain.ReleaseActivationCancel) (domain.ReleaseActivation, error) {
	if err := s.ready(ctx); err != nil {
		return domain.ReleaseActivation{}, err
	}
	if err := s.Studio.Authorize(ctx, actor, domain.StudioPublish, request.AgentID); err != nil {
		return domain.ReleaseActivation{}, err
	}
	if request.ActivationID <= 0 {
		return domain.ReleaseActivation{}, validationError("activation_id", "required")
	}
	if len(request.Reason) > 400 {
		return domain.ReleaseActivation{}, validationError("reason", "must be at most 400 characters")
	}
	tx, err := s.DB.BeginTx(ctx, activationQueries)
	if err != nil {
		return domain.ReleaseActivation{}, fmt.Errorf("begin activation cancel: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	res, err := tx.Query(ctx, qActivationCancel, string(actor.Principal.Kind), actor.Principal.ID,
		nullableString(strings.TrimSpace(request.Reason)), actor.TenantID, request.AgentID, request.ActivationID)
	if err != nil {
		return domain.ReleaseActivation{}, fmt.Errorf("cancel release activation: %w", err)
	}
	if len(res.Rows) == 0 {
		current, getErr := tx.Query(ctx, qActivationGet, actor.TenantID, request.AgentID, request.ActivationID)
		if getErr != nil {
			return domain.ReleaseActivation{}, fmt.Errorf("load release activation: %w", getErr)
		}
		if len(current.Rows) == 0 {
			return domain.ReleaseActivation{}, fmt.Errorf("%w: activation %d of agent %q", domain.ErrNotFound, request.ActivationID, request.AgentID)
		}
		return domain.ReleaseActivation{}, fmt.Errorf("%w: activation %d is %s", domain.ErrConflict, request.ActivationID, common.AsString(current.Rows[0][9]))
	}
	activation := activationFromRow(res.Rows[0])
	if err = auditStudio(ctx, tx, actor, request.AgentID, "ACTIVATION_CANCEL", fmt.Sprintf("activation %d of version %s", activation.ID, activation.Version)); err != nil {
		return domain.ReleaseActivation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ReleaseActivation{}, fmt.Errorf("commit activation cancel: %w", err)
	}
	committed = true
	return activation, nil
}

// ActivateDue activates up to settings.Batch due activations, judged by the store clock.
// It returns what it did and the transient failures it deferred.
func (s *ReleaseActivations) ActivateDue(ctx context.Context, settings domain.ReleaseActivationSettings) (domain.ReleaseActivationReport, error) {
	var report domain.ReleaseActivationReport
	if settings.Batch <= 0 || settings.PollPeriod <= 0 || settings.MaxAttempts <= 0 || settings.Timeout < 0 {
		return report, fmt.Errorf("%w: activation needs a positive batch, poll period, and attempt ceiling and a non-negative timeout, got %d/%s/%d/%s",
			domain.ErrValidation, settings.Batch, settings.PollPeriod, settings.MaxAttempts, settings.Timeout)
	}
	if err := s.ready(ctx); err != nil {
		return report, err
	}
	due, err := s.qs.Query(ctx, qActivationDue, settings.Batch)
	if err != nil {
		return report, fmt.Errorf("list due activations: %w", err)
	}
	var failures []error
	for _, row := range due.Rows {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		state, err := s.activate(ctx, common.AsInt64(row[0]), settings)
		if err != nil {
			failures = append(failures, err)
		}
		switch state {
		case domain.ActivationActivated:
			report.Activated++
		case domain.ActivationRefused:
			report.Refused++
		case domain.ActivationPending:
			report.Deferred++
		}
	}
	return report, errors.Join(failures...)
}

// activate claims one due activation and settles it. A transient failure rolls the claim
// back and records the attempt; the returned state is what the activation now is, or
// empty when another worker holds it or it is no longer due.
func (s *ReleaseActivations) activate(ctx context.Context, id int64, settings domain.ReleaseActivationSettings) (domain.ReleaseActivationState, error) {
	timeout := activationTimeout(settings)
	activation, state, err := s.claimAndSettle(ctx, id, timeout)
	if err == nil || activation == nil {
		return state, err
	}
	// A stopping worker is not the activation's failure: the claim rolled back and the next poll retries it uncharged.
	if ctx.Err() != nil {
		return "", err
	}
	// The claim transaction has rolled back, so the row lock is released before the deferral writes it.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return s.deferAttempt(recordCtx, *activation, settings, err)
}

func activationTimeout(settings domain.ReleaseActivationSettings) time.Duration {
	if settings.Timeout > 0 {
		return settings.Timeout
	}
	return DefaultActivationTimeout
}

// claimAndSettle returns the claimed activation, or nil when there was none to claim.
func (s *ReleaseActivations) claimAndSettle(parent context.Context, id int64, timeout time.Duration) (*domain.ReleaseActivation, domain.ReleaseActivationState, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, activationQueries)
	if err != nil {
		return nil, "", fmt.Errorf("begin activation %d: %w", id, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	claimed, err := tx.Query(ctx, qActivationClaim, id)
	if err != nil {
		return nil, "", fmt.Errorf("claim activation %d: %w", id, err)
	}
	if len(claimed.Rows) == 0 {
		return nil, "", nil
	}
	activation := activationFromRow(claimed.Rows[0])
	state, err := s.settle(ctx, tx, activation)
	if err != nil {
		return &activation, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return &activation, "", fmt.Errorf("commit activation %d: %w", id, err)
	}
	committed = true
	return &activation, state, nil
}

// settle runs every check and either activates or refuses inside the claim transaction;
// an error is transient and leaves the activation pending.
func (s *ReleaseActivations) settle(ctx context.Context, tx keelport.TxQueryService, activation domain.ReleaseActivation) (domain.ReleaseActivationState, error) {
	actor := schedulingActor(activation)
	refusal, err := s.check(ctx, tx, actor, activation)
	if err != nil {
		return "", err
	}
	if refusal == nil {
		refusal, err = s.lockAgent(ctx, tx, activation)
		if err != nil {
			return "", err
		}
	}
	detail := ""
	if refusal == nil {
		detail, refusal, err = s.swap(ctx, tx, activation)
		if err != nil {
			return "", err
		}
	}
	if refusal != nil {
		if err = s.refuse(ctx, tx, actor, activation, *refusal, 0); err != nil {
			return "", err
		}
		return domain.ActivationRefused, nil
	}
	done, err := tx.Query(ctx, qActivationDone, nullableString(detail), activation.ID)
	if err != nil {
		return "", fmt.Errorf("record activation %d: %w", activation.ID, err)
	}
	if len(done.Rows) == 0 {
		return "", fmt.Errorf("%w: activation %d is no longer pending", domain.ErrConflict, activation.ID)
	}
	event := fmt.Sprintf("version %s activated as scheduled", activation.Version)
	if activation.CanaryPercentage > 0 {
		event += fmt.Sprintf(" as a %d%% canary", activation.CanaryPercentage)
	}
	if detail != "" {
		event += "; " + detail
	}
	if err = auditStudio(ctx, tx, actor, activation.AgentID, "ACTIVATE", event); err != nil {
		return "", err
	}
	return domain.ActivationActivated, nil
}

// check repeats the publish checks against the frozen release.
func (s *ReleaseActivations) check(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, activation domain.ReleaseActivation) (*activationRefusal, error) {
	if err := s.Studio.Authorize(ctx, actor, domain.StudioPublish, activation.AgentID); err != nil {
		return refusalOr(err, domain.ActivationNotAuthorized)
	}
	version, err := tx.Query(ctx, qActivationVersion, activation.TenantID, activation.AgentID, activation.Version)
	if err != nil {
		return nil, fmt.Errorf("load scheduled version: %w", err)
	}
	if len(version.Rows) == 0 {
		return &activationRefusal{reason: domain.ActivationVersionMissing, detail: "version " + activation.Version + " does not exist"}, nil
	}
	definition, err := decodeDefinition(version.Rows[0][0])
	if err != nil {
		return &activationRefusal{reason: domain.ActivationDigestMismatch, detail: "the stored definition does not decode"}, nil
	}
	digest, err := s.Studio.Compiler.DefinitionDigest(definition)
	if err != nil {
		return refusalOr(err, domain.ActivationDigestMismatch)
	}
	if digest != activation.DefinitionDigest || digest != common.AsString(version.Rows[0][1]) || digest != definition.DefinitionDigest {
		return &activationRefusal{reason: domain.ActivationDigestMismatch, detail: "the release no longer matches the digest it was scheduled with"}, nil
	}
	settings, refusal, err := frozenSettings(ctx, tx, activation, definition.SettingsDigest)
	if refusal != nil || err != nil {
		return refusal, err
	}
	if err = s.Studio.checkBudget(ctx, tx, settings.Resources, definition); err != nil {
		return refusalOr(err, domain.ActivationGuardRefused)
	}
	if err = s.Studio.guard(ctx, domain.PublishCandidate{Actor: actor, Definition: definition, Settings: settings, Activation: true}); err != nil {
		return refusalOr(err, domain.ActivationGuardRefused)
	}
	return nil, nil
}

// lockAgent holds the agent's draft row, which Archive and Publish write, until the swap
// commits, then reads the agent's state afresh so an archive cannot slip in between.
func (s *ReleaseActivations) lockAgent(ctx context.Context, tx keelport.TxQueryService, activation domain.ReleaseActivation) (*activationRefusal, error) {
	locked, err := tx.Query(ctx, qActivationLockDraft, activation.TenantID, activation.AgentID)
	if err != nil {
		return nil, fmt.Errorf("lock agent for activation %d: %w", activation.ID, err)
	}
	if len(locked.Rows) == 0 {
		return &activationRefusal{reason: domain.ActivationVersionMissing, detail: "agent " + activation.AgentID + " does not exist"}, nil
	}
	state, err := tx.Query(ctx, qActivationAgentState, activation.TenantID, activation.AgentID)
	if err != nil {
		return nil, fmt.Errorf("read agent state for activation %d: %w", activation.ID, err)
	}
	if len(state.Rows) == 0 {
		return &activationRefusal{reason: domain.ActivationVersionMissing, detail: "agent " + activation.AgentID + " does not exist"}, nil
	}
	if domain.AgentState(common.AsString(state.Rows[0][0])) == domain.AgentStateArchived {
		return &activationRefusal{reason: domain.ActivationAgentArchived, detail: "agent " + activation.AgentID + " is archived"}, nil
	}
	return nil, nil
}

// swap moves the deployment to the release only while it still serves what the activation
// was scheduled over. A deployment already serving the release as planned is activated again
// without a write, so a replayed activation is idempotent.
func (s *ReleaseActivations) swap(ctx context.Context, tx keelport.TxQueryService, activation domain.ReleaseActivation) (string, *activationRefusal, error) {
	var (
		res *keelmodel.QueryResult
		err error
	)
	baseCanary := nullableString(activation.BaseCanary)
	switch {
	case activation.CanaryPercentage > 0:
		res, err = tx.Query(ctx, qActivationCanary, activation.Version, activation.CanaryPercentage,
			activation.TenantID, activation.AgentID, activation.BaseStable, baseCanary)
	case activation.BaseStable == "":
		res, err = tx.Query(ctx, qActivationFirstStable, activation.TenantID, activation.AgentID, activation.Version)
	default:
		res, err = tx.Query(ctx, qActivationStable, activation.Version,
			activation.TenantID, activation.AgentID, activation.BaseStable, baseCanary)
	}
	if err != nil {
		return "", nil, fmt.Errorf("swap deployment for activation %d: %w", activation.ID, err)
	}
	if len(res.Rows) > 0 {
		return "", nil, nil
	}
	current, err := tx.Query(ctx, qActivationDeployment, activation.TenantID, activation.AgentID)
	if err != nil {
		return "", nil, fmt.Errorf("read deployment for activation %d: %w", activation.ID, err)
	}
	stable, canary, percentage := "", "", 0
	if len(current.Rows) > 0 {
		row := current.Rows[0]
		stable, canary, percentage = common.AsString(row[0]), common.AsString(row[1]), int(common.AsInt64(row[2]))
	}
	planned := stable == activation.Version && canary == ""
	if activation.CanaryPercentage > 0 {
		planned = canary == activation.Version && percentage == activation.CanaryPercentage
	}
	if planned {
		return "the deployment already served it", nil, nil
	}
	return "", &activationRefusal{reason: domain.ActivationDeploymentChanged, detail: fmt.Sprintf(
		"scheduled over stable %q canary %q, the deployment now serves stable %q canary %q",
		activation.BaseStable, activation.BaseCanary, stable, canary)}, nil
}

func (s *ReleaseActivations) refuse(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, activation domain.ReleaseActivation, refusal activationRefusal, attempt int) error {
	detail := truncate(refusal.detail, 400)
	res, err := tx.Query(ctx, qActivationRefuse, string(refusal.reason), nullableString(detail), attempt, activation.ID)
	if err != nil {
		return fmt.Errorf("refuse activation %d: %w", activation.ID, err)
	}
	if len(res.Rows) == 0 {
		return fmt.Errorf("%w: activation %d is no longer pending", domain.ErrConflict, activation.ID)
	}
	return auditStudio(ctx, tx, actor, activation.AgentID, "ACTIVATION_REFUSED",
		truncate(fmt.Sprintf("version %s: %s: %s", activation.Version, refusal.reason, detail), 400))
}

// deferAttempt records a transient failure: the activation waits attempts × poll period
// before its next try and is refused once its attempts are spent. The stored detail stays
// generic; the cause returns to the worker's journal.
func (s *ReleaseActivations) deferAttempt(ctx context.Context, activation domain.ReleaseActivation, settings domain.ReleaseActivationSettings, cause error) (domain.ReleaseActivationState, error) {
	attempt := activation.Attempts + 1
	cause = fmt.Errorf("activation %d attempt %d: %w", activation.ID, attempt, cause)
	tx, err := s.DB.BeginTx(ctx, activationQueries)
	if err != nil {
		return "", errors.Join(cause, fmt.Errorf("begin activation %d deferral: %w", activation.ID, err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	state := domain.ActivationPending
	if attempt >= settings.MaxAttempts {
		state = domain.ActivationRefused
		err = s.refuse(ctx, tx, schedulingActor(activation), activation, activationRefusal{reason: domain.ActivationAttemptsExhausted,
			detail: fmt.Sprintf("%d attempts failed transiently", attempt)}, 1)
	} else {
		delay := int64(attempt) * int64(settings.PollPeriod/time.Second)
		var res *keelmodel.QueryResult
		res, err = tx.Query(ctx, qActivationDefer, max(delay, 1), fmt.Sprintf("attempt %d failed transiently; retrying", attempt), activation.ID)
		if err == nil && len(res.Rows) == 0 {
			err = fmt.Errorf("%w: activation %d is no longer pending", domain.ErrConflict, activation.ID)
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return "", errors.Join(cause, fmt.Errorf("record activation %d attempt: %w", activation.ID, err))
	}
	committed = true
	return state, cause
}

// schedulingActor is who scheduled the activation; it acts again, under its own authority, when the activation runs.
func schedulingActor(activation domain.ReleaseActivation) domain.StudioActor {
	return domain.StudioActor{TenantID: activation.TenantID, Principal: activation.ScheduledBy}
}

// frozenSettings reads the settings frozen with the scheduled version and proves they are
// the ones its definition digest covers; a version published before settings were frozen
// has neither.
func frozenSettings(ctx context.Context, tx keelport.TxQueryService, activation domain.ReleaseActivation, settingsDigest string) (domain.EffectiveRelease, *activationRefusal, error) {
	res, err := tx.Query(ctx, qStudioGetSettings, activation.TenantID, activation.AgentID, activation.Version)
	if err != nil {
		return domain.EffectiveRelease{}, nil, fmt.Errorf("load scheduled release settings: %w", err)
	}
	settings := domain.EffectiveRelease{TenantID: activation.TenantID, AgentID: activation.AgentID}
	if len(res.Rows) > 0 {
		if settings, err = decodeSettings(res.Rows[0], activation.TenantID, activation.AgentID); err != nil {
			return domain.EffectiveRelease{}, &activationRefusal{reason: domain.ActivationDigestMismatch, detail: "the frozen settings do not decode"}, nil
		}
	}
	settings.AgentVersion = activation.Version
	if settingsDigest != "" && (len(res.Rows) == 0 || scope.SettingsDigest(settings.Resources) != settingsDigest) {
		return domain.EffectiveRelease{}, &activationRefusal{reason: domain.ActivationDigestMismatch, detail: "the frozen settings no longer match the release"}, nil
	}
	return settings, nil, nil
}

// refusalOr turns a definitive refusal into reason and leaves any other error transient.
func refusalOr(err error, reason domain.ReleaseActivationReason) (*activationRefusal, error) {
	for _, definitive := range []error{domain.ErrValidation, domain.ErrForbidden, domain.ErrUnauthorized, domain.ErrPrincipalUnknown,
		domain.ErrAuthorityExceeded, domain.ErrNotFound, domain.ErrConflict, domain.ErrRevisionConflict, domain.ErrContractFailed,
		domain.ErrSealed, domain.ErrNoPrompts} {
		if errors.Is(err, definitive) {
			return &activationRefusal{reason: reason, detail: err.Error()}, nil
		}
	}
	return nil, err
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
