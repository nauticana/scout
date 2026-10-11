package approval

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/keel/actiontoken"
	"github.com/nauticana/keel/idempotency"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// tokenDB answers keel's action_token queries from rows keyed by query name.
type tokenDB struct {
	keelport.DatabaseRepository
	rows    map[string][][]any
	queries []string
}

func (db *tokenDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db
}
func (db *tokenDB) Query(_ context.Context, name string, _ ...any) (*keelmodel.QueryResult, error) {
	db.queries = append(db.queries, name)
	return &keelmodel.QueryResult{Rows: db.rows[name]}, nil
}
func (*tokenDB) GenID() int64 { return 0 }

const linkToken = "0000000000000000000000000000000000000000000000000000000000000000"

func linkedResumer(t *testing.T, claim []any) (*Resumer, *stubStore, domain.ApprovalKey, *int) {
	t.Helper()
	store, verdict := openRequest(t)
	store.stored.RiskTier = domain.RiskMedium
	resumed, failed, requeued := 0, 0, 0
	resumer := resumerFor(store, &resumed, &failed, &requeued)
	db := &tokenDB{rows: map[string][][]any{}}
	if claim != nil {
		db.rows["action_token_claim"] = [][]any{claim}
	}
	resumer.Links = &actiontoken.Service{DB: db, Ledger: &idempotency.MemoryLedger{}}
	resumer.LinkMaxRisk = domain.RiskMedium
	resumer.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return resumer, store, verdict.RequestKey, &requeued
}

func resourceKey(key domain.ApprovalKey) string {
	return fmt.Sprintf("%d/%s/%d", key.TenantID, key.RequestID, key.ExecutionStepID)
}

func TestRedeemLinkDecidesAsTheLinkedPerson(t *testing.T) {
	store, _ := openRequest(t)
	key := domain.ApprovalKey{TenantID: 7, RequestID: "request", ExecutionStepID: store.stored.ExecutionStepID}
	resumer, store, key, requeued := linkedResumer(t, []any{int64(5), int64(42), int64(7), "approval/approved", resourceKey(key), store.stored.ProposedDigest})
	request, err := resumer.RedeemLink(context.Background(), linkToken, key, domain.ApprovalStatusApproved, "fine")
	if err != nil || request.Status != domain.ApprovalStatusApproved {
		t.Fatalf("request = %+v, error = %v", request, err)
	}
	if store.resolved.Decider.ID != "42" || store.resolved.Reason != "fine" || *requeued != 1 {
		t.Fatalf("verdict = %+v, requeued = %d", store.resolved, *requeued)
	}
}

func TestRedeemLinkRefusesASpentOrForeignLink(t *testing.T) {
	resumer, _, key, _ := linkedResumer(t, nil)
	if _, err := resumer.RedeemLink(context.Background(), linkToken, key, domain.ApprovalStatusApproved, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("spent link = %v, want not found", err)
	}
	store, _ := openRequest(t)
	resumer, store, key, requeued := linkedResumer(t, []any{int64(5), int64(42), int64(9), "approval/approved", resourceKey(key), store.stored.ProposedDigest})
	if _, err := resumer.RedeemLink(context.Background(), linkToken, key, domain.ApprovalStatusApproved, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("foreign tenant = %v, want forbidden", err)
	}
	if store.resolved.Decider.ID != "" || *requeued != 0 {
		t.Fatal("a refused link must not decide")
	}
}

func TestLinksStayOutOfHighRiskDecisions(t *testing.T) {
	resumer, store, key, _ := linkedResumer(t, nil)
	store.stored.RiskTier = domain.RiskHigh
	if _, err := resumer.MintLink(context.Background(), key, 42, domain.ApprovalStatusApproved, time.Hour); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("high-risk link = %v, want forbidden", err)
	}
	resumer.LinkMaxRisk = ""
	store.stored.RiskTier = domain.RiskLow
	if _, err := resumer.MintLink(context.Background(), key, 42, domain.ApprovalStatusApproved, time.Hour); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no link policy = %v, want forbidden", err)
	}
	resumer.LinkMaxRisk = domain.RiskMedium
	if _, err := resumer.MintLink(context.Background(), key, 42, domain.ApprovalStatusEdited, time.Hour); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("edit link = %v, want validation", err)
	}
	token, err := resumer.MintLink(context.Background(), key, 42, domain.ApprovalStatusRejected, time.Hour)
	if err != nil || len(token) != 64 {
		t.Fatalf("token = %q, error = %v", token, err)
	}
}

func TestPendingKeepsOnlyWhatTheDeciderMayDecide(t *testing.T) {
	resumer := &Resumer{
		Inbox: &fake.ApprovalInbox{PendingFunc: func(context.Context, domain.ApprovalFilter) ([]domain.ApprovalRequest, error) {
			return []domain.ApprovalRequest{{ID: 1, ScopeID: "finance"}, {ID: 2, ScopeID: "sales"}}, nil
		}},
		Authorizer: fake.ApprovalAuthorizerFunc(func(_ context.Context, request domain.ApprovalRequest, _ domain.PrincipalRef) error {
			if request.ScopeID != "sales" {
				return domain.ErrForbidden
			}
			return nil
		}),
	}
	requests, err := resumer.Pending(context.Background(), 7, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, 10)
	if err != nil || len(requests) != 1 || requests[0].ID != 2 {
		t.Fatalf("requests = %+v, error = %v", requests, err)
	}
}

func TestResolveRefusesAnythingButAVerdict(t *testing.T) {
	store, verdict := openRequest(t)
	verdict.Status = domain.ApprovalStatusExpired
	var resumed, failed, requeued int
	if _, err := resumerFor(store, &resumed, &failed, &requeued).Resolve(context.Background(), verdict); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestRedeemLinkRevealsNothingBeforeItsToken(t *testing.T) {
	resumer, store, key, _ := linkedResumer(t, nil)
	store.stored.RiskTier = domain.RiskCritical
	if _, err := resumer.RedeemLink(context.Background(), linkToken, key, domain.ApprovalStatusApproved, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}
