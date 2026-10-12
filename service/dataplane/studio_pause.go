package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

const (
	qPauseLiveTurns     = "scout_pause_live_turns"
	qPauseHoldableTurns = "scout_pause_holdable_turns"
	defaultPauseBatch   = 200
)

// Turns already asked to stop, or to hold, drop out, so the batches end.
var pauseQueries = map[string]string{
	qPauseLiveTurns: `
SELECT turn.request_id
  FROM conversation_turn turn
  JOIN agent_conversation conv ON conv.partner_id = turn.partner_id AND conv.conversation_id = turn.conversation_id
 WHERE turn.partner_id = ? AND conv.agent_id = ? AND turn.cancel_requested_at IS NULL
   AND turn.status_code IN (` + sqlTurnLive + `)
 ORDER BY turn.request_id
 LIMIT ?`,
	qPauseHoldableTurns: `
SELECT turn.request_id
  FROM conversation_turn turn
  JOIN agent_conversation conv ON conv.partner_id = turn.partner_id AND conv.conversation_id = turn.conversation_id
 WHERE turn.partner_id = ? AND conv.agent_id = ? AND turn.cancel_requested_at IS NULL AND turn.hold_requested_at IS NULL
   AND turn.status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `, ` + sqlTurnOwned + `)
 ORDER BY turn.request_id
 LIMIT ?`,
}

// StudioPause pauses an agent. Disabling it through Studio closes admission first: the
// turn record store reads the agent's state under a share lock in the transaction that
// opens a turn, so no turn opens after the pause commits. Then the policy applies to the
// turns already live: finish leaves them, cancel asks each to stop through the durable
// canceller, under the runtime's own cancellation and effect-verification rules, and suspend
// holds a queued or parked turn at once and asks a running one to hold at its next durable
// checkpoint. Unpausing releases no held turn.
type StudioPause struct {
	DB        port.DatabaseRepository
	Studio    contract.AgentStudioHTTPBackend
	Canceller contract.TurnCanceller
	// Control holds turns under the suspend policy; the policy is refused without it.
	Control contract.TurnControl
	// Audit records each hold; nil records none.
	Audit contract.AuditSink
	// Batch is how many live turns one listing applies the policy to; default 200.
	Batch int
	Now   func() time.Time

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
	switch {
	case reason == "" || request.Policy != domain.PauseFinish && request.Policy != domain.PauseCancel && request.Policy != domain.PauseSuspend:
		return domain.AgentPauseResult{}, fmt.Errorf("%w: a pause needs a reason and the policy finish, cancel, or suspend", domain.ErrValidation)
	case request.Policy == domain.PauseSuspend && p.Control == nil:
		return domain.AgentPauseResult{}, fmt.Errorf("%w: studio pause has no turn control to suspend turns with", domain.ErrNotReady)
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
	listing, apply := qPauseLiveTurns, func(requestID string) error {
		if err := p.Canceller.Cancel(ctx, actor.TenantID, requestID, "agent paused"); err != nil {
			return err
		}
		result.Cancelled++
		return nil
	}
	if request.Policy == domain.PauseSuspend {
		listing, apply = qPauseHoldableTurns, func(requestID string) error {
			return p.hold(ctx, actor, request.AgentID, requestID, "agent paused: "+reason, &result)
		}
	}
	batch, asked := positiveOr(p.Batch, defaultPauseBatch), map[string]bool{}
	for {
		live, err := p.qs.Query(ctx, listing, actor.TenantID, request.AgentID, batch)
		if err != nil {
			return result, fmt.Errorf("list live turns of %q: %w", request.AgentID, err)
		}
		for _, row := range live.Rows {
			requestID := common.AsString(row[0])
			if asked[requestID] {
				return result, fmt.Errorf("%w: turn %q did not record the %s policy", domain.ErrConflict, requestID, request.Policy)
			}
			asked[requestID] = true
			err = apply(requestID)
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrTurnHeld) {
				continue // it ended, or a person took it over, between the listing and the request
			}
			if err != nil {
				return result, fmt.Errorf("%s turn of paused agent %q: %w", request.Policy, request.AgentID, err)
			}
		}
		if len(live.Rows) < batch {
			return result, nil
		}
	}
}

// hold holds one live turn of the paused agent at once, or asks it to hold at its next checkpoint.
func (p *StudioPause) hold(ctx context.Context, actor domain.StudioActor, agentID, requestID, reason string, result *domain.AgentPauseResult) error {
	outcome, err := p.Control.RequestHold(ctx, actor.TenantID, requestID, domain.HoldAgentPaused, reason)
	if err != nil {
		return err
	}
	category := observability.AuditCategoryTurnHoldRequested
	if outcome.Held() {
		category = observability.AuditCategoryTurnHeld
		result.Held++
	} else {
		result.HoldRequested++
	}
	return auditTurnControl(ctx, p.Audit, actor, agentID, requestID, category, outcome.Fence, reason, p.now())
}

func (p *StudioPause) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
