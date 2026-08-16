package modelgateway

import (
	"context"
	"errors"
	"time"

	"github.com/nauticana/scout/domain"
)

// modelCall is the timeline of one gateway invocation; it emits exactly one
// observation and the admission/completion serving samples for the route.
type modelCall struct {
	gateway       *Gateway
	selection     domain.ModelSelection
	request       domain.ModelRequest
	started       time.Time
	queueWait     time.Duration
	prefillTokens int64
	firstFrame    time.Time
	lastFrame     time.Time
	done          bool
}

func (call *modelCall) admissionRejected(ctx context.Context, err error) {
	call.sample(ctx, domain.ServingSample{Selection: call.selection, AdmissionRejected: true, CapacityOutcome: CapacityOutcomeRejected})
	call.observe(ctx, domain.Usage{}, domain.OutcomeRejected, err)
}

func (call *modelCall) reject(ctx context.Context, err error) {
	call.queueWait = call.gateway.now().Sub(call.started)
	call.sample(ctx, domain.ServingSample{Selection: call.selection, QueueWait: call.queueWait, CapacityOutcome: CapacityOutcomeRejected})
	call.observe(ctx, domain.Usage{}, outcomeOf(err), err)
}

func (call *modelCall) granted(ctx context.Context, selection domain.ModelSelection) {
	call.selection = selection
	call.queueWait = call.gateway.now().Sub(call.started)
	call.sample(ctx, domain.ServingSample{
		Selection: selection, QueueWait: call.queueWait, CapacityOutcome: CapacityOutcomeGranted,
		PrefillTokens: call.prefillTokens, DecodeTokens: call.request.MaxOutputTokens,
	})
}

func (call *modelCall) frame(chunk domain.ModelChunk) {
	if len(chunk.Payload) == 0 && chunk.Usage.OutputTokens == 0 {
		return
	}
	now := call.gateway.now()
	if call.firstFrame.IsZero() {
		call.firstFrame = now
	}
	call.lastFrame = now
}

func (call *modelCall) finish(ctx context.Context, usage domain.Usage, err error) {
	if call.done {
		return
	}
	call.done = true
	outcome := outcomeOf(err)
	sample := domain.ServingSample{Selection: call.selection, CapacityOutcome: CapacityOutcomeCompleted}
	if outcome == domain.OutcomeCanceled {
		sample.CapacityOutcome = CapacityOutcomeCanceled
	} else if outcome != domain.OutcomeOK {
		sample.CapacityOutcome = CapacityOutcomeFailed
	}
	if !call.firstFrame.IsZero() {
		sample.TimeToFirstToken = call.firstFrame.Sub(call.started)
		if usage.OutputTokens > 1 {
			sample.TimePerOutputToken = call.lastFrame.Sub(call.firstFrame) / time.Duration(usage.OutputTokens-1)
		}
	}
	call.sample(ctx, sample)
	call.observe(ctx, usage, outcome, err, sample.TimeToFirstToken, sample.TimePerOutputToken)
}

func (call *modelCall) sample(ctx context.Context, sample domain.ServingSample) {
	if call.gateway.Signals != nil {
		call.gateway.Signals.ObserveServing(ctx, sample)
	}
}

func (call *modelCall) observe(ctx context.Context, usage domain.Usage, outcome domain.ObservationOutcome, err error, latency ...time.Duration) {
	if call.gateway.Observer == nil {
		return
	}
	observation := domain.Observation{
		TenantID:   call.request.TenantContext.TenantID,
		TenantTier: call.request.TenantContext.Tier,
		Region:     call.request.TenantContext.Region,
		Stage:      domain.StageModel,
		Component:  "modelgateway",
		Versions:   domain.ComponentVersions{Model: call.selection.ModelVersion},
		Selection:  call.selection,
		StartedAt:  call.started,
		Duration:   call.gateway.now().Sub(call.started),
		QueueWait:  call.queueWait,
		Usage:      usage,
		Outcome:    outcome,
		ErrorClass: errorClass(err),
	}
	if len(latency) == 2 {
		observation.TimeToFirst, observation.TimePerOutput = latency[0], latency[1]
	}
	call.gateway.Observer.RecordObservation(ctx, observation)
}

func outcomeOf(err error) domain.ObservationOutcome {
	switch {
	case err == nil:
		return domain.OutcomeOK
	case errors.Is(err, context.Canceled):
		return domain.OutcomeCanceled
	case errors.Is(err, domain.ErrRateLimited), errors.Is(err, domain.ErrBudgetExceeded), errors.Is(err, domain.ErrValidation),
		errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrNoRoute):
		return domain.OutcomeRejected
	}
	return domain.OutcomeError
}
