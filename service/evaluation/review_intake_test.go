package evaluation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type samplerFunc func(context.Context, domain.SampleSignal) (string, error)

func (function samplerFunc) Sample(ctx context.Context, signal domain.SampleSignal) (string, error) {
	return function(ctx, signal)
}

func reviewedTurn() domain.ReviewTurn {
	return domain.ReviewTurn{TenantContext: domain.TenantContext{TenantID: 7, Region: "eu"}, RequestID: "req-1", AgentID: "writer-a", AgentVersion: "3"}
}

func TestReviewIntakeQueuesSampledTurnsUnderTheSamplerPolicy(t *testing.T) {
	ctx := context.Background()
	queue := &fake.HumanReviewQueue{}
	policies := &fake.SamplingPolicyRepository{Policy: domain.SamplingPolicy{BaseRate: 1, MaxPerWindow: 1, Window: time.Hour}}
	intake := &ReviewIntake{Sampler: &SamplingPolicyEnforcer{Policies: policies, Draw: func() float64 { return 0 }}, Policies: policies, Queue: queue}

	if err := intake.TurnCompleted(ctx, reviewedTurn()); err != nil {
		t.Fatal(err)
	}
	if err := intake.TurnCompleted(ctx, reviewedTurn()); err != nil {
		t.Fatal(err)
	}
	items := queue.Enqueued()
	if len(items) != 1 {
		t.Fatalf("the window cap admitted %d samples, want 1", len(items))
	}
	if item := items[0]; item.Source != domain.ReviewSourceSampled || item.TenantID != 7 || item.Reason != "sampled: base" ||
		item.Turn != (domain.ReviewTurnRef{RequestID: "req-1"}) || item.AgentID != "" {
		t.Fatalf("sampled item = %+v", item)
	}

	failing := &ReviewIntake{Sampler: samplerFunc(func(context.Context, domain.SampleSignal) (string, error) { return "", errors.New("policy store down") }), Queue: queue}
	if err := failing.TurnCompleted(ctx, reviewedTurn()); err == nil {
		t.Fatal("a sampler failure was swallowed")
	}
	if err := intake.TurnCompleted(ctx, domain.ReviewTurn{RequestID: "req-1", AgentID: "writer-a"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("turn without tenant = %v", err)
	}
	if err := (&ReviewIntake{Queue: queue}).TurnCompleted(ctx, reviewedTurn()); err == nil {
		t.Fatal("an intake without a sampler accepted a turn")
	}
}

func TestReviewIntakeQueuesLowRatingsWithoutTheRater(t *testing.T) {
	ctx := context.Background()
	queue := &fake.HumanReviewQueue{}
	policies := &fake.SamplingPolicyRepository{}
	intake := &ReviewIntake{Policies: policies, Queue: queue, FeedbackThreshold: 2}

	for _, rating := range []int{5, 3} {
		if err := intake.TurnRated(ctx, reviewedTurn(), rating); err != nil {
			t.Fatal(err)
		}
	}
	if len(queue.Enqueued()) != 0 {
		t.Fatalf("ratings above the threshold queued %+v", queue.Enqueued())
	}
	if err := intake.TurnRated(ctx, reviewedTurn(), 2); err != nil {
		t.Fatal(err)
	}
	items := queue.Enqueued()
	if len(items) != 1 || items[0].Source != domain.ReviewSourceFeedback || items[0].Reason != "rated 2 of 5" ||
		items[0].Turn != (domain.ReviewTurnRef{RequestID: "req-1"}) || items[0].Reviewer != (domain.PrincipalRef{}) {
		t.Fatalf("feedback items = %+v", items)
	}

	for name, policy := range map[string]domain.SamplingPolicy{
		"opt-out":            {OptOut: true},
		"redaction required": {RedactionRequired: true},
		"other residency":    {ResidencyRegion: "us"},
	} {
		blocked := &fake.HumanReviewQueue{}
		gated := &ReviewIntake{Policies: &fake.SamplingPolicyRepository{Policy: policy}, Queue: blocked, FeedbackThreshold: 5}
		if err := gated.TurnRated(ctx, reviewedTurn(), 1); err != nil || len(blocked.Enqueued()) != 0 {
			t.Fatalf("%s queued %+v, %v", name, blocked.Enqueued(), err)
		}
	}

	off := &ReviewIntake{Policies: policies, Queue: queue, FeedbackThreshold: 0}
	if err := off.TurnRated(ctx, reviewedTurn(), 1); err != nil || len(queue.Enqueued()) != 1 {
		t.Fatalf("threshold 0 queued a rating: %v", err)
	}
	if err := intake.TurnRated(ctx, reviewedTurn(), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("rating 0 = %v", err)
	}
	if err := (&ReviewIntake{Policies: policies, Queue: queue, FeedbackThreshold: 6}).TurnRated(ctx, reviewedTurn(), 1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("threshold 6 = %v", err)
	}
	broken := &ReviewIntake{Policies: &fake.SamplingPolicyRepository{PolicyFunc: func(context.Context, int64) (domain.SamplingPolicy, error) {
		return domain.SamplingPolicy{}, errors.New("down")
	}}, Queue: queue, FeedbackThreshold: 2}
	if err := broken.TurnRated(ctx, reviewedTurn(), 1); err == nil {
		t.Fatal("a policy failure queued or passed silently")
	}
}
