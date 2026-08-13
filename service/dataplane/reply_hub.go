package dataplane

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemoryReplyHub is an in-process reply broker: bounded per-subscriber queues,
// a replay ring per request for reconnect, and disconnect-on-overflow so a
// slow reader gets a clean domain.ErrReplayExpired instead of silent gaps.
type MemoryReplyHub struct {
	// SubscriberBuffer bounds each subscriber queue; default 16.
	SubscriberBuffer int
	// RetainedFrames bounds the per-request replay ring; default 64.
	RetainedFrames int
	// Linger keeps a completed stream for reconnect; default 30s.
	Linger time.Duration
	// IdleTTL expires streams that never complete; default 10m.
	IdleTTL time.Duration
	// MaxStreams bounds tracked requests; default 4096.
	MaxStreams int
	Now        func() time.Time

	mu        sync.Mutex
	streams   map[streamKey]*replyStream
	nextSweep time.Time
}

var _ contract.TurnReplyPublisher = (*MemoryReplyHub)(nil)
var _ contract.TurnReplySubscriber = (*MemoryReplyHub)(nil)
var _ contract.ReplayTurnReplySubscriber = (*MemoryReplyHub)(nil)

type streamKey struct {
	tenantID  int64
	requestID string
}

type replyStream struct {
	frames       []domain.TurnReply
	minRetained  int64
	nextSequence int64
	trimmed      bool
	subscribers  map[*replySubscription]struct{}
	finalAt      time.Time
	activeAt     time.Time
}

type replySubscription struct {
	hub    *MemoryReplyHub
	key    streamKey
	route  string
	frames chan domain.TurnReply
	err    error
	closed bool
}

func (hub *MemoryReplyHub) defaults() (int, int, time.Duration, time.Duration, int, func() time.Time) {
	buffer, retained, linger, idle, streams, now := hub.SubscriberBuffer, hub.RetainedFrames, hub.Linger, hub.IdleTTL, hub.MaxStreams, hub.Now
	if buffer <= 0 {
		buffer = 16
	}
	if retained <= 0 {
		retained = 64
	}
	if linger <= 0 {
		linger = 30 * time.Second
	}
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	if streams <= 0 {
		streams = 4096
	}
	if now == nil {
		now = time.Now
	}
	return buffer, retained, linger, idle, streams, now
}

// Publish retains and fans out a frame; matching retained duplicates are no-ops.
func (hub *MemoryReplyHub) Publish(ctx context.Context, reply domain.TurnReply) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reply.TenantID <= 0 || strings.TrimSpace(reply.RequestID) == "" || reply.Sequence < 0 {
		return fmt.Errorf("%w: tenant, request, and sequence are required", domain.ErrValidation)
	}
	_, retained, _, _, maxStreams, nowFn := hub.defaults()
	now := nowFn()
	key := streamKey{reply.TenantID, reply.RequestID}

	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.sweepLocked(now)
	stream := hub.streams[key]
	if stream == nil {
		var err error
		if stream, err = hub.openLocked(key, maxStreams); err != nil {
			return err
		}
	}
	stream.activeAt = now
	if reply.Sequence < stream.nextSequence {
		retained, found := retainedReply(stream.frames, reply.Sequence)
		if !found {
			return fmt.Errorf("%w: reply sequence %d is no longer retained", domain.ErrReplayExpired, reply.Sequence)
		}
		if !sameReply(retained, reply) {
			return fmt.Errorf("%w: reply sequence %d has different content", domain.ErrConflict, reply.Sequence)
		}
		return nil
	}
	if reply.Sequence > stream.nextSequence {
		return fmt.Errorf("%w: reply sequence %d, want %d", domain.ErrConflict, reply.Sequence, stream.nextSequence)
	}
	if !stream.finalAt.IsZero() {
		return fmt.Errorf("%w: reply stream is already final", domain.ErrConflict)
	}
	stream.nextSequence++
	stream.frames = append(stream.frames, reply)
	if len(stream.frames) > retained {
		stream.trimmed = true
		stream.minRetained = stream.frames[1].Sequence
		stream.frames = append(stream.frames[:0], stream.frames[1:]...)
	}
	for subscription := range stream.subscribers {
		select {
		case subscription.frames <- reply:
		default:
			// Overflow disconnects: an unbroken prefix beats a silent gap.
			hub.dropLocked(stream, subscription, domain.ErrReplayExpired)
		}
	}
	if reply.Final {
		stream.finalAt = now
	}
	return nil
}

func retainedReply(frames []domain.TurnReply, sequence int64) (domain.TurnReply, bool) {
	for _, frame := range frames {
		if frame.Sequence == sequence {
			return frame, true
		}
	}
	return domain.TurnReply{}, false
}

func sameReply(left, right domain.TurnReply) bool {
	return left.TenantID == right.TenantID && left.RequestID == right.RequestID &&
		left.ConversationID == right.ConversationID && left.ReplyRoute == right.ReplyRoute &&
		left.Sequence == right.Sequence && bytes.Equal(left.Payload, right.Payload) &&
		left.Final == right.Final && left.ErrorCode == right.ErrorCode && left.AgentVersion == right.AgentVersion
}

// Subscribe opens one reply stream without a replay cursor.
func (hub *MemoryReplyHub) Subscribe(ctx context.Context, tenantID int64, requestID string) (contract.TurnReplySubscription, error) {
	return hub.SubscribeFrom(ctx, tenantID, requestID, 0)
}

// SubscribeFrom opens one reply stream, replaying retained frames from fromSequence.
func (hub *MemoryReplyHub) SubscribeFrom(ctx context.Context, tenantID int64, requestID string, fromSequence int64) (contract.TurnReplySubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || fromSequence < 0 {
		return nil, fmt.Errorf("%w: tenant, request, and cursor are required", domain.ErrValidation)
	}
	buffer, _, _, _, maxStreams, nowFn := hub.defaults()
	now := nowFn()
	key := streamKey{tenantID, requestID}

	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.sweepLocked(now)
	stream := hub.streams[key]
	if stream == nil {
		var err error
		if stream, err = hub.openLocked(key, maxStreams); err != nil {
			return nil, err
		}
	}
	stream.activeAt = now
	if stream.trimmed && fromSequence < stream.minRetained {
		return nil, fmt.Errorf("%w: cursor %d precedes retained %d", domain.ErrReplayExpired, fromSequence, stream.minRetained)
	}
	var replay []domain.TurnReply
	for _, frame := range stream.frames {
		if frame.Sequence >= fromSequence {
			replay = append(replay, frame)
		}
	}
	// Replay and registration share the lock, so no live frame slips between them.
	subscription := &replySubscription{
		hub:    hub,
		key:    key,
		route:  fmt.Sprintf("memreply:%d:%s", tenantID, requestID),
		frames: make(chan domain.TurnReply, buffer+len(replay)),
	}
	for _, frame := range replay {
		subscription.frames <- frame
	}
	stream.subscribers[subscription] = struct{}{}
	return subscription, nil
}

func (hub *MemoryReplyHub) openLocked(key streamKey, maxStreams int) (*replyStream, error) {
	if hub.streams == nil {
		hub.streams = make(map[streamKey]*replyStream)
	}
	if len(hub.streams) >= maxStreams {
		return nil, fmt.Errorf("%w: reply stream capacity reached", domain.ErrRateLimited)
	}
	stream := &replyStream{subscribers: make(map[*replySubscription]struct{})}
	hub.streams[key] = stream
	return stream, nil
}

// dropLocked detaches one subscriber, recording why its channel closed.
func (hub *MemoryReplyHub) dropLocked(stream *replyStream, subscription *replySubscription, cause error) {
	if subscription.closed {
		return
	}
	subscription.closed = true
	subscription.err = cause
	delete(stream.subscribers, subscription)
	close(subscription.frames)
}

// sweepLocked expires completed and idle streams at most once per second.
func (hub *MemoryReplyHub) sweepLocked(now time.Time) {
	if now.Before(hub.nextSweep) {
		return
	}
	hub.nextSweep = now.Add(time.Second)
	_, _, linger, idle, _, _ := hub.defaults()
	for key, stream := range hub.streams {
		expired := (!stream.finalAt.IsZero() && now.Sub(stream.finalAt) > linger) ||
			(stream.finalAt.IsZero() && now.Sub(stream.activeAt) > idle)
		if !expired {
			continue
		}
		for subscription := range stream.subscribers {
			hub.dropLocked(stream, subscription, io.EOF)
		}
		delete(hub.streams, key)
	}
}

// Route returns the opaque destination workers publish to.
func (subscription *replySubscription) Route() string { return subscription.route }

// Receive waits for the next reply frame.
func (subscription *replySubscription) Receive(ctx context.Context) (domain.TurnReply, error) {
	select {
	case frame, open := <-subscription.frames:
		if !open {
			subscription.hub.mu.Lock()
			cause := subscription.err
			subscription.hub.mu.Unlock()
			if cause == nil {
				cause = io.EOF
			}
			return domain.TurnReply{}, cause
		}
		return frame, nil
	case <-ctx.Done():
		return domain.TurnReply{}, ctx.Err()
	}
}

// Close releases the subscription.
func (subscription *replySubscription) Close() error {
	hub := subscription.hub
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if stream := hub.streams[subscription.key]; stream != nil {
		hub.dropLocked(stream, subscription, io.EOF)
	} else if !subscription.closed {
		subscription.closed = true
		close(subscription.frames)
	}
	return nil
}

var _ contract.TurnReplySubscription = (*replySubscription)(nil)
