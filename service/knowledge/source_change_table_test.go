package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestSourceChangeEnqueuePollAck(t *testing.T) {
	occurred := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	query := &ingestQueryFake{}
	event := domain.SourceChangeEvent{TenantID: 7, KnowledgeBaseID: "kb", ObjectID: "obj", SourceVersion: "s3", Op: domain.SourceUpserted, Entitlements: []byte(`{"a":1}`), OccurredAt: occurred}
	if err := EnqueueSourceChange(context.Background(), query, event); err != nil {
		t.Fatal(err)
	}
	insert := query.named(qSourceEventInsert)
	if len(insert) != 1 || insert[0].args[0] != int64(42) || insert[0].args[1] != int64(7) || insert[0].args[2] != "kb" || insert[0].args[3] != "obj" || insert[0].args[4] != "s3" || insert[0].args[5] != "upsert" || insert[0].args[6] != `{"a":1}` || insert[0].args[7] != occurred {
		t.Fatalf("insert args = %v", insert)
	}
	if err := EnqueueSourceChange(context.Background(), query, domain.SourceChangeEvent{TenantID: 7, KnowledgeBaseID: "kb", ObjectID: "obj", Op: "rename"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad op = %v", err)
	}
	if queries := SourceChangeWriteQueries(); queries[qSourceEventInsert] == "" {
		t.Fatal("write queries missing insert")
	}

	query.rows = map[string][][]any{qSourceEventPoll: {
		{int64(7), "kb", "obj", "s3", "upsert", `{"a":1}`, occurred},
		{int64(7), "kb", "gone", "s4", "delete", nil, occurred.Add(time.Second)},
	}}
	source := &TableSourceChangeSource{DB: ingestDBFake{query: query}}
	events, err := source.Poll(context.Background(), 7, "kb", 50)
	if err != nil || len(events) != 2 {
		t.Fatalf("poll = %+v, %v", events, err)
	}
	if events[0].ObjectID != "obj" || string(events[0].Entitlements) != `{"a":1}` || events[1].Op != domain.SourceDeleted || events[1].Entitlements != nil || !events[1].OccurredAt.Equal(occurred.Add(time.Second)) {
		t.Fatalf("events = %+v", events)
	}
	if args := query.named(qSourceEventPoll)[0].args; args[0] != int64(7) || args[1] != "kb" || args[2] != 50 {
		t.Fatalf("poll args = %v", args)
	}
	if _, err := source.Poll(context.Background(), 7, "kb", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("limit = %v", err)
	}
	if err := source.Ack(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	acks := query.named(qSourceEventAck)
	if len(acks) != 2 || acks[1].args[2] != "gone" || acks[1].args[3] != "s4" || acks[1].args[4] != "delete" || query.commits != 1 {
		t.Fatalf("acks = %v commits = %d", acks, query.commits)
	}
	if err := source.Ack(context.Background(), nil); err != nil {
		t.Fatalf("empty ack = %v", err)
	}
}
