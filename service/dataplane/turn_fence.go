package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
)

// turnOwnerGuard is the guarded touch of the owner's turn row. It matches only while the
// owner's fence and a worker-owned status hold, and the implicit row lock of this conditional
// write, held for one statement or transaction, linearizes the owner's write against a hold:
// PostgreSQL re-evaluates fence = ? on the committed row, so no FOR UPDATE or advisory lock is needed.
const turnOwnerGuard = `
UPDATE conversation_turn SET fence = fence
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
RETURNING turn_no`

// ownedBy opens a fenced single-statement write. Its first three placeholders are the owner's
// tenant, request, and fence, and owner holds one row while the owner still owns its turn.
const ownedBy = `WITH owner AS (` + turnOwnerGuard + `),`

// fencedWrite reports a fenced statement's outcome: whether the owner still owned its turn and
// how many rows the write itself changed, so a lost fence is told apart from a key conflict.
const fencedWrite = `
SELECT (SELECT COUNT(*) FROM owner), (SELECT COUNT(*) FROM written)`

type ownerQuerier interface {
	Query(ctx context.Context, queryName string, args ...any) (*keelmodel.QueryResult, error)
}

// ownerArgs prefixes a fenced statement's own arguments with the owner's guard arguments.
func ownerArgs(owner domain.TurnOwner, args ...any) []any {
	return append([]any{owner.TenantID, owner.RequestID, owner.Fence}, args...)
}

// guardOwner runs the guarded touch as the first statement of a transaction; zero rows means
// the owner's fence moved on.
func guardOwner(ctx context.Context, tx ownerQuerier, query string, owner domain.TurnOwner) error {
	_, err := guardOwnerTurn(ctx, tx, query, owner)
	return err
}

// guardOwnerTurn is guardOwner returning the number of the owner's turn.
func guardOwnerTurn(ctx context.Context, tx ownerQuerier, query string, owner domain.TurnOwner) (int64, error) {
	if err := owner.Check(); err != nil {
		return 0, err
	}
	touched, err := tx.Query(ctx, query, ownerArgs(owner)...)
	if err != nil {
		return 0, fmt.Errorf("guard turn owner: %w", err)
	}
	if len(touched.Rows) == 0 || len(touched.Rows[0]) == 0 {
		return 0, fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	return common.AsInt64(touched.Rows[0][0]), nil
}

// fencedOutcome decodes a fencedWrite row: ErrFenceLost when the owner no longer owned the
// turn, else whether the write changed a row.
func fencedOutcome(result *keelmodel.QueryResult, owner domain.TurnOwner) (bool, error) {
	if result == nil || len(result.Rows) == 0 || len(result.Rows[0]) < 2 {
		return false, fmt.Errorf("decode fenced write: expected one row of 2 columns")
	}
	if common.AsInt64(result.Rows[0][0]) == 0 {
		return false, fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	return common.AsInt64(result.Rows[0][1]) > 0, nil
}

// MemoryTurnFences is the in-process fence authority of the memory stores: it grants each
// acquisition of a turn the next fence and tells a store whether an owner still holds its turn.
type MemoryTurnFences struct {
	mu    sync.Mutex
	turns map[streamKey]memoryFence
}

type memoryFence struct {
	fence int64
	owned bool
}

// Acquire grants the turn's next fence to a new owner, fencing out the previous one.
func (fences *MemoryTurnFences) Acquire(tenantID int64, requestID string) (domain.TurnOwner, error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return domain.TurnOwner{}, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	return domain.TurnOwner{TenantID: tenantID, RequestID: requestID, Fence: fences.advance(tenantID, requestID, true)}, nil
}

// Hold moves the turn past its owner without granting a new one and returns the held fence.
func (fences *MemoryTurnFences) Hold(tenantID int64, requestID string) int64 {
	return fences.advance(tenantID, requestID, false)
}

func (fences *MemoryTurnFences) advance(tenantID int64, requestID string, owned bool) int64 {
	fences.mu.Lock()
	defer fences.mu.Unlock()
	if fences.turns == nil {
		fences.turns = make(map[streamKey]memoryFence)
	}
	key := streamKey{tenantID, requestID}
	next := memoryFence{fence: fences.turns[key].fence + 1, owned: owned}
	fences.turns[key] = next
	return next.fence
}

// Check refuses a missing owner and one whose fence moved on.
func (fences *MemoryTurnFences) Check(owner domain.TurnOwner) error {
	if fences == nil {
		return fmt.Errorf("%w: memory store has no fence authority", domain.ErrFenceLost)
	}
	if err := owner.Check(); err != nil {
		return err
	}
	fences.mu.Lock()
	current := fences.turns[streamKey{owner.TenantID, owner.RequestID}]
	fences.mu.Unlock()
	if !current.owned || current.fence != owner.Fence {
		return fmt.Errorf("%w: turn %q is at fence %d, not %d", domain.ErrFenceLost, owner.RequestID, current.fence, owner.Fence)
	}
	return nil
}
