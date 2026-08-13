package dataplane

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func frame(seq int64, final bool) domain.TurnReply {
	return domain.TurnReply{TenantID: 1, RequestID: "r", Sequence: seq, Payload: []byte{byte(seq)}, Final: final}
}

func TestReplyHubDeliversInOrder(t *testing.T) {
	hub := &MemoryReplyHub{}
	ctx := context.Background()
	subscription, err := hub.Subscribe(ctx, 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if subscription.Route() == "" {
		t.Fatal("route must be set")
	}
	for seq := int64(0); seq < 3; seq++ {
		if err := hub.Publish(ctx, frame(seq, seq == 2)); err != nil {
			t.Fatal(err)
		}
	}
	for seq := int64(0); seq < 3; seq++ {
		reply, err := subscription.Receive(ctx)
		if err != nil || reply.Sequence != seq {
			t.Fatalf("frame %d = %+v, %v", seq, reply, err)
		}
		if reply.Final != (seq == 2) {
			t.Fatalf("final flag on %d", seq)
		}
	}
}

func TestReplyHubReplaysFromCursor(t *testing.T) {
	hub := &MemoryReplyHub{}
	ctx := context.Background()
	for seq := int64(0); seq < 5; seq++ {
		if err := hub.Publish(ctx, frame(seq, false)); err != nil {
			t.Fatal(err)
		}
	}
	subscription, err := hub.SubscribeFrom(ctx, 1, "r", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	for want := int64(3); want < 5; want++ {
		reply, err := subscription.Receive(ctx)
		if err != nil || reply.Sequence != want {
			t.Fatalf("replayed = %+v, %v", reply, err)
		}
	}
	// A frame published after replay continues the same stream.
	if err := hub.Publish(ctx, frame(5, true)); err != nil {
		t.Fatal(err)
	}
	if reply, err := subscription.Receive(ctx); err != nil || reply.Sequence != 5 {
		t.Fatalf("live = %+v, %v", reply, err)
	}
}

func TestReplyHubExpiredCursorAndOverflow(t *testing.T) {
	hub := &MemoryReplyHub{RetainedFrames: 2, SubscriberBuffer: 1}
	ctx := context.Background()
	for seq := int64(0); seq < 4; seq++ {
		if err := hub.Publish(ctx, frame(seq, false)); err != nil {
			t.Fatal(err)
		}
	}
	// Frames 0 and 1 are trimmed, so cursor 1 is unrecoverable.
	if _, err := hub.SubscribeFrom(ctx, 1, "r", 1); !errors.Is(err, domain.ErrReplayExpired) {
		t.Fatalf("expired cursor = %v", err)
	}
	if _, err := hub.Subscribe(ctx, 1, "r"); !errors.Is(err, domain.ErrReplayExpired) {
		t.Fatalf("uncursored replay = %v", err)
	}
	subscription, err := hub.SubscribeFrom(ctx, 1, "r", 3)
	if err != nil {
		t.Fatal(err)
	}
	// One replayed frame plus buffer 1 leaves room for 1 unread live frame; the next overflows.
	if err := hub.Publish(ctx, frame(4, false)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, frame(5, false)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, frame(6, false)); err != nil {
		t.Fatal(err)
	}
	var lastErr error
	for range 4 {
		if _, lastErr = subscription.Receive(ctx); lastErr != nil {
			break
		}
	}
	if !errors.Is(lastErr, domain.ErrReplayExpired) {
		t.Fatalf("overflow = %v", lastErr)
	}
}

func TestReplyHubExpiresCompletedStreams(t *testing.T) {
	now := time.Unix(0, 0)
	hub := &MemoryReplyHub{Linger: time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()
	subscription, err := hub.Subscribe(ctx, 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, frame(0, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	// After linger, the sweep closes the stream; the subscriber sees EOF.
	now = now.Add(3 * time.Second)
	if err := hub.Publish(ctx, domain.TurnReply{TenantID: 1, RequestID: "other", Sequence: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("after expiry = %v", err)
	}
	if len(hub.streams) != 1 {
		t.Fatalf("streams = %d", len(hub.streams))
	}
}

func TestReplyHubBoundsStreamsAndValidates(t *testing.T) {
	hub := &MemoryReplyHub{MaxStreams: 1}
	ctx := context.Background()
	if _, err := hub.Subscribe(ctx, 1, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Subscribe(ctx, 1, "r2"); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("stream cap = %v", err)
	}
	if err := hub.Publish(ctx, domain.TurnReply{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
	if _, err := hub.SubscribeFrom(ctx, 0, "", -1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
}

func TestReplyHubRejectsSequenceGapsAndFramesAfterFinal(t *testing.T) {
	hub := &MemoryReplyHub{}
	ctx := context.Background()
	if err := hub.Publish(ctx, frame(1, false)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("gap = %v", err)
	}
	if err := hub.Publish(ctx, frame(0, true)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, frame(1, false)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("after final = %v", err)
	}
}

func TestReplyHubAcceptsOnlyMatchingRetainedDuplicates(t *testing.T) {
	hub := &MemoryReplyHub{RetainedFrames: 2}
	ctx := context.Background()
	subscription, err := hub.Subscribe(ctx, 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	original := frame(0, true)
	original.ConversationID = "c"
	original.ReplyRoute = "route"
	original.AgentVersion = "v1"
	original.EmittedAt = time.Now()
	if err := hub.Publish(ctx, original); err != nil {
		t.Fatal(err)
	}
	duplicate := original
	duplicate.EmittedAt = original.EmittedAt.Add(time.Second)
	if err := hub.Publish(ctx, duplicate); err != nil {
		t.Fatalf("duplicate = %v", err)
	}
	if queued := len(subscription.(*replySubscription).frames); queued != 1 {
		t.Fatalf("duplicate fan-out queued %d frames", queued)
	}
	changed := duplicate
	changed.Payload = []byte("different")
	if err := hub.Publish(ctx, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed duplicate = %v", err)
	}
}

func TestReplyHubCannotVerifyTrimmedDuplicate(t *testing.T) {
	hub := &MemoryReplyHub{RetainedFrames: 1}
	ctx := context.Background()
	if err := hub.Publish(ctx, frame(0, false)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, frame(1, false)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, frame(0, false)); !errors.Is(err, domain.ErrReplayExpired) {
		t.Fatalf("trimmed duplicate = %v", err)
	}
}
