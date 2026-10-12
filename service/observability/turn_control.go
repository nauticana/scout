package observability

import (
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/domain"
)

// controlState is where one turn stands between its control records.
type controlState int

const (
	controlLive controlState = iota
	controlHeld
	controlOperated
	controlEnded
)

// VerifyTurnControl checks one request's records, oldest first, against its call outcomes: a
// takeover follows a hold and precedes the operated turn's actions at its fence and then its
// handback, lapse, hold on an unknown call, or finish; a release follows a hold; every transition
// leaves the turn at a higher fence than the one before; and no takeover happens while a call
// dispatched before it had an open outcome. Records of other categories are not control steps.
func VerifyTurnControl(records []domain.DecisionRecord, outcomes []domain.CallOutcome) error {
	state, last, operated := controlLive, int64(0), int64(0)
	for index, record := range records {
		category := record.Category
		switch category {
		case AuditCategoryTurnHeld, AuditCategoryTurnHoldRequested, AuditCategoryTurnTakenOver, AuditCategoryTurnOperatorAction,
			AuditCategoryTurnHandedBack, AuditCategoryTurnReleased, AuditCategoryTurnHoldLapsed, AuditCategoryTurnCompleted,
			AuditCategoryTurnFailed, AuditCategoryTurnCancelled, AuditCategoryTurnSuspended:
		default:
			continue
		}
		fence, err := recordFence(record)
		if err != nil {
			return fmt.Errorf("%w: %s record %d: %w", domain.ErrConflict, category, index, err)
		}
		if fence < last {
			return fmt.Errorf("%w: %s record %d is at fence %d, below %d", domain.ErrConflict, category, index, fence, last)
		}
		advance := func(from []controlState, to controlState) error {
			if !containsState(from, state) {
				return fmt.Errorf("%w: %s record %d does not follow from the turn's state", domain.ErrConflict, category, index)
			}
			if fence == last {
				return fmt.Errorf("%w: %s record %d leaves the turn at fence %d, which an earlier transition reached", domain.ErrConflict, category, index, fence)
			}
			state, last = to, fence
			return nil
		}
		switch category {
		case AuditCategoryTurnHeld:
			err = advance([]controlState{controlLive, controlOperated}, controlHeld)
		case AuditCategoryTurnTakenOver:
			if err = advance([]controlState{controlHeld}, controlOperated); err == nil {
				operated, err = fence, openAtTakeover(outcomes, fence)
			}
		case AuditCategoryTurnOperatorAction:
			if state != controlOperated || fence != operated {
				err = fmt.Errorf("%w: operator action %d is not at the fence %d the turn was taken over at", domain.ErrConflict, index, operated)
			}
		case AuditCategoryTurnHandedBack:
			err = advance([]controlState{controlOperated}, controlLive)
		case AuditCategoryTurnHoldLapsed:
			err = advance([]controlState{controlOperated}, controlHeld)
		case AuditCategoryTurnReleased:
			err = advance([]controlState{controlHeld}, controlLive)
		case AuditCategoryTurnSuspended:
			err = advance([]controlState{controlLive}, controlLive)
		case AuditCategoryTurnCompleted:
			// The worker completes a live turn; a person finishes the one they operate, at its fence.
			if state == controlOperated && fence == operated || state == controlLive {
				state, last = controlEnded, fence
			} else {
				err = fmt.Errorf("%w: completion record %d does not follow from the turn's state", domain.ErrConflict, index)
			}
		case AuditCategoryTurnCancelled:
			// A cancellation wakes a held or operated turn, and the worker ends it.
			state, last = controlEnded, fence
		default:
			if state != controlLive {
				err = fmt.Errorf("%w: %s record %d is made while no worker runs the turn", domain.ErrConflict, category, index)
			}
			if category == AuditCategoryTurnFailed {
				state, last = controlEnded, fence
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func containsState(states []controlState, state controlState) bool {
	for _, candidate := range states {
		if candidate == state {
			return true
		}
	}
	return false
}

// recordFence reads the fence every turn record's payload carries.
func recordFence(record domain.DecisionRecord) (int64, error) {
	var payload struct {
		Fence *int64 `json:"fence"`
	}
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return 0, fmt.Errorf("payload does not decode: %w", err)
	}
	if payload.Fence == nil || *payload.Fence < 0 {
		return 0, fmt.Errorf("payload carries no fence")
	}
	return *payload.Fence, nil
}

// openAtTakeover refuses a takeover at fence while a call dispatched before it was still open:
// one still open now, or one settled at or after the takeover's fence.
func openAtTakeover(outcomes []domain.CallOutcome, fence int64) error {
	for _, outcome := range outcomes {
		if outcome.DispatchFence < fence && (outcome.Status.Open() || outcome.ResolvedFence >= fence) {
			return fmt.Errorf("%w: the takeover at fence %d happened while call %q had an open outcome", domain.ErrConflict, fence, outcome.Key.CallID)
		}
	}
	return nil
}
