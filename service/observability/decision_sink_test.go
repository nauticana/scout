package observability

import (
	"context"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type decisionQueryFake struct {
	keelport.DatabaseRepository
	keelport.QueryService
	keys []any
}

func (fake *decisionQueryFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return fake
}

func (fake *decisionQueryFake) Query(_ context.Context, _ string, args ...any) (*keelmodel.QueryResult, error) {
	fake.keys = append(fake.keys, args[len(args)-1])
	return &keelmodel.QueryResult{}, nil
}

func TestTableAuditSinkKeysADecisionByItsScopeSoReplayWritesItOnce(t *testing.T) {
	if !strings.Contains(decisionQueries[qDecisionInsert], "ON CONFLICT (tenant_id, decision_key) DO NOTHING") {
		t.Fatal("the insert must keep the first record of a decision key")
	}
	query := &decisionQueryFake{}
	sink := &TableAuditSink{DB: query}
	decision := domain.DecisionRecord{
		TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"},
		Category: domain.DecisionCategoryGuardrail, Action: "output", Resource: "flag", Outcome: domain.DecisionAllow, RequestID: "r",
	}
	delivery := func() context.Context {
		return domain.WithDecisionScope(domain.WithDecisionScope(context.Background(), "turn/", "r"), "step/", 1)
	}
	first := delivery()
	for _, ctx := range []context.Context{first, first, delivery(), context.Background()} {
		if err := sink.Record(ctx, decision); err != nil {
			t.Fatal(err)
		}
	}
	once, again, replayed, unscoped := query.keys[0], query.keys[1], query.keys[2], query.keys[3]
	if once == nil || once == again {
		t.Fatalf("two identical decisions in one scope are two decisions: %v, %v", once, again)
	}
	if replayed != once {
		t.Fatalf("a redelivery must derive the first delivery's key: %v, %v", replayed, once)
	}
	if unscoped != nil {
		t.Fatalf("a decision outside any scope stays append-only, got key %v", unscoped)
	}
}
