package dataplane

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"
	keelschema "github.com/nauticana/keel/schema"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/dataplane/dataplanetest"
)

// keel's memory cache reads its sweep interval from configuration.
func TestMain(m *testing.M) {
	keelschema.LoadTestConfig()
	m.Run()
}

func replyFrame(sequence int64, payload string, final bool) domain.TurnReply {
	return domain.TurnReply{TenantID: 7, RequestID: "request-1", ConversationID: "conv", Sequence: sequence, Payload: []byte(payload), Final: final}
}

func TestCacheReplyHubConformance(t *testing.T) {
	dataplanetest.RunReplySuite(t, func(t *testing.T) dataplanetest.ReplyHarness {
		hub := &CacheReplyHub{Cache: cache.NewMemoryCacheService(), PollInterval: 10 * time.Millisecond}
		return dataplanetest.ReplyHarness{Publisher: hub, Subscriber: hub, Close: func() { _ = hub.Close() }}
	})
}

// Two hubs over one cache stand for a worker process and an ingress process.
func TestCacheReplyHubDeliversAcrossProcessesInOrderAndResumes(t *testing.T) {
	shared := cache.NewMemoryCacheService()
	worker := &CacheReplyHub{Cache: shared}
	ingress := &CacheReplyHub{Cache: shared, PollInterval: 10 * time.Millisecond}
	defer ingress.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subscription, err := ingress.Subscribe(ctx, 7, "request-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	received := make(chan domain.TurnReply, 3)
	go func() {
		for {
			frame, err := subscription.Receive(ctx)
			if err != nil {
				close(received)
				return
			}
			received <- frame
		}
	}()
	for sequence, payload := range []string{"a", "b", "c"} {
		if err = worker.Publish(ctx, replyFrame(int64(sequence), payload, sequence == 2)); err != nil {
			t.Fatalf("Publish %d: %v", sequence, err)
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		if frame := <-received; string(frame.Payload) != want {
			t.Fatalf("frame = %q, want %q", frame.Payload, want)
		}
	}
	if _, open := <-received; open {
		t.Fatal("a delivered final frame ends the subscription")
	}

	// A reconnect on any process resumes from its cursor.
	resumed, err := (&CacheReplyHub{Cache: shared}).SubscribeFrom(ctx, 7, "request-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if frame, err := resumed.Receive(ctx); err != nil || string(frame.Payload) != "c" || !frame.Final {
		t.Fatalf("resumed frame = %+v, %v", frame, err)
	}
	if _, err = resumed.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF after the final frame, got %v", err)
	}
}

func TestCacheReplyHubPublishIsIdempotentAndOrdered(t *testing.T) {
	hub := &CacheReplyHub{Cache: cache.NewMemoryCacheService()}
	ctx := context.Background()
	if err := hub.Publish(ctx, replyFrame(0, "a", false)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, replyFrame(0, "a", false)); err != nil {
		t.Fatalf("a redelivered frame must be a no-op, got %v", err)
	}
	if err := hub.Publish(ctx, replyFrame(0, "changed", false)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for changed content, got %v", err)
	}
	if err := hub.Publish(ctx, replyFrame(2, "gap", false)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for a gap, got %v", err)
	}
	// A suspended turn ends its delivery with a final frame and resumes at the same sequence.
	if err := hub.Publish(ctx, replyFrame(1, "", true)); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(ctx, replyFrame(1, "resumed", false)); err != nil {
		t.Fatalf("a resumed turn must supersede its suspension frame, got %v", err)
	}
	if err := hub.Publish(ctx, replyFrame(2, "done", true)); err != nil {
		t.Fatal(err)
	}
}

func TestCacheReplyHubNeverHidesAGapAndFallsBackToTheTurnRecord(t *testing.T) {
	shared := cache.NewMemoryCacheService()
	hub := &CacheReplyHub{Cache: shared, PollInterval: 10 * time.Millisecond}
	defer hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for sequence := range int64(2) {
		if err := hub.Publish(ctx, replyFrame(sequence, "x", false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := shared.Delete(ctx, replyStreamKey(7, "request-1")+":0"); err != nil {
		t.Fatal(err)
	}
	expired, _ := hub.SubscribeFrom(ctx, 7, "request-1", 0)
	if _, err := expired.Receive(ctx); !errors.Is(err, domain.ErrReplayExpired) {
		t.Fatalf("want ErrReplayExpired for an expired frame, got %v", err)
	}

	// The cache holds nothing for this request, but the turn already completed.
	hub.Records = &fake.TurnRecordStore{FindFunc: func(_ context.Context, _ int64, requestID string) (int64, string, []byte, error) {
		if requestID == "failed" {
			return 1, "failed", []byte("budget_exceeded"), nil
		}
		return 1, "completed", []byte("answer"), nil
	}}
	settled, _ := hub.Subscribe(ctx, 7, "settled")
	if frame, err := settled.Receive(ctx); err != nil || !frame.Final || string(frame.Payload) != "answer" {
		t.Fatalf("settled frame = %+v, %v", frame, err)
	}
	failed, _ := hub.Subscribe(ctx, 7, "failed")
	if frame, err := failed.Receive(ctx); err != nil || !frame.Final || frame.ErrorCode != "budget_exceeded" || frame.Payload != nil {
		t.Fatalf("failed frame = %+v, %v", frame, err)
	}
	stored, err := hub.StoredReply(ctx, 7, "settled", 9)
	if err != nil || stored.Sequence != 9 || !stored.Final || string(stored.Payload) != "answer" {
		t.Fatalf("stored frame = %+v, %v", stored, err)
	}
	stored, err = hub.StoredReply(ctx, 7, "failed", 10)
	if err != nil || stored.Sequence != 10 || stored.ErrorCode != "budget_exceeded" {
		t.Fatalf("stored failed frame = %+v, %v", stored, err)
	}
	hub.Records = &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) {
		return 1, "cancelled", nil, nil
	}}
	stored, err = hub.StoredReply(ctx, 7, "cancelled", 11)
	if err != nil || stored.ErrorCode != "canceled" {
		t.Fatalf("stored canceled frame = %+v, %v", stored, err)
	}
}
