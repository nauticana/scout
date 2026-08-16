package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// ErrorCodeTruncated marks a final frame cut off at MaxOutputTokens.
const ErrorCodeTruncated = "max_output_tokens"

// StreamPump forwards one model stream to a reply route: every frame passes
// the guardrail before publication, a publish failure cancels generation, and
// attempts one final frame on every non-publication terminal path.
type StreamPump struct {
	Guardrails contract.GuardrailEnforcer
	Publisher  contract.TurnReplyPublisher
	// MaxOutputTokens truncates the stream after the counted budget; zero is unlimited.
	MaxOutputTokens int64
	// FinalPublishTimeout bounds the terminal best-effort publish; default 1s.
	FinalPublishTimeout time.Duration
	// Observer receives one model-stage observation per run with TTFT and TPOT; nil skips.
	Observer contract.ObservationRecorder
	Now      func() time.Time
}

// StreamPumpComponent names the pump in observations.
const StreamPumpComponent = "stream_pump"

// Run pumps the stream until completion, truncation, or failure, returning accumulated usage.
func (pump *StreamPump) Run(ctx context.Context, turn domain.TurnRequest, route, agentVersion string, config domain.GuardrailConfig, stream contract.ModelStream) (domain.Usage, error) {
	if pump.Guardrails == nil || pump.Publisher == nil {
		return domain.Usage{}, fmt.Errorf("stream pump: guardrails and publisher are required")
	}
	if turn.TenantContext.TenantID <= 0 || strings.TrimSpace(turn.RequestID) == "" || strings.TrimSpace(route) == "" {
		return domain.Usage{}, fmt.Errorf("%w: tenant, request, and route are required", domain.ErrValidation)
	}
	if stream == nil {
		return domain.Usage{}, fmt.Errorf("%w: model stream is required", domain.ErrValidation)
	}
	defer stream.Close()

	span := stage.Begin(pump.clock()(), domain.StageModel, StreamPumpComponent, domain.ComponentVersions{Agent: agentVersion})
	span.Observation.TenantID = turn.TenantContext.TenantID
	span.Observation.TenantTier = turn.TenantContext.Tier
	span.Observation.PriorityClass = turn.TenantContext.PriorityClass
	span.Observation.Region = turn.TenantContext.Region
	timing := &streamTiming{}
	usage, err := pump.run(ctx, turn, route, agentVersion, config, stream, timing)
	if pump.Observer != nil {
		var outcome domain.ObservationOutcome
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			outcome = domain.OutcomeCanceled
		}
		timing.apply(&span.Observation, usage.OutputTokens)
		pump.Observer.RecordObservation(ctx, span.End(pump.clock()(), outcome, usage, err))
	}
	return usage, err
}

// streamTiming captures first and last approved frame times for TTFT and TPOT.
type streamTiming struct {
	first, last time.Time
	frames      int
}

func (timing *streamTiming) published(now time.Time) {
	if timing.frames == 0 {
		timing.first = now
	}
	timing.last = now
	timing.frames++
}

func (timing *streamTiming) apply(observation *domain.Observation, outputTokens int64) {
	if timing.frames == 0 {
		return
	}
	observation.TimeToFirst = timing.first.Sub(observation.StartedAt)
	if outputTokens > 1 && timing.frames > 1 {
		observation.TimePerOutput = timing.last.Sub(timing.first) / time.Duration(outputTokens-1)
	}
}

func (pump *StreamPump) run(ctx context.Context, turn domain.TurnRequest, route, agentVersion string, config domain.GuardrailConfig, stream contract.ModelStream, timing *streamTiming) (domain.Usage, error) {
	var usage domain.Usage
	var sequence int64
	for {
		chunk, receiveErr := stream.Receive(ctx)
		if err := addChunkUsage(&usage, chunk.Usage); err != nil {
			return usage, pump.fail(ctx, turn, route, agentVersion, sequence, domain.StageModel, err)
		}
		if receiveErr != nil && !errors.Is(receiveErr, io.EOF) {
			return usage, pump.fail(ctx, turn, route, agentVersion, sequence, domain.StageModel, receiveErr)
		}
		if pump.MaxOutputTokens > 0 && usage.OutputTokens > pump.MaxOutputTokens {
			err := pump.publish(ctx, turn, route, agentVersion, sequence, nil, true, ErrorCodeTruncated, pump.clock()())
			return usage, stage.At(domain.StagePublish, err)
		}
		done := errors.Is(receiveErr, io.EOF) || chunk.FinishReason != ""
		if len(chunk.Payload) > 0 || done {
			guarded, err := pump.Guardrails.AfterModelChunk(ctx, config, chunk)
			if err != nil {
				return usage, pump.fail(ctx, turn, route, agentVersion, sequence, domain.StageGuardrail, err)
			}
			emittedAt := pump.clock()()
			if err := pump.publish(ctx, turn, route, agentVersion, sequence, guarded.Payload, done, "", emittedAt); err != nil {
				return usage, stage.At(domain.StagePublish, err)
			}
			timing.published(emittedAt)
			sequence++
		}
		if done {
			return usage, nil
		}
		if pump.MaxOutputTokens > 0 && usage.OutputTokens >= pump.MaxOutputTokens {
			err := pump.publish(ctx, turn, route, agentVersion, sequence, nil, true, ErrorCodeTruncated, pump.clock()())
			return usage, stage.At(domain.StagePublish, err)
		}
	}
}

// fail publishes a payload-free final error frame so the client always sees an end.
func (pump *StreamPump) fail(ctx context.Context, turn domain.TurnRequest, route, agentVersion string, sequence int64, turnStage domain.TurnStage, cause error) error {
	timeout := pump.FinalPublishTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	publishErr := pump.publish(publishCtx, turn, route, agentVersion, sequence, nil, true, string(turnStage), pump.clock()())
	return errors.Join(stage.At(turnStage, cause), publishErr)
}

func (pump *StreamPump) clock() func() time.Time {
	if pump.Now == nil {
		return time.Now
	}
	return pump.Now
}

func (pump *StreamPump) publish(ctx context.Context, turn domain.TurnRequest, route, agentVersion string, sequence int64, payload []byte, final bool, errorCode string, emittedAt time.Time) error {
	return pump.Publisher.Publish(ctx, domain.TurnReply{
		TenantID:       turn.TenantContext.TenantID,
		RequestID:      turn.RequestID,
		ConversationID: turn.ConversationID,
		ReplyRoute:     route,
		Sequence:       sequence,
		Payload:        payload,
		Final:          final,
		ErrorCode:      errorCode,
		AgentVersion:   agentVersion,
		EmittedAt:      emittedAt,
	})
}

func addChunkUsage(total *domain.Usage, usage domain.Usage) error {
	const maxInt64 = int64(^uint64(0) >> 1)
	const maxInt = int(^uint(0) >> 1)
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostMinorUnits < 0 {
		return fmt.Errorf("%w: stream usage cannot be negative", domain.ErrValidation)
	}
	if total.InputTokens > maxInt64-usage.InputTokens || total.OutputTokens > maxInt64-usage.OutputTokens ||
		total.ToolCalls > maxInt-usage.ToolCalls || total.CostMinorUnits > maxInt64-usage.CostMinorUnits {
		return fmt.Errorf("%w: stream usage overflow", domain.ErrValidation)
	}
	if usage.CostMinorUnits > 0 && len(usage.Currency) != 3 {
		return fmt.Errorf("%w: stream cost requires a currency", domain.ErrValidation)
	}
	if total.Currency != "" && usage.Currency != "" && total.Currency != usage.Currency {
		return fmt.Errorf("%w: stream usage currencies differ", domain.ErrValidation)
	}
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.ToolCalls += usage.ToolCalls
	total.CostMinorUnits += usage.CostMinorUnits
	if total.Currency == "" {
		total.Currency = usage.Currency
	}
	return nil
}
