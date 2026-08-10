package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// LoopDetector contains configurable loop callbacks.
type LoopDetector struct {
	ObserveFunc func(context.Context, int64, string, string) error
	ResetFunc   func(context.Context, int64, string) error
}

// Observe invokes ObserveFunc.
func (detector *LoopDetector) Observe(ctx context.Context, tenantID int64, conversationID, fingerprint string) error {
	return detector.ObserveFunc(ctx, tenantID, conversationID, fingerprint)
}

// Reset invokes ResetFunc.
func (detector *LoopDetector) Reset(ctx context.Context, tenantID int64, conversationID string) error {
	return detector.ResetFunc(ctx, tenantID, conversationID)
}

// CostCircuitBreaker contains configurable cost callbacks.
type CostCircuitBreaker struct {
	AllowFunc  func(context.Context, int64, string, int64) error
	RecordFunc func(context.Context, int64, string, domain.Usage) error
}

// Allow invokes AllowFunc.
func (breaker *CostCircuitBreaker) Allow(ctx context.Context, tenantID int64, agentID string, projectedCostMinorUnits int64) error {
	return breaker.AllowFunc(ctx, tenantID, agentID, projectedCostMinorUnits)
}

// Record invokes RecordFunc.
func (breaker *CostCircuitBreaker) Record(ctx context.Context, tenantID int64, agentID string, usage domain.Usage) error {
	return breaker.RecordFunc(ctx, tenantID, agentID, usage)
}

var _ contract.LoopDetector = (*LoopDetector)(nil)
var _ contract.CostCircuitBreaker = (*CostCircuitBreaker)(nil)
