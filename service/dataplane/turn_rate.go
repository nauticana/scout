package dataplane

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/keel/common"
	keellimiter "github.com/nauticana/keel/limiter"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/isolation"
	"github.com/nauticana/scout/service/scope"
)

const (
	qTurnRateLock     = "scout_turn_rate_lock"
	qTurnRateReplay   = "scout_turn_rate_replay"
	qRecordRecentTurn = "scout_turn_record_recent_turn"

	turnRateScope = "agent.turns_per_minute"
)

var turnRateQueries = common.MergeMaps(scope.ReleaseReadQueries, map[string]string{
	qTurnRateLock:   `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`,
	qTurnRateReplay: `SELECT 1 FROM conversation_turn WHERE partner_id = ? AND request_id = ?`,
	// Newest first, the agent's turn at OFFSET limit-1 within the last minute has to age out
	// before another may open; the wait comes from the database clock.
	qRecordRecentTurn: `
SELECT CAST(CEIL(EXTRACT(EPOCH FROM (t.queued_at + INTERVAL '1 minute' - CURRENT_TIMESTAMP)) * 1000) AS BIGINT)
  FROM conversation_turn t
  JOIN agent_conversation c ON c.partner_id = t.partner_id AND c.conversation_id = t.conversation_id
 WHERE t.partner_id = ? AND c.agent_id = ? AND t.queued_at > CURRENT_TIMESTAMP - INTERVAL '1 minute'
 ORDER BY t.queued_at DESC
 LIMIT 1 OFFSET ?`,
})

// turnRateRequest names the agent turn about to be inserted under its conversation's lock.
type turnRateRequest struct {
	TenantID  int64
	ScopeID   string
	AgentID   string
	Release   string
	RequestID string
}

// admitRate refuses an agent turn beyond its pinned release's turns per minute. Every path
// inserting agent turns calls it in its opening transaction after the conversation lock, so
// no second pooled connection is awaited under those locks; a replayed request id passes.
func admitRate(ctx context.Context, tx port.QueryService, request turnRateRequest) error {
	replay, err := tx.Query(ctx, qTurnRateReplay, request.TenantID, request.RequestID)
	if err != nil {
		return fmt.Errorf("open turn: find replayed request: %w", err)
	}
	if len(replay.Rows) > 0 {
		return nil
	}
	limits, err := (&isolation.ReleaseLimits{Releases: txReleases{qs: tx}}).ExecutionLimits(ctx, domain.Principal{
		Kind: domain.PrincipalAgent, ID: request.AgentID, TenantID: request.TenantID, ScopeID: request.ScopeID, Release: request.Release,
	})
	if err != nil {
		return fmt.Errorf("open turn: read release limits: %w", err)
	}
	if limits.TurnsPerMinute <= 0 {
		return nil
	}
	if _, err = tx.Query(ctx, qTurnRateLock, fmt.Sprintf("turn-rate/%d/%s", request.TenantID, request.AgentID)); err != nil {
		return fmt.Errorf("open turn: lock agent turn rate: %w", err)
	}
	recent, err := tx.Query(ctx, qRecordRecentTurn, request.TenantID, request.AgentID, limits.TurnsPerMinute-1)
	if err != nil {
		return fmt.Errorf("open turn: read recent turns: %w", err)
	}
	if len(recent.Rows) == 0 {
		return nil
	}
	wait := max(time.Duration(common.AsInt64(recent.Rows[0][0]))*time.Millisecond, time.Millisecond)
	return &keellimiter.LimitError{Err: domain.ErrRateLimited, Scope: turnRateScope, After: wait}
}

// txReleases reads compiled releases through one open transaction.
type txReleases struct{ qs port.QueryService }

func (r txReleases) Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.EffectiveRelease, error) {
	return scope.GetRelease(ctx, r.qs, tenantID, agentID, agentVersion)
}

func (txReleases) Put(context.Context, domain.EffectiveRelease) error {
	return fmt.Errorf("%w: opening a turn never writes a release", domain.ErrForbidden)
}

var _ contract.EffectiveReleaseRepository = txReleases{}
