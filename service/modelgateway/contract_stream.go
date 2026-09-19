package modelgateway

import (
	"context"
	"errors"
	"io"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// contractStream holds streamed frames to the same contract a unary result
// meets: tool calls per frame, the constrained answer once the stream ends.
type contractStream struct {
	stream      contract.ModelStream
	contract    *modelContract
	output      []byte
	calledTools bool
	checked     bool
}

func (stream *contractStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	chunk, err := stream.stream.Receive(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return chunk, errors.Join(stream.checkTerminal(), err)
		}
		return chunk, err
	}
	if err := stream.contract.checkToolCalls(chunk.ToolCalls); err != nil {
		return domain.ModelChunk{}, err
	}
	stream.calledTools = stream.calledTools || len(chunk.ToolCalls) > 0
	if stream.contract.output != nil {
		stream.output = append(stream.output, chunk.Payload...)
	}
	if chunk.FinishReason != "" && chunk.FinishReason != domain.FinishReasonInterrupted {
		if err := stream.checkTerminal(); err != nil {
			return domain.ModelChunk{}, err
		}
	}
	return chunk, nil
}

func (stream *contractStream) checkTerminal() error {
	if stream.checked {
		return nil
	}
	stream.checked = true
	return stream.contract.checkTerminal(stream.output, stream.calledTools)
}

func (stream *contractStream) Close() error { return stream.stream.Close() }

var _ contract.ModelStream = (*contractStream)(nil)
