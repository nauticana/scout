package provider

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicsse "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/openai/openai-go"
	openaisse "github.com/openai/openai-go/packages/ssestream"
	"google.golang.org/genai"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// sseResponse is a canned event stream, as the vendor SDKs decode one off the wire.
func sseResponse(events ...string) *http.Response {
	body := strings.Join(events, "") + "\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func event(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

// drain reads every frame and reports the text delivered incrementally.
func drain(t *testing.T, stream contract.ModelStream) (string, domain.ModelChunk) {
	t.Helper()
	text, frames := "", 0
	var terminal domain.ModelChunk
	for {
		chunk, err := stream.Receive(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		frames++
		if chunk.Sequence != int64(frames) {
			t.Fatalf("frame %d has sequence %d", frames, chunk.Sequence)
		}
		text += string(chunk.Payload)
		terminal = chunk
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return text, terminal
}

func TestAnthropicStreamsTextDeltasThenOneTerminalFrame(t *testing.T) {
	stream := anthropicStream(anthropicsse.NewStream[anthropic.MessageStreamEventUnion](anthropicsse.NewDecoder(sseResponse(
		event("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tial"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
		event("message_stop", `{"type":"message_stop"}`),
	)), nil))

	text, terminal := drain(t, stream)
	if text != "partial" {
		t.Fatalf("streamed text = %q", text)
	}
	if terminal.FinishReason != "end_turn" || terminal.Usage.InputTokens != 5 || terminal.Usage.OutputTokens != 7 {
		t.Fatalf("terminal frame = %+v", terminal)
	}
}

func TestOpenAIStreamsContentDeltasThenOneTerminalFrame(t *testing.T) {
	chunk := func(delta string) string {
		return `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"` + delta + `"}}]}`
	}
	stream := openAIStream(openaisse.NewStream[openai.ChatCompletionChunk](openaisse.NewDecoder(sseResponse(
		event("", chunk("par")),
		event("", chunk("tial")),
		event("", `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`),
		"data: [DONE]\n\n",
	)), nil), domain.ModelRequest{})

	text, terminal := drain(t, stream)
	if text != "partial" {
		t.Fatalf("streamed text = %q", text)
	}
	if terminal.FinishReason != "stop" || terminal.Usage.InputTokens != 5 || terminal.Usage.OutputTokens != 7 {
		t.Fatalf("terminal frame = %+v", terminal)
	}
}

func TestGoogleStreamsTextPartsAndFoldsGroundingIntoTheTerminalFrame(t *testing.T) {
	responses := func(yield func(*genai.GenerateContentResponse, error) bool) {
		chunks := []*genai.GenerateContentResponse{
			{Candidates: []*genai.Candidate{{
				Content: &genai.Content{Parts: []*genai.Part{{Text: "par"}}},
				GroundingMetadata: &genai.GroundingMetadata{
					GroundingChunks: []*genai.GroundingChunk{{Web: &genai.GroundingChunkWeb{URI: "https://a.example/p", Title: "A"}}},
				},
			}},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 3}},
			{Candidates: []*genai.Candidate{{
				Content:      &genai.Content{Parts: []*genai.Part{{Text: "hidden", Thought: true}, {Text: "tial"}}},
				FinishReason: genai.FinishReasonStop,
				GroundingMetadata: &genai.GroundingMetadata{
					WebSearchQueries: []string{"who ranks for x"},
					GroundingSupports: []*genai.GroundingSupport{{
						GroundingChunkIndices: []int32{0}, Segment: &genai.Segment{Text: "partial"},
					}},
				},
			}}, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 7}},
		}
		for _, response := range chunks {
			if !yield(response, nil) {
				return
			}
		}
	}
	text, terminal := drain(t, googleStream(iter.Seq2[*genai.GenerateContentResponse, error](responses), groundedRequest(0)))
	if text != "partial" {
		t.Fatalf("streamed text = %q, a thought part is not answer text", text)
	}
	if terminal.FinishReason != string(genai.FinishReasonStop) || terminal.Usage.OutputTokens != 7 || terminal.Usage.SearchQueries != 1 {
		t.Fatalf("terminal frame = %+v", terminal)
	}
	requireCitation(t, terminal.Citations, 0, "https://a.example/p", "A", "partial")
}

func TestGoogleFoldKeepsSupportsOnTheirOwnChunks(t *testing.T) {
	grounded := func(url, segment string) *genai.GenerateContentResponse {
		return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{GroundingMetadata: &genai.GroundingMetadata{
			GroundingChunks:   []*genai.GroundingChunk{{Web: &genai.GroundingChunkWeb{URI: url}}},
			GroundingSupports: []*genai.GroundingSupport{{GroundingChunkIndices: []int32{0}, Segment: &genai.Segment{Text: segment}}},
		}}}}
	}
	aggregate := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{}}}}
	googleFold(aggregate, grounded("https://a.example/p", "about a"))
	googleFold(aggregate, grounded("https://b.example/p", "about b"))
	citations := googleCitations(aggregate.Candidates[0].GroundingMetadata)
	requireCitation(t, citations, 0, "https://a.example/p", "", "about a")
	requireCitation(t, citations, 1, "https://b.example/p", "", "about b")
}

func TestGoogleStreamReportsItsError(t *testing.T) {
	failing := func(yield func(*genai.GenerateContentResponse, error) bool) {
		yield(nil, errors.New("upstream closed"))
	}
	stream := googleStream(iter.Seq2[*genai.GenerateContentResponse, error](failing), domain.ModelRequest{})
	if _, err := stream.Receive(context.Background()); err == nil || !strings.Contains(err.Error(), "upstream closed") {
		t.Fatalf("a stream failure must surface, got %v", err)
	}
	if _, err := stream.Receive(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("a failed stream ends, got %v", err)
	}
}
