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
	Now                 func() time.Time
}

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
			err := pump.publish(ctx, turn, route, agentVersion, sequence, nil, true, ErrorCodeTruncated)
			return usage, stage.At(domain.StagePublish, err)
		}
		done := errors.Is(receiveErr, io.EOF) || chunk.FinishReason != ""
		if len(chunk.Payload) > 0 || done {
			guarded, err := pump.Guardrails.AfterModelChunk(ctx, config, chunk)
			if err != nil {
				return usage, pump.fail(ctx, turn, route, agentVersion, sequence, domain.StageGuardrail, err)
			}
			if err := pump.publish(ctx, turn, route, agentVersion, sequence, guarded.Payload, done, ""); err != nil {
				return usage, stage.At(domain.StagePublish, err)
			}
			sequence++
		}
		if done {
			return usage, nil
		}
		if pump.MaxOutputTokens > 0 && usage.OutputTokens >= pump.MaxOutputTokens {
			err := pump.publish(ctx, turn, route, agentVersion, sequence, nil, true, ErrorCodeTruncated)
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
	publishErr := pump.publish(publishCtx, turn, route, agentVersion, sequence, nil, true, string(turnStage))
	return errors.Join(stage.At(turnStage, cause), publishErr)
}

func (pump *StreamPump) publish(ctx context.Context, turn domain.TurnRequest, route, agentVersion string, sequence int64, payload []byte, final bool, errorCode string) error {
	now := pump.Now
	if now == nil {
		now = time.Now
	}
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
		EmittedAt:      now(),
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
