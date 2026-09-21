package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/cache"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	replyChannel            = "scout:reply"
	defaultReplyRetention   = 10 * time.Minute
	defaultReplyPollPeriod  = 500 * time.Millisecond
	cacheReplyRoutePrefix   = "cachereply"
	replyHeadSuffix         = "head"
	replyTerminalRouteLabel = "record"
	recordReadPeriod        = 5 * time.Second
)

// CacheReplyHub carries reply frames between processes through the shared keel
// cache: a worker publishes, any ingress process subscribes or resumes. Frames
// are a delivery buffer that expires after Retention, never the record of a
// turn. When the buffer holds nothing for a request, Records supplies the final
// frame of a turn that already ended, so losing the cache never loses an answer.
type CacheReplyHub struct {
	Cache cache.CacheService
	// Records is optional; without it a subscriber of an expired stream waits until its context ends.
	Records contract.TurnRecordStore
	// Retention is each frame's lifetime; default 10m.
	Retention time.Duration
	// PollInterval bounds delivery latency when a wake-up is lost; default 500ms.
	PollInterval time.Duration

	once   sync.Once
	signal cacheSignal
}

var (
	_ contract.TurnReplyPublisher        = (*CacheReplyHub)(nil)
	_ contract.TurnReplySubscriber       = (*CacheReplyHub)(nil)
	_ contract.ReplayTurnReplySubscriber = (*CacheReplyHub)(nil)
	_ contract.StoredTurnReplyReader     = (*CacheReplyHub)(nil)
)

type replyHead struct {
	Sequence int64 `json:"seq"`
	Final    bool  `json:"final"`
}

func replyStreamKey(tenantID int64, requestID string) string {
	return fmt.Sprintf("%s:%d:%s", replyChannel, tenantID, requestID)
}

func (hub *CacheReplyHub) retention() time.Duration {
	if hub.Retention > 0 {
		return hub.Retention
	}
	return defaultReplyRetention
}

func (hub *CacheReplyHub) relay() *cacheSignal {
	hub.once.Do(func() { hub.signal.cache, hub.signal.channel = hub.Cache, replyChannel })
	return &hub.signal
}

// Close releases the hub's cache subscription.
func (hub *CacheReplyHub) Close() error {
	hub.signal.close()
	return nil
}

// head returns the newest published sequence; found is false for a stream the cache does not hold.
func (hub *CacheReplyHub) head(ctx context.Context, stream string) (replyHead, bool, error) {
	raw, err := hub.Cache.Get(ctx, stream+":"+replyHeadSuffix)
	if errors.Is(err, cache.ErrCacheMiss) {
		return replyHead{}, false, nil
	}
	if err != nil {
		return replyHead{}, false, fmt.Errorf("read reply head: %w", err)
	}
	var head replyHead
	if err = json.Unmarshal([]byte(raw), &head); err != nil {
		return replyHead{}, false, fmt.Errorf("decode reply head: %w", err)
	}
	return head, true, nil
}

func (hub *CacheReplyHub) frame(ctx context.Context, stream string, sequence int64) (domain.TurnReply, bool, error) {
	raw, err := hub.Cache.Get(ctx, fmt.Sprintf("%s:%d", stream, sequence))
	if errors.Is(err, cache.ErrCacheMiss) {
		return domain.TurnReply{}, false, nil
	}
	if err != nil {
		return domain.TurnReply{}, false, fmt.Errorf("read reply frame: %w", err)
	}
	var reply domain.TurnReply
	if err = json.Unmarshal([]byte(raw), &reply); err != nil {
		return domain.TurnReply{}, false, fmt.Errorf("decode reply frame: %w", err)
	}
	return reply, true, nil
}

// Publish stores the frame, advances the head, and wakes subscribers. One leased
// worker publishes a request's frames, so the head needs no compare-and-set.
func (hub *CacheReplyHub) Publish(ctx context.Context, reply domain.TurnReply) error {
	if hub.Cache == nil {
		return fmt.Errorf("cache reply hub: cache is required")
	}
	if reply.TenantID <= 0 || strings.TrimSpace(reply.RequestID) == "" || reply.Sequence < 0 {
		return fmt.Errorf("%w: tenant, request, and sequence are required", domain.ErrValidation)
	}
	stream := replyStreamKey(reply.TenantID, reply.RequestID)
	head, started, err := hub.head(ctx, stream)
	if err != nil {
		return err
	}
	switch {
	case started && reply.Sequence <= head.Sequence:
		retained, found, err := hub.frame(ctx, stream, reply.Sequence)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: reply sequence %d is no longer retained", domain.ErrReplayExpired, reply.Sequence)
		}
		if sameReply(retained, reply) {
			return nil
		}
		// A final frame ends one delivery, not the stream: a suspended turn resumes at its sequence.
		if !head.Final || reply.Sequence != head.Sequence {
			return fmt.Errorf("%w: reply sequence %d has different content", domain.ErrConflict, reply.Sequence)
		}
	// A stream the cache no longer holds restarts at any sequence; a live one admits no gap.
	case started && reply.Sequence > head.Sequence+1:
		return fmt.Errorf("%w: reply sequence %d, want %d", domain.ErrConflict, reply.Sequence, head.Sequence+1)
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("encode reply frame: %w", err)
	}
	encodedHead, _ := json.Marshal(replyHead{Sequence: reply.Sequence, Final: reply.Final})
	if err = hub.Cache.Set(ctx, fmt.Sprintf("%s:%d", stream, reply.Sequence), string(encoded), hub.retention()); err != nil {
		return fmt.Errorf("store reply frame: %w", err)
	}
	if err = hub.Cache.Set(ctx, stream+":"+replyHeadSuffix, string(encodedHead), hub.retention()); err != nil {
		return fmt.Errorf("store reply head: %w", err)
	}
	if err = hub.relay().notify(ctx, stream); err != nil {
		return fmt.Errorf("wake reply subscribers: %w", err)
	}
	return nil
}

// Subscribe opens one reply stream without a replay cursor.
func (hub *CacheReplyHub) Subscribe(ctx context.Context, tenantID int64, requestID string) (contract.TurnReplySubscription, error) {
	return hub.SubscribeFrom(ctx, tenantID, requestID, 0)
}

// SubscribeFrom opens one reply stream from fromSequence, on whichever process the client reached.
func (hub *CacheReplyHub) SubscribeFrom(ctx context.Context, tenantID int64, requestID string, fromSequence int64) (contract.TurnReplySubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hub.Cache == nil {
		return nil, fmt.Errorf("cache reply hub: cache is required")
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || fromSequence < 0 {
		return nil, fmt.Errorf("%w: tenant, request, and cursor are required", domain.ErrValidation)
	}
	stream := replyStreamKey(tenantID, requestID)
	wake, release, err := hub.relay().wait(stream)
	if err != nil {
		return nil, fmt.Errorf("subscribe to reply wake-ups: %w", err)
	}
	return &cacheReplySubscription{
		hub: hub, tenantID: tenantID, requestID: requestID, stream: stream,
		next: fromSequence, wake: wake, release: release,
	}, nil
}

// StoredReply rebuilds a terminal reply from the durable turn record.
func (hub *CacheReplyHub) StoredReply(ctx context.Context, tenantID int64, requestID string, sequence int64) (domain.TurnReply, error) {
	if hub.Records == nil {
		return domain.TurnReply{}, fmt.Errorf("%w: durable turn records are unavailable", domain.ErrNotReady)
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || sequence < 0 {
		return domain.TurnReply{}, fmt.Errorf("%w: tenant, request, and cursor are required", domain.ErrValidation)
	}
	_, status, payload, err := hub.Records.Find(ctx, tenantID, requestID)
	if err != nil {
		return domain.TurnReply{}, err
	}
	if !isTerminalTurnStatus(status) {
		return domain.TurnReply{}, fmt.Errorf("%w: turn %q is not terminal", domain.ErrConflict, requestID)
	}
	reply := domain.TurnReply{
		TenantID: tenantID, RequestID: requestID, ReplyRoute: replyTerminalRouteLabel,
		Sequence: sequence, Final: true,
	}
	if status == "completed" {
		reply.Payload = payload
	} else {
		reply.ErrorCode = terminalTurnErrorCode(status, payload)
	}
	return reply, nil
}

type cacheReplySubscription struct {
	hub       *CacheReplyHub
	tenantID  int64
	requestID string
	stream    string
	next      int64
	done      bool
	// recordReadAt throttles the durable fallback.
	recordReadAt time.Time
	wake         <-chan struct{}
	release      func()
}

var _ contract.TurnReplySubscription = (*cacheReplySubscription)(nil)

func (subscription *cacheReplySubscription) Route() string {
	return fmt.Sprintf("%s:%d:%s", cacheReplyRoutePrefix, subscription.tenantID, subscription.requestID)
}

// Receive returns the next frame in order. A frame the head has passed but the
// cache no longer holds is ErrReplayExpired: an unbroken prefix beats a silent gap.
func (subscription *cacheReplySubscription) Receive(ctx context.Context) (domain.TurnReply, error) {
	hub := subscription.hub
	period := hub.PollInterval
	if period <= 0 {
		period = defaultReplyPollPeriod
	}
	poll := time.NewTicker(period)
	defer poll.Stop()
	for {
		if subscription.done {
			return domain.TurnReply{}, io.EOF
		}
		reply, found, err := hub.frame(ctx, subscription.stream, subscription.next)
		if err != nil {
			return domain.TurnReply{}, err
		}
		if !found {
			if reply, found, err = subscription.missing(ctx); err != nil {
				return domain.TurnReply{}, err
			}
		}
		if found {
			subscription.next = reply.Sequence + 1
			subscription.done = reply.Final
			return reply, nil
		}
		select {
		case <-subscription.wake:
		case <-poll.C:
		case <-ctx.Done():
			return domain.TurnReply{}, ctx.Err()
		}
	}
}

// missing explains an absent frame: expired behind the head, not published yet,
// or already settled in the turn record, which answers for a stream the cache does not hold.
func (subscription *cacheReplySubscription) missing(ctx context.Context) (domain.TurnReply, bool, error) {
	hub := subscription.hub
	head, started, err := hub.head(ctx, subscription.stream)
	if err != nil {
		return domain.TurnReply{}, false, err
	}
	if started && head.Sequence >= subscription.next {
		// The head may have advanced between the two reads; only a frame still absent expired.
		if reply, found, err := hub.frame(ctx, subscription.stream, subscription.next); err != nil || found {
			return reply, found, err
		}
		return domain.TurnReply{}, false, fmt.Errorf("%w: reply sequence %d is no longer retained", domain.ErrReplayExpired, subscription.next)
	}
	// A running stream is answered by the cache alone; the record is read for one that is absent or final.
	if hub.Records == nil || started && !head.Final || time.Since(subscription.recordReadAt) < recordReadPeriod {
		return domain.TurnReply{}, false, nil
	}
	subscription.recordReadAt = time.Now()
	_, status, payload, err := hub.Records.Find(ctx, subscription.tenantID, subscription.requestID)
	if errors.Is(err, domain.ErrNotFound) || err == nil && !isTerminalTurnStatus(status) {
		return domain.TurnReply{}, false, nil
	}
	if err != nil {
		return domain.TurnReply{}, false, err
	}
	// Past the final frame of a settled turn there is nothing left; a suspended turn's stream continues.
	if started && head.Final {
		subscription.done = true
		return domain.TurnReply{}, false, nil
	}
	reply := domain.TurnReply{
		TenantID: subscription.tenantID, RequestID: subscription.requestID, ReplyRoute: replyTerminalRouteLabel,
		Sequence: subscription.next, Final: true,
	}
	if status == "completed" {
		reply.Payload = payload
	} else {
		reply.ErrorCode = terminalTurnErrorCode(status, payload)
	}
	return reply, true, nil
}

func terminalTurnErrorCode(status string, payload []byte) string {
	if len(payload) > 0 {
		return string(payload)
	}
	if status == "cancelled" {
		return "canceled"
	}
	return status
}

func (subscription *cacheReplySubscription) Close() error {
	subscription.done = true
	subscription.release()
	return nil
}
