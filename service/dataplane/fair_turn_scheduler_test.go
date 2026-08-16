package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func newSchedulerCodec(t *testing.T) (*ObjectStateStore, domain.ObjectRef) {
	t.Helper()
	codec := &ObjectStateStore{Storage: &fake.ObjectStorage{}, Bucket: "turns", MaxBytes: 1 << 20}
	ref, err := codec.Dehydrate(context.Background(), "turn-input/1/request-1", []byte("input"))
	if err != nil {
		t.Fatal(err)
	}
	return codec, ref
}

func queueRow(id int64, tenantID int64, requestID, conversationID string, ref domain.ObjectRef, attempt int, token int64, enqueuedAt, leaseUntil time.Time) []any {
	return []any{id, tenantID, requestID, conversationID, "agent", "route:" + requestID,
		ref.URI, ref.Digest, int64(attempt), enqueuedAt, token, leaseUntil}
}

func newTestScheduler(t *testing.T, query *queueQueryFake, deadLetters *fake.DeadLetterQueue) (*QueueTurnScheduler, time.Time) {
	t.Helper()
	codec, _ := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	return &QueueTurnScheduler{
		DB: queueDBFake{query: query}, Objects: codec, DeadLetters: deadLetters,
		MaxAttempts: 3, PartitionFrom: 0, PartitionTo: 7,
		Now: func() time.Time { return now },
	}, now
}

func TestQueueTurnSchedulerClaimsFairestTenant(t *testing.T) {
	codec, ref := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	query := &queueQueryFake{genID: 99, rows: map[string][][]any{
		qSchedReclaim: nil,
		qSchedCandidates: {
			{int64(1), int64(3), int64(0), now},
			{int64(2), int64(0), int64(0), now},
		},
		qSchedClaim: {queueRow(11, 2, "request-1", "conversation-1", ref, 1, 99, now, now.Add(time.Minute))},
	}}
	scheduler := &QueueTurnScheduler{
		DB: queueDBFake{query: query}, Objects: codec, DeadLetters: &fake.DeadLetterQueue{},
		MaxAttempts: 3, PartitionTo: 7, Now: func() time.Time { return now },
	}
	lease, err := scheduler.Claim(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Message.MessageID != "11:99" || string(lease.Message.Dispatch.Turn.Input) != "input" || lease.Message.Attempt != 1 {
		t.Fatalf("lease = %+v", lease)
	}
	if !lease.Deadline.Equal(now.Add(time.Minute)) {
		t.Fatalf("deadline = %s", lease.Deadline)
	}
	args := query.firstArgs(qSchedClaim)
	want := []any{int64(99), now.Add(time.Minute), "worker-1", int64(2), now, 0, 7, 3}
	if len(args) != len(want) {
		t.Fatalf("claim args = %v", args)
	}
	for i, value := range want {
		if args[i] != value {
			t.Fatalf("claim arg %d = %v, want %v", i, args[i], value)
		}
	}
}

func TestQueueTurnSchedulerClaimReportsEmptyQueue(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{}}
	scheduler, _ := newTestScheduler(t, query, &fake.DeadLetterQueue{})
	if _, err := scheduler.Claim(context.Background(), "worker-1", time.Minute); !errors.Is(err, ErrNoReadyTurn) {
		t.Fatalf("error = %v, want no ready turn", err)
	}
}

func TestQueueTurnSchedulerDeadLettersExpiredLastAttempt(t *testing.T) {
	codec, ref := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	var parked []domain.QueueMessage
	deadLetters := &fake.DeadLetterQueue{PublishFunc: func(_ context.Context, message domain.QueueMessage, reason string) error {
		parked = append(parked, message)
		return nil
	}}
	query := &queueQueryFake{rows: map[string][][]any{
		qSchedReclaim:   nil,
		qSchedExhausted: {queueRow(11, 1, "request-1", "conversation-1", ref, 3, 0, now, now)},
		qSchedDead:      {{int64(11)}},
	}}
	scheduler := &QueueTurnScheduler{
		DB: queueDBFake{query: query}, Objects: codec, DeadLetters: deadLetters,
		MaxAttempts: 3, PartitionTo: 7, Now: func() time.Time { return now },
	}
	if _, err := scheduler.Claim(context.Background(), "worker-1", time.Minute); !errors.Is(err, ErrNoReadyTurn) {
		t.Fatalf("error = %v", err)
	}
	if len(parked) != 1 || parked[0].Dispatch.Turn.RequestID != "request-1" {
		t.Fatalf("parked = %+v", parked)
	}
}

func TestQueueTurnSchedulerFencesAckAndExtend(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qSchedAck: nil, qSchedExtend: {{int64(11)}}}}
	scheduler, now := newTestScheduler(t, query, &fake.DeadLetterQueue{})
	ctx := context.Background()
	if err := scheduler.Ack(ctx, "11:99", "worker-1"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("lost ack = %v, want conflict", err)
	}
	if got := query.firstArgs(qSchedAck); len(got) != 3 || got[0] != int64(11) || got[1] != "worker-1" || got[2] != int64(99) {
		t.Fatalf("ack args = %v", got)
	}
	if err := scheduler.Extend(ctx, "11:99", "worker-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := query.firstArgs(qSchedExtend); len(got) != 4 || got[0] != now.Add(time.Minute) {
		t.Fatalf("extend args = %v", got)
	}
	if err := scheduler.Ack(ctx, "not-a-message", "worker-1"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad message id = %v, want validation", err)
	}
}

func TestQueueTurnSchedulerNackRetriesThenDeadLetters(t *testing.T) {
	codec, ref := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	for name, testCase := range map[string]struct {
		attempt   int
		wantQuery string
	}{
		"retryable attempt":  {attempt: 1, wantQuery: qSchedRetry},
		"exhausted attempts": {attempt: 3, wantQuery: qSchedDead},
	} {
		t.Run(name, func(t *testing.T) {
			var parked []string
			deadLetters := &fake.DeadLetterQueue{PublishFunc: func(_ context.Context, message domain.QueueMessage, reason string) error {
				parked = append(parked, reason)
				return nil
			}}
			query := &queueQueryFake{rows: map[string][][]any{
				qSchedLockLeased: {queueRow(11, 1, "request-1", "conversation-1", ref, testCase.attempt, 99, now, now.Add(time.Minute))},
				qSchedRetry:      {{int64(11)}},
				qSchedDead:       {{int64(11)}},
			}}
			scheduler := &QueueTurnScheduler{
				DB: queueDBFake{query: query}, Objects: codec, DeadLetters: deadLetters,
				MaxAttempts: 3, PartitionTo: 7, Now: func() time.Time { return now },
			}
			if err := scheduler.Nack(context.Background(), "11:99", "worker-1", "boom"); err != nil {
				t.Fatal(err)
			}
			if query.args[testCase.wantQuery] == nil {
				t.Fatalf("calls = %v, want %s", query.calls, testCase.wantQuery)
			}
			if testCase.wantQuery == qSchedDead && len(parked) != 1 {
				t.Fatalf("parked = %v", parked)
			}
			if testCase.wantQuery == qSchedRetry {
				if got := query.firstArgs(qSchedRetry); len(got) != 3 || got[0] != now.Add(defaultQueueBackoff(1)) {
					t.Fatalf("retry args = %v", got)
				}
				if len(parked) != 0 {
					t.Fatalf("retryable attempt was parked: %v", parked)
				}
			}
		})
	}
}

func TestQueueTurnSchedulerNackFencesLostLease(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qSchedLockLeased: nil}}
	scheduler, _ := newTestScheduler(t, query, &fake.DeadLetterQueue{})
	if err := scheduler.Nack(context.Background(), "11:99", "worker-1", "boom"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestTurnQueueWorkerQueriesBindTokenFirst(t *testing.T) {
	queries, pending, claim, reclaim := TurnQueueWorkerQueries("runtime-1", 90*time.Second, 25, 4)
	if len(queries) != 3 || queries[pending] == "" || queries[reclaim] == "" {
		t.Fatalf("queries = %v", queries)
	}
	if !strings.Contains(queries[claim], "lease_token = ?") || !strings.Contains(queries[claim], "WHERE id = ?") {
		t.Fatalf("claim query = %s", queries[claim])
	}
	if strings.Index(queries[claim], "lease_token = ?") > strings.Index(queries[claim], "WHERE id = ?") {
		t.Fatal("claim query must bind the lease token before the id")
	}
	if !strings.Contains(queries[claim], "INTERVAL '90 seconds'") || !strings.Contains(queries[claim], "worker_id = 'runtime-1'") {
		t.Fatalf("claim query = %s", queries[claim])
	}
	if !strings.Contains(queries[pending], "LIMIT 25") || !strings.Contains(queries[pending], "attempt < 4") {
		t.Fatalf("pending query = %s", queries[pending])
	}
	if !strings.Contains(queries[reclaim], "attempt >= 4") || !strings.Contains(queries[reclaim], "INSERT INTO turn_dead_letter") {
		t.Fatalf("reclaim query = %s", queries[reclaim])
	}
}

func TestTableDeadLetterQueuePublishArgumentOrder(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qDeadLetterInsert: {{int64(5)}}}}
	queue := &TableDeadLetterQueue{DB: queueDBFake{query: query}}
	message := domain.QueueMessage{MessageID: "11:99", Dispatch: testDispatch(7, "request-1", "conversation-1"), Attempt: 3}
	if err := queue.Publish(context.Background(), message, "exhausted"); err != nil {
		t.Fatal(err)
	}
	got := query.firstArgs(qDeadLetterInsert)
	if len(got) != 5 || got[0] != "exhausted" || got[1] != 3 || got[2] != int64(11) || got[3] != int64(7) || got[4] != "request-1" {
		t.Fatalf("args = %v", got)
	}
	if err := queue.Publish(context.Background(), domain.QueueMessage{MessageID: "bad"}, "exhausted"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}
