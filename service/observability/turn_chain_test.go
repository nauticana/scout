package observability

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func chainRecord(category, key string) domain.DecisionRecord {
	return domain.DecisionRecord{TenantID: 7, RequestID: "request-1", Category: category, DecisionKey: key}
}

// A person may settle an unknown call while the turn is held or after it ended; those records are
// call evidence, not lifecycle steps, so the chain still ends with its one terminal state.
func TestVerifyTurnChainAdmitsReconciliationAroundTheTerminalState(t *testing.T) {
	chain := []domain.DecisionRecord{
		chainRecord(AuditCategoryTurnAdmitted, "a"), chainRecord(AuditCategoryTurnHeld, "h"),
		chainRecord(AuditCategoryCallOutcomeAttested, "c1"), chainRecord(AuditCategoryTurnCancelled, "x"),
		chainRecord(AuditCategoryCallOutcomeAttested, "c2"), chainRecord(AuditCategoryCallOutcomeResolved, "r"),
	}
	if err := VerifyTurnChain(chain); err != nil {
		t.Fatalf("reconciliation after the terminal state = %v", err)
	}
	for name, broken := range map[string][]domain.DecisionRecord{
		"lifecycle after terminal": append(chain[:4:4], chainRecord(AuditCategoryTurnReleased, "late")),
		"no terminal":              {chainRecord(AuditCategoryTurnAdmitted, "a"), chainRecord(AuditCategoryCallOutcomeResolved, "r")},
		"repeated evidence":        append(chain[:6:6], chainRecord(AuditCategoryCallOutcomeResolved, "r")),
		"unkeyed evidence":         append(chain[:6:6], chainRecord(AuditCategoryCallOutcomeAttested, "")),
	} {
		if err := VerifyTurnChain(broken); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s = %v, want ErrConflict", name, err)
		}
	}
}
