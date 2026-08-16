package guardrail

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// outputSession inspects one ordered stream, holding at most lookback-1 bytes so a phrase or
// bounded regex spanning two chunks is still caught before either half is released.
type outputSession struct {
	enforcer *LayeredEnforcer
	config   domain.GuardrailConfig
	subject  domain.GuardrailSubject
	lookback int

	mu       sync.Mutex
	pending  []byte
	total    int
	sequence int64
	closed   bool
	failure  error
}

var _ contract.GuardrailOutputSession = (*outputSession)(nil)

func (session *outputSession) Inspect(ctx context.Context, chunk domain.ModelChunk) (domain.ModelChunk, bool, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.usable(ctx); err != nil {
		return domain.ModelChunk{}, false, err
	}
	if len(chunk.Payload) > session.enforcer.maxChunkBytes {
		return domain.ModelChunk{}, false, fmt.Errorf("%w: stream chunk exceeds %d bytes", domain.ErrValidation, session.enforcer.maxChunkBytes)
	}
	session.sequence = chunk.Sequence
	final := chunk.FinishReason != ""
	released, err := session.advance(ctx, chunk.Payload, final)
	if err != nil {
		return domain.ModelChunk{}, false, err
	}
	approved := domain.ModelChunk{Sequence: chunk.Sequence, Payload: released, FinishReason: chunk.FinishReason, Usage: chunk.Usage}
	return approved, len(session.pending) > 0, nil
}

func (session *outputSession) Flush(ctx context.Context) ([]domain.ModelChunk, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.usable(ctx); err != nil {
		return nil, err
	}
	if len(session.pending) == 0 {
		return nil, nil
	}
	released, err := session.advance(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	if len(released) == 0 {
		return nil, nil
	}
	session.sequence++
	return []domain.ModelChunk{{Sequence: session.sequence, Payload: released}}, nil
}

// Close discards held bytes; a closed session releases nothing further.
func (session *outputSession) Close() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.closed = true
	session.pending = nil
	return nil
}

func (session *outputSession) usable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session.failure != nil {
		return session.failure
	}
	if session.closed {
		return fmt.Errorf("%w: guardrail output session is closed", domain.ErrConflict)
	}
	return nil
}

// advance scans the held tail plus new bytes, then releases everything but the tail unless final.
func (session *outputSession) advance(ctx context.Context, payload []byte, final bool) ([]byte, error) {
	from := len(session.pending)
	session.pending = append(session.pending, payload...)
	session.total += len(payload)
	in := &inspection{stage: domain.GuardrailStageOutput, subject: session.subject, content: session.pending, sizeBytes: session.total, from: from}
	content, _, err := session.enforcer.inspect(ctx, session.config, in)
	if err != nil {
		session.pending = nil
		if _, violated := asViolation(err); violated {
			session.failure = err
		}
		return nil, err
	}
	hold := session.lookback - 1
	if final || hold <= 0 || len(content) <= hold {
		if final || hold <= 0 {
			session.pending = nil
			return content, nil
		}
		session.pending = content
		return nil, nil
	}
	released := append([]byte(nil), content[:len(content)-hold]...)
	session.pending = append([]byte(nil), content[len(content)-hold:]...)
	return released, nil
}

func asViolation(err error) (*ViolationError, bool) {
	var violation *ViolationError
	if errors.As(err, &violation) {
		return violation, true
	}
	return nil, false
}
