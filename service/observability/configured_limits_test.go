package observability

import (
	"context"
	"errors"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type pageQueryFake struct {
	keelport.DatabaseRepository
	keelport.QueryService
	limit any
}

func (fake *pageQueryFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return fake
}

func (fake *pageQueryFake) Query(_ context.Context, _ string, args ...any) (*keelmodel.QueryResult, error) {
	fake.limit = args[len(args)-1]
	return &keelmodel.QueryResult{}, nil
}

func TestTableAuditSinkPageSizes(t *testing.T) {
	cases := map[string]struct {
		pageSize, maxPageSize, limit int
		want                         int
	}{
		"default page":             {want: DefaultDecisionPageSize},
		"default cap":              {limit: DefaultMaxDecisionPageSize + 1, want: DefaultMaxDecisionPageSize},
		"configured page":          {pageSize: 25, want: 25},
		"configured cap":           {pageSize: 10, maxPageSize: 50, limit: 60, want: 50},
		"configured page and cap":  {pageSize: 10, maxPageSize: 20, want: 10},
		"a request within the cap": {pageSize: 10, maxPageSize: 50, limit: 30, want: 30},
	}
	for name, tc := range cases {
		query := &pageQueryFake{}
		sink := &TableAuditSink{DB: query, PageSize: tc.pageSize, MaxPageSize: tc.maxPageSize}
		if _, err := sink.Decisions(context.Background(), domain.DecisionQuery{TenantID: 7, Limit: tc.limit}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if query.limit != tc.want {
			t.Fatalf("%s: limit = %v, want %d", name, query.limit, tc.want)
		}
	}
	for name, sink := range map[string]*TableAuditSink{
		"negative page":       {PageSize: -1},
		"negative cap":        {MaxPageSize: -1},
		"page above cap":      {PageSize: 30, MaxPageSize: 20},
		"page above default":  {PageSize: DefaultMaxDecisionPageSize + 1},
		"default above a cap": {MaxPageSize: DefaultDecisionPageSize - 1},
	} {
		sink.DB = &pageQueryFake{}
		if _, err := sink.Decisions(context.Background(), domain.DecisionQuery{TenantID: 7}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want a validation refusal", name, err)
		}
	}
}

func TestTableSafetyEventSinkMaxPage(t *testing.T) {
	ctx := context.Background()
	query := &pageQueryFake{}
	sink := &TableSafetyEventSink{DB: query, MaxPage: 20}
	if _, err := sink.SafetyEvents(ctx, 7, domain.SafetyEventFilter{AgentID: "writer-a", Limit: 20}); err != nil || query.limit != 20 {
		t.Fatalf("a page at the configured cap: limit %v, %v", query.limit, err)
	}
	if _, err := sink.SafetyEvents(ctx, 7, domain.SafetyEventFilter{AgentID: "writer-a", Limit: 21}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a page above the configured cap: %v", err)
	}
	sink.MaxPage = 0
	if _, err := sink.SafetyEvents(ctx, 7, domain.SafetyEventFilter{AgentID: "writer-a", Limit: DefaultMaxSafetyEventPage}); err != nil {
		t.Fatalf("a page at the default cap: %v", err)
	}
	sink.MaxPage = -1
	if _, err := sink.SafetyEvents(ctx, 7, domain.SafetyEventFilter{AgentID: "writer-a", Limit: 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative cap: %v", err)
	}
}
