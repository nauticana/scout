package release

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qRolloutGetState         = "scout_release_rollout_get_state"
	qRolloutCreateState      = "scout_release_rollout_create_state"
	qRolloutCasState         = "scout_release_rollout_cas_state"
	qRolloutInsertTransition = "scout_release_rollout_insert_transition"
	qRolloutLiveStates       = "scout_release_rollout_live_states"
	qRolloutRecordBypass     = "scout_release_rollout_record_bypass"
	qRolloutBypasses         = "scout_release_rollout_bypasses"
	qRolloutAcquireLease     = "scout_release_rollout_acquire_lease"
	qRolloutReleaseLease     = "scout_release_rollout_release_lease"
	qRolloutStartRing        = "scout_release_rollout_start_ring"
	qRolloutSetRing          = "scout_release_rollout_set_ring"
	qRolloutFinishRings      = "scout_release_rollout_finish_rings"
)

const rolloutStateColumns = `s.platform_version, s.stage_code, s.ring_code, s.traffic_percentage, s.generation,
       s.paused, s.pause_reason, s.stage_started_at, s.min_samples, s.min_duration_ms,
       s.consecutive_breaches, s.consecutive_healthy, s.last_breach_at, s.lease_owner, s.lease_until`

var rolloutStateQueries = map[string]string{
	qRolloutGetState: `
SELECT ` + rolloutStateColumns + `
  FROM platform_rollout_state s
 WHERE s.platform_version = ?`,
	qRolloutCreateState: `
INSERT INTO platform_rollout_state
       (platform_version, stage_code, ring_code, traffic_percentage, generation, paused, stage_started_at, min_samples, min_duration_ms)
VALUES (?, ?, ?, ?, 1, FALSE, ?, ?, ?)`,
	// The lease and generation guards make a stale controller lose the race instead of overwriting.
	qRolloutCasState: `
UPDATE platform_rollout_state
   SET stage_code = ?, ring_code = ?, traffic_percentage = ?, generation = generation + 1,
       paused = ?, pause_reason = ?, stage_started_at = ?, min_samples = ?, min_duration_ms = ?,
       consecutive_breaches = ?, consecutive_healthy = ?, last_breach_at = ?
 WHERE platform_version = ? AND generation = ? AND lease_owner = ? AND lease_until > ?
RETURNING generation`,
	qRolloutInsertTransition: `
INSERT INTO platform_rollout_transition
       (platform_version, from_stage_code, to_stage_code, from_generation, actor, reason, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
	qRolloutLiveStates: `
SELECT ` + rolloutStateColumns + `
  FROM platform_rollout_state s
  JOIN rollout_stage stage ON stage.code = s.stage_code
 WHERE stage.is_terminal = FALSE
 ORDER BY s.stage_started_at, s.platform_version`,
	qRolloutRecordBypass: `
INSERT INTO platform_rollout_bypass
       (platform_version, stage_code, scope, reason, requested_by, approved_by, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	qRolloutBypasses: `
SELECT id, stage_code, scope, reason, requested_by, approved_by, expires_at, created_at
  FROM platform_rollout_bypass
 WHERE platform_version = ?
 ORDER BY created_at, id`,
	qRolloutAcquireLease: `
UPDATE platform_rollout_state
   SET lease_owner = ?, lease_until = ?
 WHERE platform_version = ?
   AND (lease_owner IS NULL OR lease_owner = ? OR lease_until <= ?)
RETURNING lease_owner`,
	qRolloutReleaseLease: `
UPDATE platform_rollout_state
   SET lease_owner = NULL, lease_until = NULL
 WHERE platform_version = ? AND lease_owner = ?`,
	qRolloutStartRing: `
INSERT INTO platform_rollout (platform_version, ring_code, traffic_percentage, status_code, started_at)
SELECT ?, ?, ?, 'active', ?
 WHERE NOT EXISTS (SELECT 1 FROM platform_rollout WHERE platform_version = ? AND ring_code = ?)`,
	qRolloutSetRing: `
UPDATE platform_rollout
   SET traffic_percentage = ?, status_code = ?, halt_reason = ?
 WHERE platform_version = ? AND ring_code = ?`,
	qRolloutFinishRings: `
UPDATE platform_rollout
   SET status_code = ?, completed_at = ?, halt_reason = ?
 WHERE platform_version = ? AND completed_at IS NULL`,
}

// TableRolloutStateStore is the keel-backed RolloutStateStore and RolloutLease
// over platform_rollout_state; it mirrors ring stages into platform_rollout.
type TableRolloutStateStore struct {
	DB  keelport.DatabaseRepository
	Now func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

var (
	_ contract.RolloutStateStore = (*TableRolloutStateStore)(nil)
	_ contract.RolloutLease      = (*TableRolloutStateStore)(nil)
)

func (store *TableRolloutStateStore) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("rollout state store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, rolloutStateQueries) })
	if store.qs == nil {
		return fmt.Errorf("rollout state store: query service is required")
	}
	return nil
}

func (store *TableRolloutStateStore) now() time.Time {
	if store.Now != nil {
		return store.Now()
	}
	return time.Now()
}

func (store *TableRolloutStateStore) Get(ctx context.Context, platformVersion string) (domain.RolloutState, error) {
	if err := store.init(ctx); err != nil {
		return domain.RolloutState{}, err
	}
	result, err := store.qs.Query(ctx, qRolloutGetState, platformVersion)
	if err != nil {
		return domain.RolloutState{}, fmt.Errorf("get rollout state: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.RolloutState{}, fmt.Errorf("%w: rollout state for %q", domain.ErrNotFound, platformVersion)
	}
	return decodeRolloutState(result.Rows[0]), nil
}

func (store *TableRolloutStateStore) Create(ctx context.Context, state domain.RolloutState) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	if _, err := store.qs.Query(ctx, qRolloutCreateState,
		state.PlatformVersion, string(state.Stage), nullableString(state.Ring), state.TrafficPercentage,
		state.StageStartedAt, state.MinSamples, state.MinDuration.Milliseconds()); err != nil {
		return fmt.Errorf("create rollout state: %w", err)
	}
	return nil
}

func (store *TableRolloutStateStore) Transition(ctx context.Context, transition domain.RolloutTransition, next domain.RolloutState) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	tx, err := store.DB.BeginTx(ctx, rolloutStateQueries)
	if err != nil {
		return fmt.Errorf("rollout transition: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	updated, err := tx.Query(ctx, qRolloutCasState,
		string(next.Stage), nullableString(next.Ring), next.TrafficPercentage,
		next.Paused, nullableString(next.PauseReason), next.StageStartedAt, next.MinSamples, next.MinDuration.Milliseconds(),
		next.ConsecutiveBreaches, next.ConsecutiveHealthy, nullableTime(next.LastBreachAt),
		next.PlatformVersion, transition.FromGeneration, next.LeaseOwner, transition.OccurredAt)
	if err != nil {
		return fmt.Errorf("rollout transition: update: %w", err)
	}
	if len(updated.Rows) == 0 {
		return fmt.Errorf("%w: rollout %s generation %d is stale or lease lost", domain.ErrRevisionConflict, next.PlatformVersion, transition.FromGeneration)
	}
	if _, err = tx.Query(ctx, qRolloutInsertTransition,
		transition.PlatformVersion, string(transition.From), string(transition.To), transition.FromGeneration,
		transition.Actor, transition.Reason, transition.OccurredAt); err != nil {
		return fmt.Errorf("rollout transition: record: %w", err)
	}
	if err = store.mirrorRing(ctx, tx, transition, next); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("rollout transition: commit: %w", err)
	}
	committed = true
	return nil
}

func (store *TableRolloutStateStore) mirrorRing(ctx context.Context, tx keelport.TxQueryService, transition domain.RolloutTransition, next domain.RolloutState) error {
	switch next.Stage {
	case domain.StageRolledBack, domain.StageQuarantined:
		_, err := tx.Query(ctx, qRolloutFinishRings, "rolled_back", transition.OccurredAt, transition.Reason, next.PlatformVersion)
		return wrapErr("rollout transition: finish rings", err)
	case domain.StageGlobalDefault, domain.StageRetired:
		_, err := tx.Query(ctx, qRolloutFinishRings, "completed", transition.OccurredAt, nil, next.PlatformVersion)
		return wrapErr("rollout transition: finish rings", err)
	}
	if next.Ring == "" {
		return nil
	}
	if _, err := tx.Query(ctx, qRolloutStartRing, next.PlatformVersion, next.Ring, next.TrafficPercentage, transition.OccurredAt, next.PlatformVersion, next.Ring); err != nil {
		return fmt.Errorf("rollout transition: start ring: %w", err)
	}
	status := "active"
	if next.Paused {
		status = "paused"
	}
	_, err := tx.Query(ctx, qRolloutSetRing, next.TrafficPercentage, status, nullableString(next.PauseReason), next.PlatformVersion, next.Ring)
	return wrapErr("rollout transition: set ring", err)
}

func (store *TableRolloutStateStore) Live(ctx context.Context) ([]domain.RolloutState, error) {
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	result, err := store.qs.Query(ctx, qRolloutLiveStates)
	if err != nil {
		return nil, fmt.Errorf("list live rollouts: %w", err)
	}
	states := make([]domain.RolloutState, 0, len(result.Rows))
	for _, row := range result.Rows {
		states = append(states, decodeRolloutState(row))
	}
	return states, nil
}

func (store *TableRolloutStateStore) RecordBypass(ctx context.Context, bypass domain.RolloutBypass) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	if _, err := store.qs.Query(ctx, qRolloutRecordBypass,
		bypass.PlatformVersion, string(bypass.Stage), bypass.Scope, bypass.Reason,
		bypass.RequestedBy, bypass.ApprovedBy, bypass.ExpiresAt, bypass.CreatedAt); err != nil {
		return fmt.Errorf("record rollout bypass: %w", err)
	}
	return nil
}

func (store *TableRolloutStateStore) Bypasses(ctx context.Context, platformVersion string) ([]domain.RolloutBypass, error) {
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	result, err := store.qs.Query(ctx, qRolloutBypasses, platformVersion)
	if err != nil {
		return nil, fmt.Errorf("list rollout bypasses: %w", err)
	}
	bypasses := make([]domain.RolloutBypass, 0, len(result.Rows))
	for _, row := range result.Rows {
		bypasses = append(bypasses, domain.RolloutBypass{
			ID: common.AsInt64(row[0]), PlatformVersion: platformVersion, Stage: domain.RolloutStage(common.AsString(row[1])),
			Scope: common.AsString(row[2]), Reason: common.AsString(row[3]), RequestedBy: common.AsString(row[4]),
			ApprovedBy: common.AsString(row[5]), ExpiresAt: common.AsTime(row[6]), CreatedAt: common.AsTime(row[7]),
		})
	}
	return bypasses, nil
}

func (store *TableRolloutStateStore) Acquire(ctx context.Context, platformVersion, owner string, ttl time.Duration) (bool, error) {
	if strings.TrimSpace(owner) == "" || ttl <= 0 {
		return false, fmt.Errorf("%w: lease owner and positive ttl are required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return false, err
	}
	now := store.now()
	result, err := store.qs.Query(ctx, qRolloutAcquireLease, owner, now.Add(ttl), platformVersion, owner, now)
	if err != nil {
		return false, fmt.Errorf("acquire rollout lease: %w", err)
	}
	return len(result.Rows) > 0, nil
}

func (store *TableRolloutStateStore) Release(ctx context.Context, platformVersion, owner string) error {
	if err := store.init(ctx); err != nil {
		return err
	}
	if _, err := store.qs.Query(ctx, qRolloutReleaseLease, platformVersion, owner); err != nil {
		return fmt.Errorf("release rollout lease: %w", err)
	}
	return nil
}

func decodeRolloutState(row []any) domain.RolloutState {
	return domain.RolloutState{
		PlatformVersion:     common.AsString(row[0]),
		Stage:               domain.RolloutStage(common.AsString(row[1])),
		Ring:                common.AsString(row[2]),
		TrafficPercentage:   int(common.AsInt64(row[3])),
		Generation:          common.AsInt64(row[4]),
		Paused:              common.AsBool(row[5]),
		PauseReason:         common.AsString(row[6]),
		StageStartedAt:      common.AsTime(row[7]),
		MinSamples:          common.AsInt64(row[8]),
		MinDuration:         time.Duration(common.AsInt64(row[9])) * time.Millisecond,
		ConsecutiveBreaches: int(common.AsInt64(row[10])),
		ConsecutiveHealthy:  int(common.AsInt64(row[11])),
		LastBreachAt:        common.AsTime(row[12]),
		LeaseOwner:          common.AsString(row[13]),
		LeaseUntil:          common.AsTime(row[14]),
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func wrapErr(prefix string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}
