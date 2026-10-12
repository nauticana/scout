package observability

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Turn chain categories: a turn's decision chain opens with admission and closes
// with exactly one terminal state. Suspension and turn control are intermediate.
const (
	AuditCategoryTurnAdmitted      = "turn_admitted"
	AuditCategoryTurnSuspended     = "turn_suspended"
	AuditCategoryTurnHoldRequested = "turn_hold_requested"
	AuditCategoryTurnHeld          = "turn_held"
	AuditCategoryTurnReleased      = "turn_released"
	// AuditCategoryTurnTakenOver records a person taking over a held turn under a lease.
	AuditCategoryTurnTakenOver = "turn_taken_over"
	// AuditCategoryTurnOperatorAction records one entry a person made in a turn they operate.
	AuditCategoryTurnOperatorAction = "turn_operator_action"
	// AuditCategoryTurnHandedBack records a person returning an operated turn to its agent.
	AuditCategoryTurnHandedBack = "turn_handed_back"
	// AuditCategoryTurnHoldLapsed records an operator lease lapsing and the turn held again.
	AuditCategoryTurnHoldLapsed = "turn_hold_lapsed"
	AuditCategoryTurnCompleted  = "turn_completed"
	AuditCategoryTurnFailed     = "turn_failed"
	AuditCategoryTurnCancelled  = "turn_cancelled"
	// AuditCategoryCallOutcomeAttested records one person's attestation of an unknown call outcome.
	AuditCategoryCallOutcomeAttested = "call_outcome_attested"
	// AuditCategoryCallOutcomeResolved records a person settling an unknown call outcome.
	AuditCategoryCallOutcomeResolved = "call_outcome_resolved"
)

var terminalTurnCategories = []string{AuditCategoryTurnCompleted, AuditCategoryTurnFailed, AuditCategoryTurnCancelled}

// reconciliationCategories are evidence about one call, not steps of the turn's lifecycle: a person
// may settle a call while the turn is held or after it ended.
var reconciliationCategories = []string{AuditCategoryCallOutcomeAttested, AuditCategoryCallOutcomeResolved}

// MemoryAuditSink keeps decisions in process with the table sink's semantics: the
// first record of a decision key wins, a keyless record always appends.
type MemoryAuditSink struct {
	mu      sync.Mutex
	records []domain.DecisionRecord
	keys    map[string]struct{}
}

var _ contract.AuditSink = (*MemoryAuditSink)(nil)

func (sink *MemoryAuditSink) Record(ctx context.Context, decision domain.DecisionRecord) error {
	decision.DecisionKey = domain.DecisionKeyFor(ctx, decision)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if decision.DecisionKey != "" {
		if _, recorded := sink.keys[decision.DecisionKey]; recorded {
			return nil
		}
		if sink.keys == nil {
			sink.keys = make(map[string]struct{})
		}
		sink.keys[decision.DecisionKey] = struct{}{}
	}
	sink.records = append(sink.records, decision)
	return nil
}

// Request returns one request's records in the order they were made.
func (sink *MemoryAuditSink) Request(tenantID int64, requestID string) []domain.DecisionRecord {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var records []domain.DecisionRecord
	for _, record := range sink.records {
		if record.TenantID == tenantID && record.RequestID == requestID {
			records = append(records, record)
		}
	}
	return records
}

// VerifyTurnChain checks one request's records, oldest first, for a gap-free chain:
// admission first, one terminal state last among the lifecycle records, every decision keyed
// and made once. Call reconciliation records may fall anywhere after admission.
func VerifyTurnChain(records []domain.DecisionRecord) error {
	if len(records) == 0 {
		return fmt.Errorf("%w: the turn left no decision record", domain.ErrNotFound)
	}
	if records[0].Category != AuditCategoryTurnAdmitted {
		return fmt.Errorf("%w: the chain opens with %q, not admission", domain.ErrConflict, records[0].Category)
	}
	last := 0
	for index, record := range records {
		if !slices.Contains(reconciliationCategories, record.Category) {
			last = index
		}
	}
	seen := make(map[string]struct{}, len(records))
	for index, record := range records {
		if record.RequestID != records[0].RequestID || record.TenantID != records[0].TenantID {
			return fmt.Errorf("%w: record %d belongs to another request", domain.ErrConflict, index)
		}
		if record.DecisionKey == "" {
			return fmt.Errorf("%w: %s record %d has no decision key, so replay would repeat it", domain.ErrConflict, record.Category, index)
		}
		if _, repeated := seen[record.DecisionKey]; repeated {
			return fmt.Errorf("%w: %s record %d repeats an earlier decision", domain.ErrConflict, record.Category, index)
		}
		seen[record.DecisionKey] = struct{}{}
		if slices.Contains(reconciliationCategories, record.Category) {
			continue
		}
		if terminal := slices.Contains(terminalTurnCategories, record.Category); terminal != (index == last) {
			return fmt.Errorf("%w: the chain must end with its one terminal state; record %d is %q", domain.ErrConflict, index, record.Category)
		}
	}
	return nil
}
