// Package provider holds concrete inference adapters behind Scout's
// contract.ModelProvider and contract.MediaProvider. Credentials, endpoints,
// and sampling defaults are injected at construction; adapters never read
// configuration or price a call.
package provider

import (
	"context"
	"io"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	// GoogleProviderID is the canonical model-provider id for Google's adapter.
	GoogleProviderID = "google"
	// OpenAIProviderID is the canonical model-provider id for OpenAI's adapter.
	OpenAIProviderID = "openai"
	// AnthropicProviderID is the canonical model-provider id for Anthropic's adapter.
	AnthropicProviderID = "anthropic"

	// DefaultMaxOutputTokens bounds provider calls whose request omits a limit.
	DefaultMaxOutputTokens int64 = 8192
)

func maxOutputTokens(request domain.ModelRequest) int64 {
	if request.MaxOutputTokens > 0 {
		return request.MaxOutputTokens
	}
	return DefaultMaxOutputTokens
}

func mediaPrompt(prompt, styleHint string) string {
	if styleHint == "" {
		return prompt
	}
	return prompt + ". Style: " + styleHint
}

func atLeastOne(count int32) int32 {
	if count <= 0 {
		return 1
	}
	return count
}

// singleFrameStream falls back to a unary call where a vendor's frames would
// lose part of the answer: the whole completion arrives as one frame, then
// io.EOF, without the adapter pretending to deliver incremental tokens.
func singleFrameStream(ctx context.Context, p contract.ModelProvider, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	result, err := p.Generate(ctx, selection, request)
	if err != nil {
		return nil, err
	}
	return &completedStream{result: result}, nil
}

type completedStream struct {
	result domain.ModelResult
	once   sync.Once
	sent   bool
}

func (s *completedStream) Receive(context.Context) (domain.ModelChunk, error) {
	delivered := false
	s.once.Do(func() { delivered = true; s.sent = true })
	if !delivered {
		return domain.ModelChunk{}, io.EOF
	}
	return domain.ModelChunk{
		Sequence: 1, Payload: s.result.Output, ToolCalls: s.result.ToolCalls,
		FinishReason: s.result.FinishReason, Usage: s.result.Usage,
	}, nil
}

func (s *completedStream) Close() error { return nil }

// eventStream turns a vendor event stream into ordered model frames: one frame
// per text delta, then one terminal frame carrying the tool calls, citations,
// finish reason, and the usage of the whole call.
type eventStream struct {
	// advance returns the next frame; a false second result ends the stream.
	advance  func() (domain.ModelChunk, bool, error)
	shutdown func() error
	sequence int64
	ended    bool
	closed   sync.Once
}

var _ contract.ModelStream = (*eventStream)(nil)

func (s *eventStream) Receive(context.Context) (domain.ModelChunk, error) {
	if s.ended {
		return domain.ModelChunk{}, io.EOF
	}
	chunk, more, err := s.advance()
	if err != nil || !more {
		s.ended = true
		if err != nil {
			return domain.ModelChunk{}, err
		}
		return domain.ModelChunk{}, io.EOF
	}
	s.sequence++
	chunk.Sequence = s.sequence
	return chunk, nil
}

func (s *eventStream) Close() error {
	var err error
	s.closed.Do(func() { err = s.shutdown() })
	return err
}

// terminalFrame carries what only a complete response can report.
func terminalFrame(result domain.ModelResult) domain.ModelChunk {
	return domain.ModelChunk{
		ToolCalls: result.ToolCalls, Citations: result.Citations,
		FinishReason: result.FinishReason, Usage: result.Usage,
	}
}
