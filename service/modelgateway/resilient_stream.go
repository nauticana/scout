package modelgateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// resilientStream enforces first-token, idle, total, and drain deadlines on one
// logical stream; before the first token it may reopen the provider stream.
type resilientStream struct {
	gateway   *ResilientGateway
	streamCtx context.Context
	selection domain.ModelSelection
	request   domain.ModelRequest
	started   time.Time

	receiveMu  sync.Mutex
	mu         sync.Mutex
	inner      contract.ModelStream
	cancel     context.CancelFunc
	attempts   int
	firstToken bool
	lastFrame  time.Time
	sequence   int64
	finished   bool
}

var _ contract.ModelStream = (*resilientStream)(nil)

type openedStream struct {
	stream contract.ModelStream
	err    error
}

// open dials the provider within the first-token budget, retrying retryable
// failures up to MaxPreTokenRetries.
func (stream *resilientStream) open() error {
	gateway := stream.gateway
	for {
		kind, remaining := stream.budget(gateway.now(), time.Time{})
		if remaining <= 0 {
			return stageModel(&StreamDeadlineError{Kind: kind, Limit: stream.limitOf(kind)})
		}
		attemptCtx, cancel := context.WithCancel(stream.streamCtx)
		opened := stream.dial(attemptCtx, cancel, remaining, kind)
		if opened.err == nil {
			stream.mu.Lock()
			stream.inner, stream.cancel = opened.stream, cancel
			stream.mu.Unlock()
			return nil
		}
		cancel()
		if !retryable(opened.err) || stream.attempts >= gateway.MaxPreTokenRetries {
			return stageModel(opened.err)
		}
		stream.attempts++
		_, totalLeft := stream.totalBudget(gateway.now())
		if err := gateway.backoff(stream.streamCtx, stream.attempts, totalLeft); err != nil {
			return stageModel(errors.Join(opened.err, err))
		}
	}
}

// dial runs Inner.Stream under the attempt context and bounds the wait; on
// timeout the attempt is canceled and a late stream is closed so nothing leaks.
func (stream *resilientStream) dial(attemptCtx context.Context, cancel context.CancelFunc, remaining time.Duration, kind DeadlineKind) openedStream {
	gateway := stream.gateway
	done := make(chan openedStream, 1)
	go func() {
		opened, err := gateway.Inner.Stream(attemptCtx, stream.selection, stream.request)
		if err == nil && opened == nil {
			err = errors.New("model gateway returned a nil stream")
		}
		// A provider may return after cancellation. Close that late stream here
		// so the deadline path never waits for a non-cooperative provider.
		if attemptCtx.Err() != nil && opened != nil {
			err = errors.Join(err, opened.Close())
			opened = nil
		}
		done <- openedStream{stream: opened, err: err}
	}()
	waitCtx, stopWait := context.WithTimeout(stream.streamCtx, remaining)
	defer stopWait()
	select {
	case opened := <-done:
		return opened
	case <-waitCtx.Done():
	}
	cancel()
	if err := stream.streamCtx.Err(); err != nil {
		return openedStream{err: err}
	}
	return openedStream{err: &StreamDeadlineError{Kind: kind, Limit: stream.limitOf(kind)}}
}

func (stream *resilientStream) limitOf(kind DeadlineKind) time.Duration {
	switch kind {
	case DeadlineFirstToken:
		return stream.gateway.Deadlines.FirstToken
	case DeadlineIdle:
		return stream.gateway.Deadlines.Idle
	case DeadlineDrain:
		return 0
	}
	return stream.gateway.Deadlines.Total
}

func (stream *resilientStream) totalBudget(now time.Time) (DeadlineKind, time.Duration) {
	return DeadlineTotal, stream.gateway.Deadlines.Total - now.Sub(stream.started)
}

// budget returns the binding deadline and how much of it remains.
func (stream *resilientStream) budget(now, drainDeadline time.Time) (DeadlineKind, time.Duration) {
	kind, remaining := stream.totalBudget(now)
	if !stream.firstToken {
		if left := stream.gateway.Deadlines.FirstToken - now.Sub(stream.started); left < remaining {
			kind, remaining = DeadlineFirstToken, left
		}
	} else if left := stream.gateway.Deadlines.Idle - now.Sub(stream.lastFrame); left < remaining {
		kind, remaining = DeadlineIdle, left
	}
	if !drainDeadline.IsZero() {
		if left := drainDeadline.Sub(now); left < remaining {
			kind, remaining = DeadlineDrain, left
		}
	}
	return kind, remaining
}

func (stream *resilientStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	stream.receiveMu.Lock()
	defer stream.receiveMu.Unlock()
	gateway := stream.gateway
	for {
		stream.mu.Lock()
		finished, inner := stream.finished, stream.inner
		stream.mu.Unlock()
		if finished {
			return domain.ModelChunk{}, io.EOF
		}
		now := gateway.now()
		drainDeadline, err := gateway.drainDeadline(ctx, stream.selection)
		if err != nil {
			return stream.fail(err)
		}
		kind, remaining := stream.budget(now, drainDeadline)
		if remaining <= 0 {
			return stream.fail(&StreamDeadlineError{Kind: kind, Limit: stream.limitOf(kind)})
		}
		receiveCtx, cancel := context.WithTimeout(ctx, remaining)
		chunk, receiveErr := inner.Receive(receiveCtx)
		cancel()
		if receiveErr == nil {
			return stream.accept(chunk), nil
		}
		if errors.Is(receiveErr, io.EOF) {
			return chunk, errors.Join(io.EOF, stream.finish())
		}
		receiveErr = gateway.classify(ctx, receiveCtx, receiveErr, kind, stream.limitOf(kind))
		if stream.firstToken || !retryable(receiveErr) || stream.attempts >= gateway.MaxPreTokenRetries {
			return stream.fail(receiveErr)
		}
		stream.attempts++
		stream.mu.Lock()
		cancelAttempt := stream.cancel
		stream.inner, stream.cancel = nil, nil
		stream.mu.Unlock()
		closeErr := inner.Close()
		if cancelAttempt != nil {
			cancelAttempt()
		}
		if err := errors.Join(closeErr, gateway.backoff(stream.streamCtx, stream.attempts, remaining)); err != nil {
			return stream.fail(errors.Join(receiveErr, err))
		}
		if err := stream.open(); err != nil {
			return stream.fail(err)
		}
	}
}

func (stream *resilientStream) accept(chunk domain.ModelChunk) domain.ModelChunk {
	stream.mu.Lock()
	if len(chunk.Payload) > 0 || chunk.Usage.OutputTokens > 0 {
		stream.firstToken = true
	}
	stream.lastFrame = stream.gateway.now()
	stream.sequence = chunk.Sequence
	stream.mu.Unlock()
	if chunk.FinishReason != "" {
		_ = stream.finish()
	}
	return chunk
}

// fail ends the stream: before the first token only the error is returned; after
// it the caller receives an explicit interrupted partial completion.
func (stream *resilientStream) fail(cause error) (domain.ModelChunk, error) {
	stream.mu.Lock()
	firstToken, sequence := stream.firstToken, stream.sequence
	stream.mu.Unlock()
	err := errors.Join(stageModel(cause), stream.finish())
	if !firstToken {
		return domain.ModelChunk{}, err
	}
	return domain.ModelChunk{Sequence: sequence + 1, FinishReason: domain.FinishReasonInterrupted}, err
}

func (stream *resilientStream) finish() error {
	stream.mu.Lock()
	if stream.finished {
		stream.mu.Unlock()
		return nil
	}
	stream.finished = true
	inner, cancel := stream.inner, stream.cancel
	stream.inner, stream.cancel = nil, nil
	stream.mu.Unlock()
	var err error
	if inner != nil {
		err = inner.Close()
	}
	if cancel != nil {
		cancel()
	}
	return err
}

func (stream *resilientStream) Close() error {
	stream.receiveMu.Lock()
	defer stream.receiveMu.Unlock()
	return stream.finish()
}
