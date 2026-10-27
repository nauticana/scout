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
	"github.com/nauticana/scout/internal/stage"
)

const (
	replyChannel            = "scout:reply"
	defaultReplyRetention   = 10 * time.Minute
	defaultReplyPollPeriod  = 500 * time.Millisecond
	cacheReplyRoutePrefix   = "cachereply"
	replyHeadSuffix         = "head"
	replyTerminalRouteLabel = "record"
	defaultRecordReadPeriod = 5 * time.Second
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
	// RecordReadInterval throttles a subscriber's durable record reads; default 5s.
	RecordReadInterval time.Duration

	once   sync.Once
	signal cacheSignal
}

var (
	_ contract.TurnReplyPublisher        = (*CacheReplyHub)(nil)
	_ contract.TurnReplySubscriber       = (*CacheReplyHub)(nil)
	_ contract.ReplayTurnReplySubscriber = (*CacheReplyHub)(nil)
	_ contract.StoredTurnReplyReader     = (*CacheReplyHub)(nil)
)

// replyHead is the stream's newest (fence, sequence); it advances only by compare-and-set.
type replyHead struct {
	Sequence int64 `json:"seq"`
	Final    bool  `json:"final"`
	Fence    int64 `json:"fence"`
}

func replyStreamKey(tenantID int64, requestID string) string {
	return fmt.Sprintf("%s:%d:%s", replyChannel, tenantID, requestID)
}

// check refuses a hub that cannot deliver: no cache, or a negative period.
func (hub *CacheReplyHub) check() error {
	if hub.Cache == nil {
		return fmt.Errorf("cache reply hub: cache is required")
	}
	if hub.Retention < 0 || hub.PollInterval < 0 || hub.RecordReadInterval < 0 {
		return fmt.Errorf("cache reply hub: retention, poll interval, and record read interval must not be negative")
	}
	return nil
}

func (hub *CacheReplyHub) retention() time.Duration {
	return positiveOr(hub.Retention, defaultReplyRetention)
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
	head, raw, err := hub.rawHead(ctx, stream)
	return head, raw != "", err
}

// rawHead also returns the stored encoding, which a compare-and-set expects; "" is no head.
func (hub *CacheReplyHub) rawHead(ctx context.Context, stream string) (replyHead, string, error) {
	raw, err := hub.Cache.Get(ctx, stream+":"+replyHeadSuffix)
	if errors.Is(err, cache.ErrCacheMiss) {
		return replyHead{}, "", nil
	}
	if err != nil {
		return replyHead{}, "", fmt.Errorf("read reply head: %w", err)
	}
	var head replyHead
	if err = json.Unmarshal([]byte(raw), &head); err != nil {
		return replyHead{}, "", fmt.Errorf("decode reply head: %w", err)
	}
	return head, raw, nil
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

// Publish advances the head by compare-and-set, then stores the frame and wakes subscribers.
// The head is the linearization point: a frame below its fence is ErrFenceLost, so a fenced-out
// worker can neither write a frame nor move the head, and only a higher fence may replace the
// frame at the head, where a resumed turn continues.
func (hub *CacheReplyHub) Publish(ctx context.Context, reply domain.TurnReply) error {
	if err := hub.check(); err != nil {
		return err
	}
	if reply.TenantID <= 0 || strings.TrimSpace(reply.RequestID) == "" || reply.Sequence < 0 || reply.Fence < 0 {
		return fmt.Errorf("%w: tenant, request, sequence, and a non-negative fence are required", domain.ErrValidation)
	}
	stream := replyStreamKey(reply.TenantID, reply.RequestID)
	encoded, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("encode reply frame: %w", err)
	}
	// A lost compare-and-set means another publisher moved the head; deciding once more against
	// the new head either finds this frame published, refuses it, or advances past it.
	advanced := false
	for attempt := 0; attempt < 2 && !advanced; attempt++ {
		head, raw, err := hub.rawHead(ctx, stream)
		if err != nil {
			return err
		}
		publish, err := hub.admit(ctx, stream, head, raw != "", reply)
		if err != nil || !publish {
			return err
		}
		encodedHead, _ := json.Marshal(replyHead{Sequence: reply.Sequence, Final: reply.Final, Fence: reply.Fence})
		if advanced, err = hub.Cache.CompareAndSet(ctx, stream+":"+replyHeadSuffix, raw, string(encodedHead), hub.retention()); err != nil {
			return fmt.Errorf("advance reply head: %w", err)
		}
	}
	if !advanced {
		return fmt.Errorf("%w: reply head of request %q moved concurrently", domain.ErrConflict, reply.RequestID)
	}
	if err = hub.storeFrame(ctx, fmt.Sprintf("%s:%d", stream, reply.Sequence), string(encoded), reply.Fence); err != nil {
		return err
	}
	if err = hub.relay().notify(ctx, stream); err != nil {
		return fmt.Errorf("wake reply subscribers: %w", err)
	}
	return nil
}

// storeFrame writes the frame by compare-and-set too: a publisher whose head advance was
// overtaken by a higher fence before its frame landed finds that fence's frame and stores nothing.
func (hub *CacheReplyHub) storeFrame(ctx context.Context, key, encoded string, fence int64) error {
	for range 2 {
		raw, err := hub.Cache.Get(ctx, key)
		if errors.Is(err, cache.ErrCacheMiss) {
			raw, err = "", nil
		}
		if err != nil {
			return fmt.Errorf("read reply frame: %w", err)
		}
		if raw != "" {
			var stored domain.TurnReply
			if err = json.Unmarshal([]byte(raw), &stored); err != nil {
				return fmt.Errorf("decode reply frame: %w", err)
			}
			if stored.Fence > fence {
				return fmt.Errorf("%w: reply fence %d was overtaken by %d", domain.ErrFenceLost, fence, stored.Fence)
			}
		}
		stored, err := hub.Cache.CompareAndSet(ctx, key, raw, encoded, hub.retention())
		if err != nil {
			return fmt.Errorf("store reply frame: %w", err)
		}
		if stored {
			return nil
		}
	}
	return fmt.Errorf("%w: reply frame %s was written concurrently", domain.ErrConflict, key)
}

// admit decides a frame against the head: publish it, accept it as an already published
// duplicate, or refuse it.
func (hub *CacheReplyHub) admit(ctx context.Context, stream string, head replyHead, started bool, reply domain.TurnReply) (bool, error) {
	switch {
	// A stream the cache no longer holds restarts at any sequence.
	case !started:
		return true, nil
	case reply.Fence < head.Fence:
		return false, fmt.Errorf("%w: reply fence %d is below the stream's %d", domain.ErrFenceLost, reply.Fence, head.Fence)
	// A higher fence takes over the stream at its head: a resumed turn continues where a final
	// frame ended a delivery, and a new owner's frame stands over one a fenced-out worker raced in,
	// whose frame may not even be stored yet.
	case reply.Sequence == head.Sequence && reply.Fence > head.Fence:
		return true, nil
	case reply.Sequence <= head.Sequence:
		retained, found, err := hub.frame(ctx, stream, reply.Sequence)
		switch {
		case err != nil:
			return false, err
		// The head's frame is absent: its publisher is still storing it, or died first. The same
		// fence, a redelivery's terminal replay among others, stores it again; a lower fence was
		// refused above and a higher one took the head over.
		case !found && reply.Sequence == head.Sequence:
			return true, nil
		case !found:
			return false, fmt.Errorf("%w: reply sequence %d is no longer retained", domain.ErrReplayExpired, reply.Sequence)
		case sameReply(retained, reply):
			return false, nil
		}
		return false, fmt.Errorf("%w: reply sequence %d has different content", domain.ErrConflict, reply.Sequence)
	case reply.Sequence > head.Sequence+1:
		return false, fmt.Errorf("%w: reply sequence %d, want %d", domain.ErrConflict, reply.Sequence, head.Sequence+1)
	}
	return true, nil
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
	if err := hub.check(); err != nil {
		return nil, err
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
	record, err := hub.Records.Find(ctx, tenantID, requestID)
	if err != nil {
		return domain.TurnReply{}, err
	}
	if !record.Status.Terminal() && !heldStatus(record.Status) {
		return domain.TurnReply{}, fmt.Errorf("%w: turn %q is not terminal", domain.ErrConflict, requestID)
	}
	return storedReply(tenantID, requestID, sequence, record), nil
}

// storedReply rebuilds the final frame of a terminal or held turn from its durable record.
func storedReply(tenantID int64, requestID string, sequence int64, record domain.TurnRecord) domain.TurnReply {
	reply := domain.TurnReply{
		TenantID: tenantID, RequestID: requestID, ReplyRoute: replyTerminalRouteLabel,
		Sequence: sequence, Final: true, Fence: record.Fence,
	}
	switch {
	case record.Status == domain.TurnCompleted:
		reply.Payload = record.Payload
	case heldStatus(record.Status):
		reply.ErrorCode, reply.Events = heldFrame()
	default:
		reply.ErrorCode = terminalTurnErrorCode(record.Status, record.Payload)
	}
	return reply
}

// heldStatus reports a turn no worker runs until it is released, handed back, or finished.
func heldStatus(status domain.TurnStatus) bool {
	return status == domain.TurnHeld || status == domain.TurnOperated
}

// heldFrame is the error code and event of the frame a held delivery ends on.
func heldFrame() (string, []domain.TurnEvent) {
	return stage.ErrorClass(domain.ErrTurnHeld), []domain.TurnEvent{{Version: domain.TurnEventVersion, Kind: domain.TurnEventHeld}}
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
	poll := time.NewTicker(positiveOr(hub.PollInterval, defaultReplyPollPeriod))
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
		// The head's own frame is stored right after the head advances.
		if head.Sequence == subscription.next {
			return domain.TurnReply{}, false, nil
		}
		return domain.TurnReply{}, false, fmt.Errorf("%w: reply sequence %d is no longer retained", domain.ErrReplayExpired, subscription.next)
	}
	// A running stream is answered by the cache alone; the record is read for one that is absent or final.
	if hub.Records == nil || started && !head.Final || time.Since(subscription.recordReadAt) < positiveOr(hub.RecordReadInterval, defaultRecordReadPeriod) {
		return domain.TurnReply{}, false, nil
	}
	subscription.recordReadAt = time.Now()
	record, err := hub.Records.Find(ctx, subscription.tenantID, subscription.requestID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.TurnReply{}, false, nil
	}
	if err != nil {
		return domain.TurnReply{}, false, err
	}
	// A turn held while the cache holds none of its stream ends this delivery on a held frame
	// rebuilt from the record; a held turn whose final frame was published already said so.
	if !record.Status.Terminal() && (!heldStatus(record.Status) || started) {
		return domain.TurnReply{}, false, nil
	}
	// Past the final frame of a settled turn there is nothing left; a suspended turn's stream continues.
	if started && head.Final {
		subscription.done = true
		return domain.TurnReply{}, false, nil
	}
	return storedReply(subscription.tenantID, subscription.requestID, subscription.next, record), true, nil
}

func terminalTurnErrorCode(status domain.TurnStatus, payload []byte) string {
	if len(payload) > 0 {
		return string(payload)
	}
	return terminalTurnFallbackCode(status)
}

func (subscription *cacheReplySubscription) Close() error {
	subscription.done = true
	subscription.release()
	return nil
}
