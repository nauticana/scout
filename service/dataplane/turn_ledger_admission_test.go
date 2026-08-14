package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func TestTurnLedgerBeginAppliesAdmissionBeforePersistence(t *testing.T) {
	denied := domain.ErrRateLimited
	var tenantID int64
	ledger := &TurnLedger{Admission: &fake.TenantRateLimiter{AllowTurnFunc: func(_ context.Context, tenant domain.TenantContext) error {
		tenantID = tenant.TenantID
		return denied
	}}}
	_, _, err := ledger.Begin(context.Background(), 7, "user", "request", "task", "input",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "running",
		domain.AgentReleaseReference{AgentID: "agent", Version: "1", Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if !errors.Is(err, denied) || tenantID != 7 {
		t.Fatalf("admission = tenant %d, error %v", tenantID, err)
	}
}
