package observability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func TestNewAuditingObservationRecorderValidates(t *testing.T) {
	next := &fake.ObservationRecorder{}
	sink := &fake.AuditSink{}
	onError := func(error) {}
	if _, err := NewAuditingObservationRecorder(nil, sink, onError, nil); err == nil {
		t.Fatal("nil next accepted")
	}
	if _, err := NewAuditingObservationRecorder(next, nil, onError, nil); err == nil {
		t.Fatal("nil audit accepted")
	}
	if _, err := NewAuditingObservationRecorder(next, sink, nil, nil); err == nil {
		t.Fatal("nil error handler accepted")
	}
}

func TestAuditingObservationRecorderAuditsOnlyRejectedAndFailed(t *testing.T) {
	var forwarded []domain.Observation
	var events []domain.AuditEvent
	var failures []error
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recorder, err := NewAuditingObservationRecorder(
		&fake.ObservationRecorder{RecordObservationFunc: func(_ context.Context, o domain.Observation) { forwarded = append(forwarded, o) }},
		&fake.AuditSink{RecordFunc: func(_ context.Context, e domain.AuditEvent) error { events = append(events, e); return nil }},
		func(err error) { failures = append(failures, err) },
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	base := domain.Observation{TenantID: 5, Stage: domain.StageGuardrail, Component: "stream_pump", Selection: domain.ModelSelection{Provider: "p", Model: "m"}, TraceID: "trace-redacted", Duration: 1500 * time.Millisecond}
	for _, outcome := range []domain.ObservationOutcome{domain.OutcomeOK, domain.OutcomeDegraded, domain.OutcomeCanceled, domain.OutcomeRejected, domain.OutcomeError} {
		observation := base
		observation.Outcome = outcome
		observation.ErrorClass = "forbidden"
		recorder.RecordObservation(context.Background(), observation)
	}
	if len(forwarded) != 5 || len(events) != 2 || len(failures) != 0 {
		t.Fatalf("forwarded=%d events=%d failures=%v", len(forwarded), len(events), failures)
	}
	event := events[0]
	if event.TenantID != 5 || event.Category != AuditCategoryObservation || !event.OccurredAt.Equal(now) {
		t.Fatalf("event = %+v", event)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stage"] != "guardrail" || payload["outcome"] != "rejected" || payload["error_class"] != "forbidden" || payload["duration_ms"] != float64(1500) || payload["model"] != "m" {
		t.Fatalf("payload = %v", payload)
	}
	for _, forbidden := range []string{"tenant_id", "request_id", "conversation_id", "prompt"} {
		if strings.Contains(string(event.Payload), forbidden) {
			t.Fatalf("payload carries %s: %s", forbidden, event.Payload)
		}
	}
}

func TestAuditingObservationRecorderSurfacesAuditFailure(t *testing.T) {
	var failures []error
	sinkErr := errors.New("audit down")
	recorder, _ := NewAuditingObservationRecorder(
		&fake.ObservationRecorder{},
		&fake.AuditSink{RecordFunc: func(context.Context, domain.AuditEvent) error { return sinkErr }},
		func(err error) { failures = append(failures, err) },
		nil,
	)
	recorder.RecordObservation(context.Background(), domain.Observation{TenantID: 1, Outcome: domain.OutcomeError})
	if len(failures) != 1 || !errors.Is(failures[0], sinkErr) {
		t.Fatalf("failures = %v", failures)
	}
}
