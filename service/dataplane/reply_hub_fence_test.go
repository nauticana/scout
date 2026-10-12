package dataplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

func fencedFrame(sequence, fence int64, payload string, final bool) domain.TurnReply {
	return domain.TurnReply{TenantID: 7, RequestID: "request-1", ConversationID: "conv", Sequence: sequence, Payload: []byte(payload), Final: final, Fence: fence}
}

func replyHubs(t *testing.T) map[string]contract.TurnReplyPublisher {
	t.Helper()
	cached := &CacheReplyHub{Cache: cache.NewMemoryCacheService()}
	t.Cleanup(func() { _ = cached.Close() })
	return map[string]contract.TurnReplyPublisher{"memory": &MemoryReplyHub{}, "cache": cached}
}

// §8.12: once a held frame is published under the hold's fence, the fenced-out worker's next
// frame is refused, a duplicate of a frame it already published included; the released turn's
// next owner continues the stream at the held frame's sequence.
func TestReplyHubsRefuseAFrameBelowTheStreamsFence(t *testing.T) {
	for name, hub := range replyHubs(t) {
		ctx := context.Background()
		for _, frame := range []domain.TurnReply{fencedFrame(0, 1, "step-1", false), fencedFrame(1, 2, "", true)} {
			if err := hub.Publish(ctx, frame); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if err := hub.Publish(ctx, fencedFrame(1, 1, "step-2", false)); !errors.Is(err, domain.ErrFenceLost) {
			t.Fatalf("%s: a stale worker's next frame = %v, want ErrFenceLost", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(0, 1, "step-1", false)); !errors.Is(err, domain.ErrFenceLost) {
			t.Fatalf("%s: a stale worker's duplicate = %v, want ErrFenceLost", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(1, 2, "other", false)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s: the held fence itself may not replace its final frame, got %v", name, err)
		}
		// The next owner replays the frames it repeats as no-ops and continues at the held sequence.
		if err := hub.Publish(ctx, fencedFrame(0, 4, "step-1", false)); err != nil {
			t.Fatalf("%s: a replayed frame under the new fence = %v", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(1, 4, "step-2", false)); err != nil {
			t.Fatalf("%s: the released turn's next frame = %v", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(2, 4, "done", true)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(3, 2, "late", false)); !errors.Is(err, domain.ErrFenceLost) {
			t.Fatalf("%s: a frame of the held fence after release = %v, want ErrFenceLost", name, err)
		}
	}
}

// §8.16: an old and a new owner publishing the same sequence at once advance the stream's
// (fence, sequence) atomically: whichever reaches the head first, the new fence's frame is the
// one that stands, and the old owner cannot publish past it.
func TestReplyHubsAdvanceFenceAndSequenceAtomically(t *testing.T) {
	for attempt := range 50 {
		for name, hub := range replyHubs(t) {
			ctx := context.Background()
			if err := hub.Publish(ctx, fencedFrame(0, 1, "start", false)); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for index, frame := range []domain.TurnReply{fencedFrame(1, 1, "old", false), fencedFrame(1, 2, "new", false)} {
				wg.Go(func() { errs[index] = hub.Publish(ctx, frame) })
			}
			wg.Wait()
			oldErr, newErr := errs[0], errs[1]
			// Either the new owner published first and the old one was fenced out, or the old one
			// reached the head first and the new owner took the head over.
			if newErr != nil || oldErr != nil && !errors.Is(oldErr, domain.ErrFenceLost) {
				t.Fatalf("attempt %d, %s: old = %v, new = %v", attempt, name, oldErr, newErr)
			}
			subscription, err := hub.(contract.ReplayTurnReplySubscriber).SubscribeFrom(ctx, 7, "request-1", 1)
			if err != nil {
				t.Fatal(err)
			}
			stood, err := subscription.Receive(ctx)
			_ = subscription.Close()
			if err != nil || string(stood.Payload) != "new" || stood.Fence != 2 {
				t.Fatalf("attempt %d, %s: sequence 1 holds %q under fence %d, %v; want the new owner's frame", attempt, name, stood.Payload, stood.Fence, err)
			}
			if err := hub.Publish(ctx, fencedFrame(1, 2, "new", false)); err != nil {
				t.Fatalf("attempt %d, %s: the new owner's frame does not stand: %v", attempt, name, err)
			}
			if err := hub.Publish(ctx, fencedFrame(2, 1, "old again", false)); !errors.Is(err, domain.ErrFenceLost) {
				t.Fatalf("attempt %d, %s: old after new = %v", attempt, name, err)
			}
		}
	}
}

// A fenced-out worker's frame that raced in at the head is replaced by the new owner's frame at
// that sequence, so the new owner continues instead of failing on a conflict.
func TestReplyHubsLetANewOwnerTakeOverTheHead(t *testing.T) {
	for name, hub := range replyHubs(t) {
		ctx := context.Background()
		for _, frame := range []domain.TurnReply{fencedFrame(0, 1, "a", false), fencedFrame(1, 1, "stale", false)} {
			if err := hub.Publish(ctx, frame); err != nil {
				t.Fatal(err)
			}
		}
		if err := hub.Publish(ctx, fencedFrame(0, 2, "b", false)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s: a new owner rewrites only the head, got %v", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(1, 2, "current", false)); err != nil {
			t.Fatalf("%s: take over = %v", name, err)
		}
		if err := hub.Publish(ctx, fencedFrame(2, 2, "next", true)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A publisher that died between advancing the head and storing its frame leaves the head's
// frame absent. The same fence, here a redelivery's terminal replay, stores it again and the
// stream ends; before, it was refused as still being published and never ended.
func TestCacheReplyHubLetsTheSameFenceStoreTheHeadsMissingFrame(t *testing.T) {
	ctx := context.Background()
	shared := cache.NewMemoryCacheService()
	hub := &CacheReplyHub{Cache: shared, PollInterval: 5 * time.Millisecond}
	t.Cleanup(func() { _ = hub.Close() })
	for _, frame := range []domain.TurnReply{fencedFrame(0, 1, "step", false), fencedFrame(1, 1, "done", true)} {
		if err := hub.Publish(ctx, frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := shared.Delete(ctx, replyStreamKey(7, "request-1")+":1"); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, fencedFrame(1, 1, "done", true)); err != nil {
		t.Fatalf("republishing the head's missing frame under its own fence = %v", err)
	}
	subscription, err := hub.SubscribeFrom(ctx, 7, "request-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if reply, err := subscription.Receive(ctx); err != nil || !reply.Final || string(reply.Payload) != "done" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
}
