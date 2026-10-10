package observability

import (
	"context"
	"errors"
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
	args [][]any
	rows [][]any
}

func (fake *decisionQueryFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return fake
}

func (fake *decisionQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if name == qDecisionPage {
		return &keelmodel.QueryResult{Rows: fake.rows}, nil
	}
	fake.keys = append(fake.keys, args[len(args)-1])
	fake.args = append(fake.args, args)
	return &keelmodel.QueryResult{}, nil
}

func TestTableAuditSinkStoresAndReadsTheClientApplication(t *testing.T) {
	query := &decisionQueryFake{}
	sink := &TableAuditSink{DB: query}
	decision := domain.DecisionRecord{
		TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "41"}, ClientRef: "oauth_client:abc",
		Category: domain.DecisionCategoryToolInvoke, Action: "publish_page", Outcome: domain.DecisionAllow,
	}
	if err := sink.Record(context.Background(), decision); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decisionQueries[qDecisionInsert], "grantor_id, client_ref,") || query.args[0][7] != decision.ClientRef {
		t.Fatalf("client_ref insert = %v", query.args[0][7])
	}
	row := make([]any, 22)
	row[21] = decision.ClientRef
	query.rows = [][]any{row}
	page, err := sink.Decisions(context.Background(), domain.DecisionQuery{TenantID: 7})
	if !strings.Contains(decisionQueries[qDecisionPage], "occurred_at, client_ref") ||
		err != nil || len(page.Records) != 1 || page.Records[0].ClientRef != decision.ClientRef {
		t.Fatalf("Decisions = %+v, %v", page, err)
	}
	decision.ClientRef = ""
	if err := sink.Record(context.Background(), decision); err != nil {
		t.Fatal(err)
	}
	if got := query.args[1][7]; got != nil {
		t.Fatalf("empty client_ref = %v", got)
	}
}

func TestTableAuditSinkKeysADecisionByItsScopeSoReplayWritesItOnce(t *testing.T) {
	if !strings.Contains(decisionQueries[qDecisionInsert], "ON CONFLICT (partner_id, decision_key) DO NOTHING") {
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

type decisionTxFake struct {
	keelport.TxQueryService
	query *decisionQueryFake
}

func (fake decisionTxFake) QueryService(string, map[string]string) keelport.QueryService {
	if fake.query == nil {
		return nil
	}
	return fake.query
}

func TestTableAuditSinkRecordTxWritesThroughTheCallersTransaction(t *testing.T) {
	sink := &TableAuditSink{}
	decision := domain.DecisionRecord{
		Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "ops"},
		Category:  domain.DecisionCategoryRestriction, Action: "replace", Outcome: domain.DecisionAllow,
	}
	query := &decisionQueryFake{}
	if err := sink.RecordTx(context.Background(), decisionTxFake{query: query}, decision); err != nil || len(query.keys) != 1 {
		t.Fatalf("RecordTx = %v, writes %d", err, len(query.keys))
	}
	var plain keelport.TxQueryService = struct{ keelport.TxQueryService }{}
	if err := sink.RecordTx(context.Background(), plain, decision); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a transaction without a query catalog: want ErrNotReady, got %v", err)
	}
	if err := sink.RecordTx(context.Background(), decisionTxFake{}, decision); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a transaction without a bound query service: want ErrNotReady, got %v", err)
	}
	decision.Principal.ID = " "
	if err := sink.RecordTx(context.Background(), decisionTxFake{query: query}, decision); !errors.Is(err, domain.ErrPrincipalUnknown) || len(query.keys) != 1 {
		t.Fatalf("an anonymous decision: want ErrPrincipalUnknown, got %v", err)
	}
}
