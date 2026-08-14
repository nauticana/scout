package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type leasedModelStream struct {
	receiveMu  sync.Mutex
	mu         sync.Mutex
	stream     contract.ModelStream
	lease      contract.CapacityLease
	releaseCtx context.Context
	usage      domain.Usage
	closed     bool
}

func (stream *leasedModelStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	stream.receiveMu.Lock()
	defer stream.receiveMu.Unlock()
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return domain.ModelChunk{}, io.EOF
	}
	stream.mu.Unlock()
	chunk, receiveErr := stream.stream.Receive(ctx)
	usageErr := stream.addUsage(chunk.Usage)
	if receiveErr != nil || usageErr != nil || chunk.FinishReason != "" {
		finishErr := stream.finish(ctx)
		return chunk, errors.Join(receiveErr, usageErr, finishErr)
	}
	return chunk, nil
}

func (stream *leasedModelStream) Close() error {
	stream.receiveMu.Lock()
	defer stream.receiveMu.Unlock()
	return stream.finish(stream.releaseCtx)
}

func (stream *leasedModelStream) addUsage(usage domain.Usage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostMinorUnits < 0 {
		return fmt.Errorf("%w: model stream usage cannot be negative", domain.ErrValidation)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if usage.Currency != "" && stream.usage.Currency != "" && usage.Currency != stream.usage.Currency {
		return fmt.Errorf("%w: model stream usage currencies differ", domain.ErrValidation)
	}
	stream.usage.InputTokens += usage.InputTokens
	stream.usage.OutputTokens += usage.OutputTokens
	stream.usage.ToolCalls += usage.ToolCalls
	stream.usage.CostMinorUnits += usage.CostMinorUnits
	if stream.usage.Currency == "" {
		stream.usage.Currency = usage.Currency
	}
	return nil
}

func (stream *leasedModelStream) finish(ctx context.Context) error {
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return nil
	}
	stream.closed = true
	usage := stream.usage
	stream.mu.Unlock()
	return errors.Join(stream.stream.Close(), stream.lease.Release(context.WithoutCancel(ctx), usage))
}

var _ contract.ModelStream = (*leasedModelStream)(nil)
