package dataplane

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestReplyHubReceiveBlocksUntilCancel(t *testing.T) {
	hub := &MemoryReplyHub{}
	defer hub.Close()
	subscription, err := hub.Subscribe(context.Background(), 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, receiveErr := subscription.Receive(ctx)
		done <- receiveErr
	}()
	select {
	case err = <-done:
		t.Fatalf("returned before cancel: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}

func TestReplyHubOneSubscriberFailsEarly(t *testing.T) {
	hub := &MemoryReplyHub{SubscriberBuffer: 1, RetainedFrames: 8}
	defer hub.Close()
	ctx := context.Background()
	slow, err := hub.Subscribe(ctx, 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	fast, err := hub.Subscribe(ctx, 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	// The slow subscriber never drains and overflows; the fast one drains each frame and survives.
	for seq := int64(0); seq < 3; seq++ {
		if err = hub.Publish(ctx, frame(seq, false)); err != nil {
			t.Fatal(err)
		}
		reply, receiveErr := fast.Receive(ctx)
		if receiveErr != nil || reply.Sequence != seq {
			t.Fatalf("fast frame %d = %+v, %v", seq, reply, receiveErr)
		}
	}
	for {
		if _, err = slow.Receive(ctx); err != nil {
			break
		}
	}
	if !errors.Is(err, domain.ErrReplayExpired) {
		t.Fatalf("slow subscriber = %v", err)
	}
	if err = hub.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = fast.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("fast subscriber close = %v", err)
	}
}

func TestReplyHubRejectsEmptyAndInvalidConfig(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		run     func() error
		wantErr error
	}{
		{name: "publish without tenant", run: func() error {
			return (&MemoryReplyHub{}).Publish(ctx, domain.TurnReply{RequestID: "r"})
		}, wantErr: domain.ErrValidation},
		{name: "publish without request", run: func() error {
			return (&MemoryReplyHub{}).Publish(ctx, domain.TurnReply{TenantID: 1, RequestID: "  "})
		}, wantErr: domain.ErrValidation},
		{name: "subscribe with negative cursor", run: func() error {
			_, err := (&MemoryReplyHub{}).SubscribeFrom(ctx, 1, "r", -1)
			return err
		}, wantErr: domain.ErrValidation},
		{name: "negative subscriber buffer", run: func() error {
			_, err := (&MemoryReplyHub{SubscriberBuffer: -1}).Subscribe(ctx, 1, "r")
			return err
		}},
		{name: "negative retained frames", run: func() error {
			return (&MemoryReplyHub{RetainedFrames: -1}).Publish(ctx, frame(0, false))
		}},
		{name: "negative max streams", run: func() error {
			_, err := (&MemoryReplyHub{MaxStreams: -1}).Subscribe(ctx, 1, "r")
			return err
		}},
		{name: "negative idle ttl", run: func() error {
			_, err := (&MemoryReplyHub{IdleTTL: -time.Second}).Subscribe(ctx, 1, "r")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("expected rejection")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReplyHubCloseIsIdempotentAndReleasesReceivers(t *testing.T) {
	hub := &MemoryReplyHub{}
	ctx := context.Background()
	const receivers = 4
	var wg sync.WaitGroup
	failures := make(chan error, receivers)
	for i := range receivers {
		subscription, err := hub.Subscribe(ctx, 1, "r")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, receiveErr := subscription.Receive(ctx); !errors.Is(receiveErr, io.EOF) {
				failures <- receiveErr
			}
			// Closing an already-detached subscription must not panic or block.
			if closeErr := subscription.Close(); closeErr != nil {
				failures <- closeErr
			}
			_ = i
		}()
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}

	settled := make(chan struct{})
	go func() { wg.Wait(); close(settled) }()
	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("receivers did not return after close")
	}
	close(failures)
	for err := range failures {
		t.Fatalf("receiver = %v", err)
	}
	if err := hub.Publish(ctx, frame(0, false)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("publish after close = %v", err)
	}
	if _, err := hub.Subscribe(ctx, 1, "r"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("subscribe after close = %v", err)
	}
}
