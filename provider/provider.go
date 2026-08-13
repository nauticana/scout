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
	// DefaultTemperature is used when an adapter has no positive configured value.
	DefaultTemperature = 0.7
)

func maxOutputTokens(request domain.ModelRequest) int64 {
	if request.MaxOutputTokens > 0 {
		return request.MaxOutputTokens
	}
	return DefaultMaxOutputTokens
}

func temperature(configured float64, explicitlyConfigured bool) float64 {
	if explicitlyConfigured {
		return configured
	}
	if configured > 0 {
		return configured
	}
	return DefaultTemperature
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

// singleFrameStream adapts a non-streaming provider: the whole completion
// arrives as one frame, then io.EOF. Callers get correct ordering and usage
// without the adapter pretending to deliver incremental tokens.
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
		Sequence: 1, Payload: s.result.Output,
		FinishReason: s.result.FinishReason, Usage: s.result.Usage,
	}, nil
}

func (s *completedStream) Close() error { return nil }
