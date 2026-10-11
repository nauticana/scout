package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type safetyQueryFake struct {
	keelport.DatabaseRepository
	keelport.QueryService
	args map[string][]any
	rows [][]any
}

func (fake *safetyQueryFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return fake
}

func (fake *safetyQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	fake.args[name] = args
	if name == qSafetyEventPage {
		return &keelmodel.QueryResult{Rows: fake.rows}, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func safetyEvent() domain.SafetyEvent {
	return domain.SafetyEvent{
		TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"}, AgentID: "writer-a", RequestID: "r-1",
		Stage: domain.GuardrailStageOutput, Layer: domain.GuardrailLayerRelease, Action: domain.GuardrailActionRedact,
		RuleIDs: []string{"pii"}, Severity: domain.GuardrailSeverityHard, ReleaseVersion: "3", PolicyVersion: "g1",
		Duration: 12 * time.Millisecond, OccurredAt: time.Unix(1000, 0),
	}
}

func TestTableSafetyEventSinkRecordsAttributionAndNeverContent(t *testing.T) {
	query := &safetyQueryFake{args: map[string][]any{}}
	sink := &TableSafetyEventSink{DB: query}
	if err := sink.Record(context.Background(), safetyEvent()); err != nil {
		t.Fatal(err)
	}
	args := query.args[qSafetyEventInsert]
	if len(args) != 14 || args[0] != int64(7) || args[1] != "agent" || args[2] != "writer-a" || args[3] != "writer-a" || args[4] != "r-1" ||
		args[9] != `["pii"]` || args[10] != "3" || args[11] != "g1" || args[12] != int64(12) {
		t.Fatalf("insert args = %v", args)
	}
	unattributed := safetyEvent()
	unattributed.Principal, unattributed.AgentID, unattributed.RequestID, unattributed.RuleIDs = domain.PrincipalRef{}, "", "", nil
	if err := sink.Record(context.Background(), unattributed); err != nil {
		t.Fatal(err)
	}
	if args = query.args[qSafetyEventInsert]; args[1] != nil || args[3] != nil || args[4] != nil || args[9] != `[]` {
		t.Fatalf("an event without attribution stores NULLs: %v", args)
	}
	incomplete := safetyEvent()
	incomplete.Layer = ""
	if err := sink.Record(context.Background(), incomplete); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if err := (&TableSafetyEventSink{}).Record(context.Background(), safetyEvent()); err == nil {
		t.Fatal("a sink without a database must refuse")
	}
}

func TestTableSafetyEventSinkPagesOneAgentNewestFirst(t *testing.T) {
	at := time.Unix(2000, 0).UTC()
	query := &safetyQueryFake{args: map[string][]any{}, rows: [][]any{
		{int64(41), "agent", "writer-a", "r-1", "output", "tenant", "block", "hard", `["tenant.codename"]`, "3", "g1", int64(5), at},
	}}
	sink := &TableSafetyEventSink{DB: query}
	page, err := sink.SafetyEvents(context.Background(), 7, domain.SafetyEventFilter{AgentID: "writer-a", Before: 50, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if args := query.args[qSafetyEventPage]; len(args) != 5 || args[0] != int64(7) || args[1] != "writer-a" || args[2] != int64(50) || args[4] != 10 {
		t.Fatalf("page args = %v", args)
	}
	event := page[0]
	if len(page) != 1 || event.ID != 41 || event.TenantID != 7 || event.AgentID != "writer-a" || event.Layer != domain.GuardrailLayerTenant ||
		event.RuleIDs[0] != "tenant.codename" || event.Duration != 5*time.Millisecond || !event.OccurredAt.Equal(at) || event.RequestID != "r-1" {
		t.Fatalf("page = %+v", page)
	}

	for _, filter := range []domain.SafetyEventFilter{
		{AgentID: "", Limit: 10}, {AgentID: "writer-a", Limit: 0}, {AgentID: "writer-a", Limit: MaxSafetyEventPage + 1},
		{AgentID: "writer-a", Limit: 10, Before: -1},
	} {
		if _, err := sink.SafetyEvents(context.Background(), 7, filter); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("filter %+v: want ErrValidation, got %v", filter, err)
		}
	}
	if _, err := sink.SafetyEvents(context.Background(), 0, domain.SafetyEventFilter{AgentID: "writer-a", Limit: 10}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no tenant: want ErrValidation, got %v", err)
	}
	query.rows[0][8] = "not json"
	if _, err := sink.SafetyEvents(context.Background(), 7, domain.SafetyEventFilter{AgentID: "writer-a", Limit: 10}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("malformed rule ids: want ErrConflict, got %v", err)
	}
	query.rows[0][8], query.rows[0][12] = `[]`, "yesterday"
	if _, err := sink.SafetyEvents(context.Background(), 7, domain.SafetyEventFilter{AgentID: "writer-a", Limit: 10}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("malformed time: want ErrConflict, got %v", err)
	}
}
