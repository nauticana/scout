package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// queueQueryFake answers the queue's named queries from canned rows and records
// every argument list, so tests can assert argument order.
type queueQueryFake struct {
	rows  map[string][][]any
	errs  map[string]error
	calls []string
	args  map[string][][]any
	genID int64
}

func (query *queueQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.args == nil {
		query.args = make(map[string][][]any)
	}
	query.calls = append(query.calls, name)
	query.args[name] = append(query.args[name], append([]any(nil), args...))
	if err := query.errs[name]; err != nil {
		return nil, err
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (query *queueQueryFake) GenID() int64             { return query.genID }
func (*queueQueryFake) Commit(context.Context) error   { return nil }
func (*queueQueryFake) Rollback(context.Context) error { return nil }
func (query *queueQueryFake) firstArgs(name string) []any {
	lists := query.args[name]
	if len(lists) == 0 {
		return nil
	}
	return lists[0]
}

type queueDBFake struct {
	keelport.DatabaseRepository
	query *queueQueryFake
}

func (db queueDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db queueDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

func testDispatch(tenantID int64, requestID, conversationID string) domain.TurnDispatch {
	return domain.TurnDispatch{
		Turn: domain.TurnRequest{
			TenantContext:  domain.TenantContext{TenantID: tenantID, PriorityClass: "interactive"},
			RequestID:      requestID,
			ConversationID: conversationID,
			AgentID:        "agent",
			Input:          []byte("input"),
		},
		ReplyRoute: "route:" + requestID,
		EnqueuedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func TestQueueTurnDispatcherEnqueueArgumentOrder(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qQueueEnqueue: {{int64(11)}}}}
	dispatcher := &QueueTurnDispatcher{
		DB: queueDBFake{query: query}, Partitions: 16, ShardsPerTenant: 2,
		PriorityRank: func(tenant domain.TenantContext) int { return 1 },
	}
	dispatch := testDispatch(7, "request-1", "conversation-1")
	if err := dispatcher.Enqueue(context.Background(), dispatch); err != nil {
		t.Fatal(err)
	}
	args := query.firstArgs(qQueueEnqueue)
	partition, err := ShufflePartition(7, "conversation-1", 16, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{"agent", partition, 1, "route:request-1", dispatch.EnqueuedAt, dispatch.EnqueuedAt,
		int64(7), "request-1", "conversation-1", DigestBytes([]byte("input"))}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i, value := range want {
		if args[i] != value {
			t.Fatalf("arg %d = %v, want %v", i, args[i], value)
		}
	}
}

func TestQueueTurnDispatcherDeduplicatesAndDetectsConflict(t *testing.T) {
	digest := DigestBytes([]byte("input"))
	for name, testCase := range map[string]struct {
		queued  [][]any
		record  [][]any
		wantErr error
	}{
		"identical replay is a no-op": {queued: [][]any{{int64(11), digest, "queued"}}},
		"different input conflicts":   {queued: [][]any{{int64(11), "other", "queued"}}, wantErr: domain.ErrConflict},
		"missing turn record":         {record: nil, wantErr: domain.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			query := &queueQueryFake{rows: map[string][][]any{
				qQueueEnqueue:    nil,
				qQueueFindByReq:  testCase.queued,
				qQueueTurnDigest: testCase.record,
			}}
			dispatcher := &QueueTurnDispatcher{DB: queueDBFake{query: query}, Partitions: 8, ShardsPerTenant: 2}
			err := dispatcher.Enqueue(context.Background(), testDispatch(7, "request-1", "conversation-1"))
			if testCase.wantErr == nil && err != nil {
				t.Fatalf("error = %v", err)
			}
			if testCase.wantErr != nil && !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

func TestQueueTurnDispatcherRejectsInvalidConfiguration(t *testing.T) {
	dispatcher := &QueueTurnDispatcher{DB: queueDBFake{query: &queueQueryFake{}}, Partitions: 2, ShardsPerTenant: 3}
	if err := dispatcher.Enqueue(context.Background(), testDispatch(7, "request-1", "conversation-1")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestShufflePartitionIsDeterministicAndSharded(t *testing.T) {
	first, err := ShufflePartition(42, "conversation-1", 32, 4)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ShufflePartition(42, "conversation-1", 32, 4)
	if err != nil || first != again {
		t.Fatalf("partition = %d then %d, error = %v", first, again, err)
	}
	shards := map[int]struct{}{}
	for _, conversation := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		partition, err := ShufflePartition(42, conversation, 32, 4)
		if err != nil {
			t.Fatal(err)
		}
		shards[partition] = struct{}{}
	}
	if len(shards) > 4 {
		t.Fatalf("tenant spread over %d partitions, want at most 4", len(shards))
	}
	if _, err := ShufflePartition(42, "conversation-1", 0, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestPickTenantsOrdersByWeightedLoad(t *testing.T) {
	policy := &weightPolicy{weights: map[int64]int{1: 1, 2: 4, 3: 1}, maxConcurrent: map[int64]int{3: 1}}
	ordered, err := pickTenants(context.Background(), policy, []tenantCandidate{
		{tenantID: 1, leased: 2, bestRank: 1, oldest: 10},
		{tenantID: 2, leased: 4, bestRank: 1, oldest: 10},
		{tenantID: 3, leased: 1, bestRank: 0, oldest: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 || ordered[0].tenantID != 2 || ordered[1].tenantID != 1 {
		t.Fatalf("ordered = %+v", ordered)
	}
}
