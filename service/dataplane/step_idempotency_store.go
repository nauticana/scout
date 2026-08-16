package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qStepFind           = "scout_step_find"
	qStepClaim          = "scout_step_claim"
	qStepReclaimExpired = "scout_step_reclaim_expired"
	qStepReplay         = "scout_step_replay"
	qStepCommit         = "scout_step_commit"
	qStepAbandon        = "scout_step_abandon"

	stepClaimed   = "claimed"
	stepCommitted = "committed"
	stepAbandoned = "abandoned"
)

// updated_at is written from the injected clock so lease arithmetic never mixes
// database and worker time.
var stepIdempotencyQueries = map[string]string{
	qStepFind: `
SELECT status_code, result_uri, result_digest, updated_at
  FROM step_idempotency
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?`,

	qStepClaim: `
INSERT INTO step_idempotency (tenant_id, request_id, execution_step_id, status_code, updated_at)
VALUES (?, ?, ?, 'claimed', ?)
ON CONFLICT DO NOTHING
RETURNING status_code`,

	qStepReclaimExpired: `
UPDATE step_idempotency
   SET updated_at = ?
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?
   AND status_code = 'claimed' AND updated_at <= ?
RETURNING status_code`,

	qStepReplay: `
UPDATE step_idempotency
   SET status_code = 'claimed', updated_at = ?
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?
   AND status_code = 'abandoned'
RETURNING status_code`,

	qStepCommit: `
UPDATE step_idempotency
   SET status_code = 'committed', result_uri = ?, result_digest = ?, updated_at = ?
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?
   AND status_code = 'claimed'
RETURNING status_code`,

	qStepAbandon: `
UPDATE step_idempotency
   SET status_code = 'abandoned', updated_at = ?
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?
   AND status_code = 'claimed'
RETURNING status_code`,
}

// StepIdempotencyStore makes step execution replay-safe over step_idempotency:
// claimed → committed | abandoned, and abandoned or lease-expired claims are
// reclaimable. Results live in object storage; the row keeps URI+digest.
type StepIdempotencyStore struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec
	// ClaimLease bounds how long a claim blocks other workers before it may be reclaimed.
	ClaimLease time.Duration
	// Now supplies the clock; nil uses time.Now.
	Now func() time.Time

	once sync.Once
	qs   port.QueryService
}

type stepClaim struct {
	status    string
	result    domain.ObjectRef
	updatedAt time.Time
}

func (store *StepIdempotencyStore) validate(tenantID int64, requestID string, step domain.ExecutionStep) error {
	if store.DB == nil || store.Objects == nil || store.ClaimLease <= 0 {
		return fmt.Errorf("%w: step idempotency store needs a database, an object codec, and a positive claim lease", domain.ErrValidation)
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || len([]rune(requestID)) > 120 || step.ExecutionStepID <= 0 {
		return fmt.Errorf("%w: tenant, request id, and compiled execution step id are required", domain.ErrValidation)
	}
	return nil
}

func (store *StepIdempotencyStore) now() time.Time {
	if store.Now != nil {
		return store.Now().UTC()
	}
	return time.Now().UTC()
}

func (store *StepIdempotencyStore) queries(ctx context.Context) port.QueryService {
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, stepIdempotencyQueries) })
	return store.qs
}

func (store *StepIdempotencyStore) find(ctx context.Context, tenantID int64, requestID string, executionStepID int64) (stepClaim, bool, error) {
	result, err := store.queries(ctx).Query(ctx, qStepFind, tenantID, requestID, executionStepID)
	if err != nil {
		return stepClaim{}, false, fmt.Errorf("find step claim: %w", err)
	}
	if len(result.Rows) == 0 {
		return stepClaim{}, false, nil
	}
	row := result.Rows[0]
	if len(row) < 4 {
		return stepClaim{}, false, fmt.Errorf("decode step claim: expected 4 columns, got %d", len(row))
	}
	claim := stepClaim{
		status: common.AsString(row[0]),
		result: domain.ObjectRef{URI: common.AsString(row[1]), Digest: common.AsString(row[2])},
	}
	claim.updatedAt, _ = common.AsTimeOK(row[3])
	return claim, true, nil
}

// Begin claims the step for this worker, replays a committed result, or
// re-claims an abandoned or lease-expired step; a live claim returns ErrConflict.
func (store *StepIdempotencyStore) Begin(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep) (domain.StepResult, bool, error) {
	if err := store.validate(tenantID, requestID, step); err != nil {
		return domain.StepResult{}, false, err
	}
	now := store.now()
	claim, found, err := store.find(ctx, tenantID, requestID, step.ExecutionStepID)
	if err != nil {
		return domain.StepResult{}, false, err
	}
	var applied bool
	switch {
	case !found:
		applied, err = store.transition(ctx, qStepClaim, tenantID, requestID, step.ExecutionStepID, now)
	case claim.status == stepCommitted:
		result, err := store.replay(ctx, claim.result)
		if err != nil {
			return domain.StepResult{}, false, fmt.Errorf("replay step %d of %q: %w", step.ExecutionStepID, requestID, err)
		}
		return result, true, nil
	case claim.status == stepClaimed && claim.updatedAt.Add(store.ClaimLease).After(now):
		return domain.StepResult{}, false, fmt.Errorf("%w: step %d of %q is claimed until %s", domain.ErrConflict, step.ExecutionStepID, requestID, claim.updatedAt.Add(store.ClaimLease).Format(time.RFC3339))
	case claim.status == stepClaimed:
		applied, err = store.transition(ctx, qStepReclaimExpired, now, tenantID, requestID, step.ExecutionStepID, now.Add(-store.ClaimLease))
	case claim.status == stepAbandoned:
		applied, err = store.transition(ctx, qStepReplay, now, tenantID, requestID, step.ExecutionStepID)
	default:
		return domain.StepResult{}, false, fmt.Errorf("%w: unknown idempotency status %q", domain.ErrConflict, claim.status)
	}
	if err != nil {
		return domain.StepResult{}, false, err
	}
	if !applied {
		return domain.StepResult{}, false, fmt.Errorf("%w: step %d of %q was claimed concurrently", domain.ErrConflict, step.ExecutionStepID, requestID)
	}
	return domain.StepResult{}, false, nil
}

// Commit binds the JSON-encoded result to the claim; a repeated commit of the
// same result is a no-op, a lost claim is ErrConflict.
func (store *StepIdempotencyStore) Commit(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep, result domain.StepResult) error {
	if err := store.validate(tenantID, requestID, step); err != nil {
		return err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode step result: %w", err)
	}
	ref, err := store.Objects.Dehydrate(ctx, stepResultName(tenantID, requestID, step.ExecutionStepID), payload)
	if err != nil {
		return fmt.Errorf("dehydrate step result: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	applied, err := store.transition(ctx, qStepCommit, ref.URI, ref.Digest, store.now(), tenantID, requestID, step.ExecutionStepID)
	if err == nil && applied {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("%w: step %d of %q is no longer claimed", domain.ErrConflict, step.ExecutionStepID, requestID)
	}
	return store.discardUnlessReferenced(ctx, tenantID, requestID, step.ExecutionStepID, ref, err)
}

// Abandon releases a claim for replay; abandoning twice is a no-op, a
// committed step is ErrConflict, an unknown one ErrNotFound.
func (store *StepIdempotencyStore) Abandon(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep) error {
	if err := store.validate(tenantID, requestID, step); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	applied, err := store.transition(ctx, qStepAbandon, store.now(), tenantID, requestID, step.ExecutionStepID)
	if err != nil || applied {
		return err
	}
	claim, found, err := store.find(ctx, tenantID, requestID, step.ExecutionStepID)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: step %d of %q was never claimed", domain.ErrNotFound, step.ExecutionStepID, requestID)
	case claim.status == stepAbandoned:
		return nil
	default:
		return fmt.Errorf("%w: step %d of %q is %s", domain.ErrConflict, step.ExecutionStepID, requestID, claim.status)
	}
}

func (store *StepIdempotencyStore) transition(ctx context.Context, query string, args ...any) (bool, error) {
	result, err := store.queries(ctx).Query(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", query, err)
	}
	return len(result.Rows) > 0, nil
}

func (store *StepIdempotencyStore) replay(ctx context.Context, ref domain.ObjectRef) (domain.StepResult, error) {
	payload, err := store.Objects.Hydrate(ctx, ref)
	if err != nil {
		return domain.StepResult{}, err
	}
	var result domain.StepResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return domain.StepResult{}, fmt.Errorf("decode stored result: %w", err)
	}
	return result, nil
}

// A committed row that already points at the same content-addressed object
// keeps it; the upload is deleted only once the row is known to point elsewhere.
func (store *StepIdempotencyStore) discardUnlessReferenced(ctx context.Context, tenantID int64, requestID string, executionStepID int64, ref domain.ObjectRef, cause error) error {
	claim, found, err := store.find(ctx, tenantID, requestID, executionStepID)
	if err != nil {
		return errors.Join(cause, err)
	}
	if found && claim.status == stepCommitted && claim.result.Digest == ref.Digest {
		if errors.Is(cause, domain.ErrConflict) {
			return nil
		}
		return cause
	}
	if err := store.Objects.Delete(context.WithoutCancel(ctx), ref); err != nil {
		return errors.Join(cause, fmt.Errorf("discard %s: %w", ref.URI, err))
	}
	return cause
}

var _ contract.StepIdempotencyStore = (*StepIdempotencyStore)(nil)
