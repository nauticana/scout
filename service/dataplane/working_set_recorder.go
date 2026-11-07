package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/workingset"
)

const (
	qWorkingSetMark  = "scout_working_set_mark"
	qWorkingSetClear = "scout_working_set_clear"
	qWorkingSetItem  = "scout_working_set_item"
)

var workingSetQueries = map[string]string{
	// The owner's guarded touch writes the assurance, so a fenced-out worker records nothing.
	qWorkingSetMark: `
UPDATE conversation_turn SET working_set_code = ?
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
RETURNING turn_no, conversation_id`,
	qWorkingSetClear: `
DELETE FROM turn_working_set_item
 WHERE partner_id = ? AND conversation_id = ? AND turn_no = ?`,
	qWorkingSetItem: `
INSERT INTO turn_working_set_item (partner_id, conversation_id, turn_no, candidate_id, selected)
VALUES (?, ?, ?, ?, ?)`,
}

// qWorkingSetItems reads what a turn's working set recorded; the compiler's candidate ceiling bounds it.
const (
	qWorkingSetItems   = "scout_working_set_items"
	workingSetItemsSQL = `
SELECT candidate_id, selected FROM turn_working_set_item
 WHERE partner_id = ? AND conversation_id = ? AND turn_no = ?
 ORDER BY candidate_id`
)

// readTurnWorkingSet reads the recorded ids of a turn that holds assurance; one without reads none.
func readTurnWorkingSet(ctx context.Context, qs port.QueryService, tenantID int64, conversationID string, turnNo int64, assurance string) (domain.TurnWorkingSet, error) {
	set := domain.TurnWorkingSet{Assurance: domain.WorkingSetAssurance(assurance)}
	if assurance == "" {
		return set, nil
	}
	result, err := qs.Query(ctx, qWorkingSetItems, tenantID, conversationID, turnNo)
	if err != nil {
		return set, fmt.Errorf("read the turn's working set: %w", err)
	}
	if err = result.RequireColumns(2); err != nil {
		return set, fmt.Errorf("read the turn's working set: %w", err)
	}
	for _, row := range result.Rows {
		if common.AsBool(row[1]) {
			set.Selected = append(set.Selected, common.AsString(row[0]))
		} else {
			set.Excluded = append(set.Excluded, common.AsString(row[0]))
		}
	}
	return set, nil
}

// TableWorkingSetRecorder records a turn's working set on conversation_turn and
// turn_working_set_item. A resumed turn compiles again, and its latest set replaces the earlier one.
type TableWorkingSetRecorder struct {
	DB port.DatabaseRepository
}

var _ contract.WorkingSetRecorder = (*TableWorkingSetRecorder)(nil)

// RecordWorkingSet writes the assurance and every selected and excluded candidate id in one
// transaction opened by the owner's guarded touch.
func (recorder *TableWorkingSetRecorder) RecordWorkingSet(ctx context.Context, owner domain.TurnOwner, set domain.WorkingSet) (err error) {
	if recorder.DB == nil {
		return fmt.Errorf("%w: working set recorder needs a database", domain.ErrNotReady)
	}
	if err := owner.Check(); err != nil {
		return err
	}
	if err := validRecord(set); err != nil {
		return err
	}
	tx, err := recorder.DB.BeginTx(ctx, workingSetQueries)
	if err != nil {
		return fmt.Errorf("record working set: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("record working set: rollback: %w", rollbackErr))
			}
		}
	}()
	marked, err := tx.Query(ctx, qWorkingSetMark, string(set.Assurance), owner.TenantID, owner.RequestID, owner.Fence)
	if err != nil {
		return fmt.Errorf("record working set: %w", err)
	}
	if len(marked.Rows) == 0 || len(marked.Rows[0]) < 2 {
		return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	turnNo, conversationID := common.AsInt64(marked.Rows[0][0]), common.AsString(marked.Rows[0][1])
	if _, err = tx.Query(ctx, qWorkingSetClear, owner.TenantID, conversationID, turnNo); err != nil {
		return fmt.Errorf("record working set: clear: %w", err)
	}
	insert := func(id string, selected bool) error {
		if _, err := tx.Query(ctx, qWorkingSetItem, owner.TenantID, conversationID, turnNo, id, selected); err != nil {
			return fmt.Errorf("record working set candidate %q: %w", id, err)
		}
		return nil
	}
	for _, item := range set.Selected {
		if err = insert(item.ID, true); err != nil {
			return err
		}
	}
	for _, id := range set.Excluded {
		if err = insert(id, false); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("record working set: commit: %w", err)
	}
	committed = true
	return nil
}

// validRecord refuses an unknown assurance, a blank, overlong, or repeated id, and more ids than a compiler may offer.
func validRecord(set domain.WorkingSet) error {
	if !set.Assurance.Valid() {
		return fmt.Errorf("%w: working set assurance %q is not a working_set_assurance code", domain.ErrValidation, set.Assurance)
	}
	if len(set.Selected)+len(set.Excluded) > workingset.MaxCandidates {
		return fmt.Errorf("%w: a working set records at most %d candidates", domain.ErrValidation, workingset.MaxCandidates)
	}
	seen := make(map[string]bool, len(set.Selected)+len(set.Excluded))
	check := func(id string) error {
		if strings.TrimSpace(id) == "" || len(id) > domain.MaxWorkingSetCandidateIDBytes || seen[id] {
			return fmt.Errorf("%w: working set candidate ids are distinct, non-blank, and at most %d bytes", domain.ErrValidation, domain.MaxWorkingSetCandidateIDBytes)
		}
		seen[id] = true
		return nil
	}
	for _, item := range set.Selected {
		if err := check(item.ID); err != nil {
			return err
		}
	}
	for _, id := range set.Excluded {
		if err := check(id); err != nil {
			return err
		}
	}
	return nil
}

// MemoryWorkingSetRecorder keeps each turn's latest working set in memory, keyed by request id,
// for replays and tests that have no conversation_turn row.
type MemoryWorkingSetRecorder struct {
	mu   sync.Mutex
	sets map[string]domain.WorkingSet
}

var _ contract.WorkingSetRecorder = (*MemoryWorkingSetRecorder)(nil)

func (recorder *MemoryWorkingSetRecorder) RecordWorkingSet(_ context.Context, owner domain.TurnOwner, set domain.WorkingSet) error {
	if err := owner.Check(); err != nil {
		return err
	}
	if err := validRecord(set); err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.sets == nil {
		recorder.sets = make(map[string]domain.WorkingSet)
	}
	recorder.sets[owner.RequestID] = set
	return nil
}

// Set returns the latest working set recorded for the request.
func (recorder *MemoryWorkingSetRecorder) Set(requestID string) (domain.WorkingSet, bool) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	set, ok := recorder.sets[requestID]
	return set, ok
}
