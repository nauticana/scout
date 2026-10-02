package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
)

const runDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func failedRun() domain.AgentRun {
	return domain.AgentRun{
		Release:  domain.AgentReleaseReference{AgentID: "writer", Version: "3", Digest: runDigest},
		TaskKind: " generate_blog ", RequestID: "req-1", Status: domain.RunFailed,
	}
}

func TestAgentRunStoreRecordsEverySettledStatus(t *testing.T) {
	query := &agentRunQueryFake{rows: map[string][][]any{qRecordAgentRun: {{int64(5)}}}}
	store := &AgentRunStore{qs: query}
	if err := store.Record(context.Background(), 8, failedRun()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	args := query.args[qRecordAgentRun]
	if len(args) != 11 || args[0] != int64(8) || args[3] != "generate_blog" || args[4] != "req-1" || args[5] != "failed" || args[10] != runDigest {
		t.Fatalf("record args = %+v", args)
	}
	outside := failedRun()
	outside.RequestID, outside.Release.Digest, outside.Status = "", "", domain.RunCompleted
	if err := store.Record(context.Background(), 8, outside); err != nil {
		t.Fatalf("Record outside a turn: %v", err)
	}
	if args = query.args[qRecordAgentRun]; args[4] != nil || args[9] != "" {
		t.Fatalf("an execution outside a turn stores no request id and checks no digest: %+v", args)
	}
}

func TestAgentRunStoreRecordsARequestOnce(t *testing.T) {
	query := &agentRunQueryFake{rows: map[string][][]any{qAgentRunByRequest: {{"writer", "3", "generate_blog", "failed"}}}}
	store := &AgentRunStore{qs: query}
	if err := store.Record(context.Background(), 8, failedRun()); err != nil {
		t.Fatalf("the same run again must be a no-op, got %v", err)
	}
	completed := failedRun()
	completed.Status = domain.RunCompleted
	if err := store.Record(context.Background(), 8, completed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another outcome for the request: want ErrConflict, got %v", err)
	}
	otherTask := failedRun()
	otherTask.TaskKind = "summarize"
	if err := store.Record(context.Background(), 8, otherTask); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another task for the request: want ErrConflict, got %v", err)
	}
}

func TestAgentRunStoreRejectsInvalidOrMismatchedRelease(t *testing.T) {
	store := &AgentRunStore{qs: &agentRunQueryFake{rows: map[string][][]any{}}}
	for name, mutate := range map[string]func(*domain.AgentRun){
		"no tenant release": func(run *domain.AgentRun) { run.Release = domain.AgentReleaseReference{} },
		"short digest":      func(run *domain.AgentRun) { run.Release.Digest = "abc" },
		"non-hex digest":    func(run *domain.AgentRun) { run.Release.Digest = strings.Repeat("z", 64) },
		"live status":       func(run *domain.AgentRun) { run.Status = "running" },
		"no task kind":      func(run *domain.AgentRun) { run.TaskKind = " " },
	} {
		run := failedRun()
		mutate(&run)
		if err := store.Record(context.Background(), 8, run); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	if err := store.Record(context.Background(), 8, failedRun()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestAgentRunStoreListsRunsNewestFirst(t *testing.T) {
	completedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	query := &agentRunQueryFake{rows: map[string][][]any{
		qAgentRuns: {{int64(9), "writer", "3", "generate_blog", "req-9", "cancelled", completedAt}},
	}}
	store := &AgentRunStore{qs: query}
	runs, err := store.Runs(context.Background(), 8, domain.AgentRunFilter{AgentID: "writer", Status: domain.RunCancelled, Before: 10, Limit: 50})
	if err != nil || len(runs) != 1 || runs[0].ID != 9 || runs[0].Status != domain.RunCancelled || runs[0].RequestID != "req-9" || !runs[0].CompletedAt.Equal(completedAt) {
		t.Fatalf("Runs = %+v, %v", runs, err)
	}
	if args := query.args[qAgentRuns]; args[1] != "writer" || args[3] != "" || args[5] != "cancelled" || args[7] != int64(10) || args[9] != 50 {
		t.Fatalf("runs args = %+v", args)
	}
	for name, filter := range map[string]domain.AgentRunFilter{
		"no limit":      {},
		"over the cap":  {Limit: MaxAgentRunPage + 1},
		"negative from": {Limit: 1, Before: -1},
		"non-terminal":  {Limit: 1, Status: "queued"},
	} {
		if _, err := store.Runs(context.Background(), 8, filter); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
}

func TestAgentRunStoreReportsLatestActivity(t *testing.T) {
	completedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := &AgentRunStore{qs: &agentRunQueryFake{rows: map[string][][]any{
		qAgentLastRun: {{"writer", completedAt}},
	}}}
	activity, err := store.LastRun(context.Background(), 8)
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !activity["writer"].Equal(completedAt) {
		t.Fatalf("activity = %+v", activity)
	}

	store.qs = &agentRunQueryFake{rows: map[string][][]any{qAgentLastRun: {{"writer", "not-a-time"}}}}
	if _, err := store.LastRun(context.Background(), 8); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("malformed activity error = %v", err)
	}
}

type agentRunQueryFake struct {
	rows map[string][][]any
	args map[string][]any
}

func (query *agentRunQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.args == nil {
		query.args = make(map[string][]any)
	}
	query.args[name] = append([]any(nil), args...)
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*agentRunQueryFake) GenID() int64 { return 0 }

func TestAgentRunStorePurgeIsBoundedAndOptional(t *testing.T) {
	query := &agentRunQueryFake{rows: map[string][][]any{qPurgeAgentRuns: {{int64(1)}, {int64(2)}}}}
	store := &AgentRunStore{qs: query}

	purged, err := store.Purge(context.Background(), 30, 200)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d, want 2", purged)
	}
	if args := query.args[qPurgeAgentRuns]; len(args) != 2 || args[0] != 30 || args[1] != 200 {
		t.Fatalf("purge args = %+v", args)
	}

	query.args = map[string][]any{}
	if purged, err = store.Purge(context.Background(), 0, 200); err != nil || purged != 0 {
		t.Fatalf("zero retention must keep everything: purged=%d err=%v", purged, err)
	}
	if _, ran := query.args[qPurgeAgentRuns]; ran {
		t.Fatal("zero retention must not issue a delete")
	}
	if _, err = store.Purge(context.Background(), -1, 200); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative retention error = %v", err)
	}
	if _, err = store.Purge(context.Background(), 30, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unbounded purge error = %v", err)
	}
}
