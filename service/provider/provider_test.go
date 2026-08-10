package provider

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type stubProvider struct {
	result domain.ModelResult
	err    error
	calls  int
}

func (s *stubProvider) Generate(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
	s.calls++
	return s.result, s.err
}

func (s *stubProvider) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return singleFrameStream(ctx, s, selection, request)
}

// A non-streaming provider still satisfies ModelStream: one frame, then EOF.
func TestSingleFrameStreamDeliversOnceThenEOF(t *testing.T) {
	stub := &stubProvider{result: domain.ModelResult{
		Output: []byte("hello"), FinishReason: "stop",
		Usage: domain.Usage{InputTokens: 3, OutputTokens: 5},
	}}
	stream, err := stub.Stream(context.Background(), domain.ModelSelection{Model: "m"}, domain.ModelRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunk, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	if string(chunk.Payload) != "hello" || chunk.Sequence != 1 || chunk.Usage.OutputTokens != 5 {
		t.Fatalf("chunk = %+v", chunk)
	}
	if _, err = stream.Receive(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("second Receive = %v, want io.EOF", err)
	}
	if err = stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSingleFrameStreamPropagatesGenerateFailure(t *testing.T) {
	broken := errors.New("provider down")
	stub := &stubProvider{err: broken}
	if _, err := stub.Stream(context.Background(), domain.ModelSelection{Model: "m"}, domain.ModelRequest{}); !errors.Is(err, broken) {
		t.Fatalf("Stream err = %v, want the generate failure", err)
	}
}

func TestRequestDefaults(t *testing.T) {
	if got := maxOutputTokens(domain.ModelRequest{}); got != defaultMaxOutputTokens {
		t.Errorf("maxOutputTokens default = %d", got)
	}
	if got := maxOutputTokens(domain.ModelRequest{MaxOutputTokens: 512}); got != 512 {
		t.Errorf("maxOutputTokens override = %d", got)
	}
	if got := temperature(0); got != defaultTemperature {
		t.Errorf("temperature default = %v", got)
	}
	if got := temperature(0.2); got != 0.2 {
		t.Errorf("temperature override = %v", got)
	}
	if got := atLeastOne(0); got != 1 {
		t.Errorf("atLeastOne(0) = %d", got)
	}
	if got := mediaPrompt("a pergola", ""); got != "a pergola" {
		t.Errorf("mediaPrompt without hint = %q", got)
	}
	if got := mediaPrompt("a pergola", "warm"); got != "a pergola. Style: warm" {
		t.Errorf("mediaPrompt with hint = %q", got)
	}
}

// Adapters that cannot serve a modality must say so, not return an empty set.
func TestUnsupportedModalitiesFail(t *testing.T) {
	if _, err := (&OpenAI{}).GenerateVideo(context.Background(), "sora-2", domain.VideoRequest{}); err == nil {
		t.Error("openai video must report that it is unsupported")
	}
	if _, err := (&Google{}).GenerateImage(context.Background(), "", domain.ImageRequest{}); !errors.Is(err, domain.ErrValidation) {
		t.Error("google image without a model must be a validation error")
	}
	if _, err := (&Anthropic{}).Generate(context.Background(), domain.ModelSelection{Model: "m"}, domain.ModelRequest{}); !errors.Is(err, domain.ErrNotReady) {
		t.Error("anthropic without a key must be ErrNotReady")
	}
}
