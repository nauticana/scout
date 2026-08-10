package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
)

func TestAgentRunStoreRecordsVerifiedRelease(t *testing.T) {
	query := &agentRunQueryFake{rows: map[string][][]any{qRecordAgentRun: {{int64(5)}}}}
	store := &AgentRunStore{qs: query}
	release := domain.AgentReleaseReference{
		AgentID: "writer", Version: "3",
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := store.Record(context.Background(), 8, release, " generate_blog "); err != nil {
		t.Fatalf("Record: %v", err)
	}
	args := query.args[qRecordAgentRun]
	if len(args) != 8 || args[0] != int64(8) || args[3] != "generate_blog" || args[7] != release.Digest {
		t.Fatalf("record args = %+v", args)
	}
}

func TestAgentRunStoreRejectsInvalidOrMismatchedRelease(t *testing.T) {
	store := &AgentRunStore{qs: &agentRunQueryFake{rows: map[string][][]any{}}}
	if err := store.Record(context.Background(), 0, domain.AgentReleaseReference{}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid error = %v", err)
	}
	release := domain.AgentReleaseReference{
		AgentID: "writer", Version: "3",
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := store.Record(context.Background(), 8, release, "task"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mismatch error = %v", err)
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
