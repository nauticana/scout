package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ObservationRecorder contains a configurable structured observation callback.
type ObservationRecorder struct {
	RecordObservationFunc func(context.Context, domain.Observation)
}

// RecordObservation invokes RecordObservationFunc when configured.
func (recorder *ObservationRecorder) RecordObservation(ctx context.Context, observation domain.Observation) {
	if recorder.RecordObservationFunc != nil {
		recorder.RecordObservationFunc(ctx, observation)
	}
}

// MetricLabelSink contains a configurable bounded-label metric callback.
type MetricLabelSink struct {
	ObserveFunc func(context.Context, string, map[string]string, float64)
}

// Observe invokes ObserveFunc when configured.
func (sink *MetricLabelSink) Observe(ctx context.Context, name string, labels map[string]string, value float64) {
	if sink.ObserveFunc != nil {
		sink.ObserveFunc(ctx, name, labels, value)
	}
}

// TenantLedger contains a configurable exact per-tenant accounting callback.
type TenantLedger struct {
	RecordTenantObservationFunc func(context.Context, domain.Observation) error
}

// RecordTenantObservation invokes RecordTenantObservationFunc when configured.
func (ledger *TenantLedger) RecordTenantObservation(ctx context.Context, observation domain.Observation) error {
	if ledger.RecordTenantObservationFunc != nil {
		return ledger.RecordTenantObservationFunc(ctx, observation)
	}
	return nil
}

// AuditSink contains a configurable decision record callback.
type AuditSink struct {
	RecordFunc func(context.Context, domain.DecisionRecord) error
}

// Record invokes RecordFunc when configured.
func (sink *AuditSink) Record(ctx context.Context, event domain.DecisionRecord) error {
	if sink.RecordFunc != nil {
		return sink.RecordFunc(ctx, event)
	}
	return nil
}

var _ contract.ObservationRecorder = (*ObservationRecorder)(nil)
var _ contract.MetricLabelSink = (*MetricLabelSink)(nil)
var _ contract.TenantLedger = (*TenantLedger)(nil)
var _ contract.AuditSink = (*AuditSink)(nil)
