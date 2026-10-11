package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// MaxActivationLead is how far ahead a publish may schedule its activation.
const MaxActivationLead = 366 * 24 * time.Hour

// deployOrSchedule puts a new release in service now, or, when the request's EffectiveAt
// is after publication, records a pending activation for the activation worker.
func (s *StudioService) deployOrSchedule(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, definition domain.AgentDefinition, request domain.AgentPublishRequest) (bool, string, *domain.ReleaseActivation, error) {
	if !request.EffectiveAt.After(definition.PublishedAt) {
		active, detail, err := deploy(ctx, tx, actor.TenantID, request.AgentID, definition.Version, request.CanaryPercentage)
		return active, detail, nil, err
	}
	activation, err := scheduleActivation(ctx, tx, actor, definition, request.CanaryPercentage, request.EffectiveAt.UTC())
	if err != nil {
		return false, "", nil, err
	}
	return false, " scheduled for " + activation.EffectiveAt.Format(time.RFC3339), activation, nil
}

// scheduleActivation records the deployment the release will replace, so activation can
// compare and swap against it. An agent holds at most one pending activation.
func scheduleActivation(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, definition domain.AgentDefinition, canary int, effectiveAt time.Time) (*domain.ReleaseActivation, error) {
	if canary < 0 || canary > 100 {
		return nil, validationError("canary_percentage", "must be between 0 and 100")
	}
	if canary == 100 {
		canary = 0
	}
	if effectiveAt.Sub(definition.PublishedAt) > MaxActivationLead {
		return nil, validationError("effective_at", "must be within 366 days")
	}
	pending, err := tx.Query(ctx, qSchedulePending, actor.TenantID, definition.AgentID)
	if err != nil {
		return nil, fmt.Errorf("check pending activation: %w", err)
	}
	if len(pending.Rows) > 0 {
		return nil, fmt.Errorf("%w: agent %q already has pending activation %d; cancel it first",
			domain.ErrConflict, definition.AgentID, common.AsInt64(pending.Rows[0][0]))
	}
	deployment, err := tx.Query(ctx, qScheduleDeployment, actor.TenantID, definition.AgentID)
	if err != nil {
		return nil, fmt.Errorf("read deployment to schedule over: %w", err)
	}
	var baseStable, baseCanary any
	if len(deployment.Rows) > 0 {
		baseStable, baseCanary = nullableString(common.AsString(deployment.Rows[0][0])), nullableString(common.AsString(deployment.Rows[0][1]))
	}
	if canary > 0 && baseStable == nil {
		return nil, validationError("canary_percentage", "the first release of an agent deploys as stable")
	}
	inserted, err := tx.Query(ctx, qScheduleInsert, actor.TenantID, definition.AgentID, definition.Version, definition.DefinitionDigest,
		canary, baseStable, baseCanary, effectiveAt, string(actor.Principal.Kind), actor.Principal.ID)
	if err != nil {
		return nil, fmt.Errorf("schedule activation: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return nil, fmt.Errorf("schedule activation: no row returned")
	}
	activation := activationFromRow(inserted.Rows[0])
	return &activation, nil
}

func activationFromRow(row []any) domain.ReleaseActivation {
	activation := domain.ReleaseActivation{
		ID: common.AsInt64(row[0]), TenantID: common.AsInt64(row[1]), AgentID: common.AsString(row[2]),
		Version: common.AsString(row[3]), DefinitionDigest: common.AsString(row[4]), CanaryPercentage: int(common.AsInt64(row[5])),
		BaseStable: common.AsString(row[6]), BaseCanary: common.AsString(row[7]), EffectiveAt: common.AsTime(row[8]).UTC(),
		State:       domain.ReleaseActivationState(common.AsString(row[9])),
		ScheduledBy: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[10])), ID: common.AsString(row[11])},
		ScheduledAt: common.AsTime(row[12]).UTC(), Attempts: int(common.AsInt64(row[13])), NextAttemptAt: optionalTime(row[14]),
		Reason: domain.ReleaseActivationReason(common.AsString(row[15])), Detail: common.AsString(row[16]),
		DecidedAt: optionalTime(row[19]),
	}
	if kind := common.AsString(row[17]); kind != "" {
		activation.DecidedBy = &domain.PrincipalRef{Kind: domain.PrincipalKind(kind), ID: common.AsString(row[18])}
	}
	return activation
}

func optionalTime(value any) *time.Time {
	at, ok := common.AsTimeOK(value)
	if !ok || at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}
