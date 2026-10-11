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
)

const (
	qPauseLiveTurns   = "scout_pause_live_turns"
	defaultPauseBatch = 200
)

// Turns already asked to stop drop out, so the batches end.
var pauseQueries = map[string]string{
	qPauseLiveTurns: `
SELECT turn.request_id
  FROM conversation_turn turn
  JOIN agent_conversation conv ON conv.partner_id = turn.partner_id AND conv.conversation_id = turn.conversation_id
 WHERE turn.partner_id = ? AND conv.agent_id = ? AND turn.cancel_requested_at IS NULL
   AND turn.status_code IN (` + sqlTurnQueued + `, ` + sqlTurnRunning + `, ` + sqlTurnStreaming + `, ` + sqlTurnSuspended + `)
 ORDER BY turn.request_id
 LIMIT ?`,
}

// StudioPause pauses an agent. Disabling it through Studio closes admission first: the
// turn record store reads the agent's state under a share lock in the transaction that
// opens a turn, so no turn opens after the pause commits. Then the policy applies to the
// turns already live: finish leaves them, cancel asks each to stop through the durable
// canceller, under the runtime's own cancellation and effect-verification rules.
type StudioPause struct {
	DB        port.DatabaseRepository
	Studio    contract.AgentStudioHTTPBackend
	Canceller contract.TurnCanceller
	// Batch is how many live turns one listing asks to stop; default 200.
	Batch int

	once sync.Once
	qs   port.QueryService
}

var _ contract.StudioPauseBackend = (*StudioPause)(nil)

func (p *StudioPause) Pause(ctx context.Context, actor domain.StudioActor, request domain.AgentPauseRequest) (domain.AgentPauseResult, error) {
	if p.DB == nil || p.Studio == nil || p.Canceller == nil {
		return domain.AgentPauseResult{}, fmt.Errorf("studio pause: database, studio, and canceller are required")
	}
	if p.Batch < 0 {
		return domain.AgentPauseResult{}, fmt.Errorf("studio pause: batch must not be negative")
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" || request.Policy != domain.PauseFinish && request.Policy != domain.PauseCancel {
		return domain.AgentPauseResult{}, fmt.Errorf("%w: a pause needs a reason and the policy finish or cancel", domain.ErrValidation)
	}
	state, err := p.Studio.SetEnabled(ctx, actor, domain.AgentSetEnabledRequest{
		AgentID: request.AgentID, Enabled: false, Reason: "paused: " + reason, ExpectedDraftRevision: request.ExpectedDraftRevision,
	})
	if err != nil {
		return domain.AgentPauseResult{}, err
	}
	result := domain.AgentPauseResult{DraftRevision: state.DraftRevision}
	if request.Policy == domain.PauseFinish {
		return result, nil
	}
	p.once.Do(func() { p.qs = p.DB.GetQueryService(ctx, pauseQueries) })
	batch, asked := positiveOr(p.Batch, defaultPauseBatch), map[string]bool{}
	for {
		live, err := p.qs.Query(ctx, qPauseLiveTurns, actor.TenantID, request.AgentID, batch)
		if err != nil {
			return result, fmt.Errorf("list live turns of %q: %w", request.AgentID, err)
		}
		for _, row := range live.Rows {
			requestID := common.AsString(row[0])
			if asked[requestID] {
				return result, fmt.Errorf("%w: turn %q did not record its cancellation", domain.ErrConflict, requestID)
			}
			asked[requestID] = true
			err = p.Canceller.Cancel(ctx, actor.TenantID, requestID, "agent paused")
			if errors.Is(err, domain.ErrNotFound) {
				continue // it ended between the listing and the request
			}
			if err != nil {
				return result, fmt.Errorf("cancel turn of paused agent %q: %w", request.AgentID, err)
			}
			result.Cancelled++
		}
		if len(live.Rows) < batch {
			return result, nil
		}
	}
}
