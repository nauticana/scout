package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
