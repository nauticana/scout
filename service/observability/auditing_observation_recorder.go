package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AuditCategoryObservation is the audit category of rejected and failed observations.
const AuditCategoryObservation = "observation"

// AuditingObservationRecorder decorates a recorder with a redacted audit trail
// for every rejected or failed observation.
type AuditingObservationRecorder struct {
	next    contract.ObservationRecorder
	audit   contract.AuditSink
	onError func(error)
	now     func() time.Time
}

var _ contract.ObservationRecorder = (*AuditingObservationRecorder)(nil)

// NewAuditingObservationRecorder wraps next; onError receives audit write failures and is required.
func NewAuditingObservationRecorder(next contract.ObservationRecorder, audit contract.AuditSink, onError func(error), now func() time.Time) (*AuditingObservationRecorder, error) {
	if next == nil || audit == nil || onError == nil {
		return nil, fmt.Errorf("auditing observation recorder: next, audit sink, and error handler are required")
	}
	if now == nil {
		now = time.Now
	}
	return &AuditingObservationRecorder{next: next, audit: audit, onError: onError, now: now}, nil
}

// RecordObservation forwards every observation and audits rejected or failed ones.
func (recorder *AuditingObservationRecorder) RecordObservation(ctx context.Context, observation domain.Observation) {
	recorder.next.RecordObservation(ctx, observation)
	if observation.Outcome != domain.OutcomeRejected && observation.Outcome != domain.OutcomeError {
		return
	}
	payload, err := json.Marshal(auditPayload{
		Stage:          observation.Stage,
		Component:      observation.Component,
		Versions:       observation.Versions,
		Provider:       observation.Selection.Provider,
		Model:          observation.Selection.Model,
		ModelVersion:   observation.Selection.ModelVersion,
		Region:         observation.Region,
		TenantTier:     observation.TenantTier,
		Outcome:        observation.Outcome,
		ErrorClass:     observation.ErrorClass,
		DurationMillis: observation.Duration.Milliseconds(),
		InputTokens:    observation.Usage.InputTokens,
		OutputTokens:   observation.Usage.OutputTokens,
		TraceID:        observation.TraceID,
	})
	if err != nil {
		recorder.onError(fmt.Errorf("auditing observation recorder: %w", err))
		return
	}
	event := domain.AuditEvent{TenantID: observation.TenantID, Category: AuditCategoryObservation, Payload: payload, OccurredAt: recorder.now()}
	if err := recorder.audit.Record(ctx, event); err != nil {
		recorder.onError(fmt.Errorf("auditing observation recorder: %w", err))
	}
}

// auditPayload is the redacted subset written to the audit trail: no ids, prompts, or free text.
type auditPayload struct {
	Stage          domain.TurnStage          `json:"stage"`
	Component      string                    `json:"component"`
	Versions       domain.ComponentVersions  `json:"versions"`
	Provider       string                    `json:"provider,omitempty"`
	Model          string                    `json:"model,omitempty"`
	ModelVersion   string                    `json:"model_version,omitempty"`
	Region         string                    `json:"region,omitempty"`
	TenantTier     string                    `json:"tenant_tier,omitempty"`
	Outcome        domain.ObservationOutcome `json:"outcome"`
	ErrorClass     string                    `json:"error_class"`
	DurationMillis int64                     `json:"duration_ms"`
	InputTokens    int64                     `json:"input_tokens"`
	OutputTokens   int64                     `json:"output_tokens"`
	TraceID        string                    `json:"trace_id,omitempty"`
}
