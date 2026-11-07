package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"

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
	if got := maxOutputTokens(domain.ModelRequest{}); got != DefaultMaxOutputTokens {
		t.Errorf("maxOutputTokens default = %d", got)
	}
	if got := maxOutputTokens(domain.ModelRequest{MaxOutputTokens: 512}); got != 512 {
		t.Errorf("maxOutputTokens override = %d", got)
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

func TestGoogleVideoLocationSeparatesOnlyVideo(t *testing.T) {
	global := &Google{ProjectID: "p", Location: "global", VideoLocation: "us-central1"}
	if config := global.clientConfig(global.Location); config.Location != "global" || global.separateVideoLocation() != "us-central1" {
		t.Fatalf("location = %q, video = %q", config.Location, global.separateVideoLocation())
	}
	for name, unseparated := range map[string]*Google{
		"unset":      {Location: "global"},
		"same":       {Location: "us-central1", VideoLocation: "us-central1"},
		"gemini api": {UseGeminiAPI: true, VideoLocation: "us-central1"},
	} {
		if location := unseparated.separateVideoLocation(); location != "" {
			t.Errorf("%s: video location = %q", name, location)
		}
	}
	got := withVideoModels([]string{"gemini-2", "veo-3"}, []string{"gemini-regional", "veo-3", "veo-3.1-fast-generate-001"})
	if !slices.Equal(got, []string{"gemini-2", "veo-3", "veo-3.1-fast-generate-001"}) {
		t.Fatalf("models = %v", got)
	}
}

func TestPromptCacheMarksThePrefixAndKeepsCacheTokensOutOfInput(t *testing.T) {
	cached := domain.ModelRequest{Instructions: []byte("rules"), Prompt: []byte("task"), PromptCache: true, MaxOutputTokens: 1}
	params, err := (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, cached)
	if err != nil || params.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("cache control = %+v, %v", params.System, err)
	}
	cached.PromptCache = false
	if params, err = (&Anthropic{}).messageParams(domain.ModelSelection{Model: "m"}, cached); err != nil || params.System[0].CacheControl.Type != "" {
		t.Fatalf("uncached = %+v, %v", params.System, err)
	}
	cached.PromptCache = true

	var message anthropic.Message
	if err = json.Unmarshal([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":90,"cache_creation_input_tokens":40}}`), &message); err != nil {
		t.Fatal(err)
	}
	result, err := anthropicResult(&message, cached)
	if err != nil || result.Usage.InputTokens != 5 || result.Usage.CacheReadTokens != 90 || result.Usage.CacheWriteTokens != 40 {
		t.Fatalf("anthropic usage = %+v, %v", result.Usage, err)
	}
	if result, err = anthropicResult(&message, domain.ModelRequest{}); err != nil || result.Usage.InputTokens != 135 || result.Usage.CacheReadTokens != 0 || result.Usage.CacheWriteTokens != 0 {
		t.Fatalf("an uncached request bills cache tokens as input: %+v, %v", result.Usage, err)
	}

	var completion openai.ChatCompletion
	if err = json.Unmarshal([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":60}}}`), &completion); err != nil {
		t.Fatal(err)
	}
	if result, err = openAIResult(OpenAIProviderID, &completion, cached); err != nil || result.Usage.InputTokens != 40 || result.Usage.CacheReadTokens != 60 {
		t.Fatalf("openai usage = %+v, %v", result.Usage, err)
	}
	if result, err = openAIResult(OpenAIProviderID, &completion, domain.ModelRequest{}); err != nil || result.Usage.InputTokens != 100 || result.Usage.CacheReadTokens != 0 {
		t.Fatalf("an uncached request keeps its input whole: %+v, %v", result.Usage, err)
	}

	gemini := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: genai.NewContentFromText("ok", genai.RoleModel)}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100, CandidatesTokenCount: 2, CachedContentTokenCount: 70}}
	if result, err = googleResult(gemini, cached); err != nil || result.Usage.InputTokens != 30 || result.Usage.CacheReadTokens != 70 {
		t.Fatalf("google usage = %+v, %v", result.Usage, err)
	}
}
